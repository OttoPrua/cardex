package main

// boardarchive.go — 项目级「手动归档」状态。
//
// 【只读纪律的边界在哪】board.go 开篇的第一条纪律是"只读"，指的是**队列数据**：
// tasks/ / archive/ / events/ / 任务 JSON 一律不写，看板挂在生产队列上，误写会污染真实队列。
// 本文件写的是看板自己的视图状态，落在独立文件 <root>/board_archive.json：
//   - 它不参与调度，runner / tick / patrol 一概不读；
//   - 删掉它只会让所有项目回到"未归档"，不丢任何队列数据；
//   - 任务卡本身一个字节都不动——归档一个项目不改变任何卡的状态。
// 这条边界必须守住：一旦有人图省事把归档标记写进任务卡，看板就从视图变成了队列的第二个写入方。
//
// 【自动复活的语义】委托人的要求是"如果有新卡则已归档任务可以自动切回到活跃状态，
// 卡片状态无变化保持原状态"。于是：
//   - **只有新增卡**才复活。卡从 queued 跑到 done、从 running 掉到 failed 都不复活——
//     手动归档表达的是"这个项目我暂时不看了"，已知卡跑完它并不构成"有新东西要看"。
//   - 复活是**只读推导**，不回写文件。归档记录原样留着，下次请求照样重算。
//     这样读路径保持零写入（HTTP GET 不产生副作用），也不存在"复活写盘失败 → 状态半档"的坑。
//   - 判据是归档时刻的 (卡数, 最新 created_at) 快照：卡数变多、或出现比快照更新的 created_at，
//     即判定有新卡。两条判据是 OR：只看卡数会被"删一张加一张"骗过，
//     只看 created_at 会被"created_at 缺失/损坏的卡"漏过。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// boardArchivePath 是归档状态文件。刻意与 archive/ 目录（任务归档）区分命名：
// 那个是队列的卡归档，这个是看板的项目折叠状态，两者毫无关系。
func boardArchivePath(root string) string { return filepath.Join(root, "board_archive.json") }

// boardArchiveRec 是一个项目的归档记录。TaskCount/MaxCreatedAt 是归档那一刻的卡面快照，
// 唯一用途是判"后来有没有新卡"。
type boardArchiveRec struct {
	ArchivedAt   string `json:"archived_at"`
	TaskCount    int    `json:"task_count"`
	MaxCreatedAt string `json:"max_created_at"`
}

// boardArchiveFile 是 board_archive.json 的整体结构。带 version 是为了将来加字段时
// 能识别老文件——不带版本号的状态文件在格式演进时只能靠猜。
type boardArchiveFile struct {
	Version  int                        `json:"version"`
	Projects map[string]boardArchiveRec `json:"projects"`
}

const boardArchiveVersion = 1

// loadBoardArchive 读归档状态。
//
// 文件不存在 = 没有任何项目被归档，是完全正常的状态，返回空结构 + nil。
// 文件损坏时返回 (空结构, err)：调用方**必须**把 err 披露出去而不是当成"没归档"——
// 否则用户手动归档的十个项目会在一次 JSON 损坏后集体"复活"，且界面上零提示。
func loadBoardArchive(root string) (*boardArchiveFile, error) {
	empty := &boardArchiveFile{Version: boardArchiveVersion, Projects: map[string]boardArchiveRec{}}
	data, err := os.ReadFile(boardArchivePath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return empty, nil
		}
		return empty, err
	}
	var f boardArchiveFile
	if err := json.Unmarshal(data, &f); err != nil {
		return empty, err
	}
	if f.Projects == nil {
		f.Projects = map[string]boardArchiveRec{}
	}
	if f.Version == 0 {
		f.Version = boardArchiveVersion
	}
	return &f, nil
}

func saveBoardArchive(root string, f *boardArchiveFile) error {
	if f.Projects == nil {
		f.Projects = map[string]boardArchiveRec{}
	}
	f.Version = boardArchiveVersion
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(boardArchivePath(root), append(data, '\n'))
}

// projectCardMark 取一批卡的 (张数, 最新 created_at)，作为归档时刻的卡面快照。
func projectCardMark(ts []*Task) (count int, maxCreated string) {
	count = len(ts)
	for _, t := range ts {
		if t.CreatedAt == "" {
			continue
		}
		if maxCreated == "" || createdAfter(t.CreatedAt, maxCreated) {
			maxCreated = t.CreatedAt
		}
	}
	return count, maxCreated
}

// createdAfter 判断 a 是否严格晚于 b。
//
// created_at 由 time.Now().Format(time.RFC3339) 写入，**带本地时区偏移**。
// 直接字符串比大小在同一台机器同一时区下没问题，但跨夏令时切换、跨机器（远端 Windows 卡
// 的偏移与本机不同）就会得出错误结论。故优先解析成时刻比较。
//
// 两边任一解析失败（老卡/损坏卡的 created_at 可能是空串或别的格式）才退回字典序比较。
// 这条退化路径只在**同格式**串上等价于时序；格式混杂时它给出的是字典序而非真实先后，
// 可能误判成"更新"从而多复活一次。这个方向是刻意选的：多复活一次的代价是用户重点一次归档，
// 漏复活的代价是新卡在总览上彻底看不见——后者才是不可接受的那种错。
func createdAfter(a, b string) bool {
	ta, okA := parseRFC3339(a)
	tb, okB := parseRFC3339(b)
	if okA && okB {
		return ta.After(tb)
	}
	return a > b
}

// archiveView 是一个项目当前的归档呈现状态（只读推导结果）。
type archiveView struct {
	// Archived 是最终生效的状态：有记录且未被新卡复活时为 true。
	Archived bool
	// ArchivedAt 是人工归档时刻（有记录时非空，无论是否已复活）。
	ArchivedAt string
	// Revived 表示"有归档记录，但检测到新卡，已自动切回活跃"。
	Revived bool
	// Reason 是复活原因的人话说明，供前端原样显示——只说"自动恢复了"而不说为什么，
	// 用户会怀疑是自己没点上归档。
	Reason string
}

// archiveViewFor 按归档记录与当前卡面推导展示状态。rec 为 nil = 从未归档。
func archiveViewFor(rec *boardArchiveRec, count int, maxCreated string) archiveView {
	if rec == nil {
		return archiveView{}
	}
	v := archiveView{Archived: true, ArchivedAt: rec.ArchivedAt}
	switch {
	case count > rec.TaskCount:
		v.Archived, v.Revived = false, true
		v.Reason = "归档后新增了 " + strconv.Itoa(count-rec.TaskCount) + " 张卡，已自动切回活跃"
	case maxCreated != "" && createdAfter(maxCreated, rec.MaxCreatedAt):
		// 卡数没变但出现了更新的 created_at：删一张 + 加一张会走到这里。
		v.Archived, v.Revived = false, true
		v.Reason = "归档后出现了更新的任务卡，已自动切回活跃"
	}
	return v
}

// boardArchiveStore 给归档状态的读写加锁。看板可能同时有多个标签页在点归档按钮，
// 无锁的 read-modify-write 会丢更新（两个页面同时归档两个项目，后写的把先写的抹掉）。
type boardArchiveStore struct {
	mu sync.Mutex
}

// set 归档 / 取消归档一个项目，返回写入后的记录（取消时为 nil）。
// mark 是当前卡面快照，归档时一并存下作为"有没有新卡"的基线。
func (s *boardArchiveStore) set(root, projectID string, archived bool,
	count int, maxCreated string, now time.Time) (*boardArchiveRec, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := loadBoardArchive(root)
	if err != nil {
		// 损坏文件上继续写 = 把用户已有的归档状态整块吞掉。宁可失败并把原因抛给前端。
		return nil, err
	}
	if !archived {
		delete(f.Projects, projectID)
		return nil, saveBoardArchive(root, f)
	}
	rec := boardArchiveRec{
		ArchivedAt:   now.Format(time.RFC3339),
		TaskCount:    count,
		MaxCreatedAt: maxCreated,
	}
	f.Projects[projectID] = rec
	if err := saveBoardArchive(root, f); err != nil {
		return nil, err
	}
	return &rec, nil
}
