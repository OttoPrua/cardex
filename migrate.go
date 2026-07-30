package main

// migrate.go —— `cardex migrate`：把数据根从改名前的旧根搬到 ~/.cardex（BD-44）。
//
// 【这条命令的性质】它动的是**生产队列的全部家当**：tasks/events/progress/archive 以及
// config.json。搬错一次没有"下一轮修一修"的机会——卡丢了就是丢了。所以整条实现按 fail-closed
// 写：任何一条前置不满足就**什么都不做**地退出，宁可让人多跑一次，也不做"搬了一半"的状态。
//
// 【为什么不自动建 symlink / 不自动卸 launchd / 不自动装二进制】那三件都是 cutover 操作员
// 在窗口里按现场情况决定的动作（BD-44）。代码替人做了，出事时人既不知道发生过什么、也没法
// 按自己的节奏回退。本命令只搬数据，然后把后续人工步骤原样打印出来。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// tallyDirs 是零丢失对账覆盖的目录。选这四个是因为它们装的是**不可再生**的东西：
// 队列卡、事件账本、进度报告、归档。logs/ 与 tmp/ 不进对账——日志可再生，tmp 下是副本残留，
// 把它们算进去只会让"字节数不一致"变成常态噪音，真丢卡时反而没人信这个数。
var tallyDirs = []string{"tasks", "events", "progress", "archive"}

// binName 是本工具的可执行名，只用于给人看的提示串。
// 【为什么不用 filepath.Base(dst)】数据根叫 .cardex（带点的隐藏目录），二进制叫 cardex，
// 拿目录名当命令名会印出 `.cardex install-launchd` 这种敲下去必然 not found 的东西。
const binName = "cardex"

// dirTally 是一个目录的对账快照：非目录条目数 + 字节合计。
//
// 【为什么按 Lstat 而不是 Stat】符号链接要按"链接本体"算。跟随的话，一条指向大文件的链接
// 会把字节数算成目标大小，搬完链接还在、目标没搬，对账反而看着"一致"。
type dirTally struct {
	Files int
	Bytes int64
}

func (t dirTally) String() string { return fmt.Sprintf("%d 个文件 / %d 字节", t.Files, t.Bytes) }

// tallyRoot 逐个目录点数。目录不存在按空计（全新根、或从没归过档的根都正常）。
func tallyRoot(root string) (map[string]dirTally, error) {
	out := map[string]dirTally{}
	for _, name := range tallyDirs {
		dir := filepath.Join(root, name)
		var t dirTally
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			fi, err := os.Lstat(p)
			if err != nil {
				return err
			}
			t.Files++
			t.Bytes += fi.Size()
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("点数 %s: %w", dir, err)
		}
		out[name] = t
	}
	return out, nil
}

func tallyEqual(a, b map[string]dirTally) (string, bool) {
	for _, name := range tallyDirs {
		if a[name] != b[name] {
			return fmt.Sprintf("%s/: 迁移前 %s，迁移后 %s", name, a[name], b[name]), false
		}
	}
	return "", true
}

// isEmptyDir 判断 path 是否是个空目录。
func isEmptyDir(path string) (bool, error) {
	ents, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(ents) == 0, nil
}

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	rootFlag := fs.String("root", "", "源数据根（默认按解析顺序取当前生效的根）")
	toFlag := fs.String("to", "", "目标数据根（默认 ~/.cardex）")
	dryRun := fs.Bool("dry-run", false, "只跑前置检查与对账预演，一个字节都不动")
	_ = fs.Parse(args)

	src := resolveRoot(*rootFlag)
	dst := *toFlag
	if dst == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("取不到 $HOME，无法推导目标根（可用 -to 显式指定）: %w", err)
		}
		dst = filepath.Join(home, rootDirName)
	}
	if abs, err := filepath.Abs(dst); err == nil {
		dst = abs
	}
	return runMigrate(src, dst, *dryRun)
}

// runMigrate 是 migrate 的可测主体（cmdMigrate 只负责解析参数）。
func runMigrate(src, dst string, dryRun bool) error {
	// ---- 前置 ①：源根必须存在且是目录 ----
	if !isExistingDir(src) {
		return fmt.Errorf("源数据根不存在或不是目录: %s", src)
	}
	if src == dst {
		return fmt.Errorf("源根与目标根是同一个路径（%s）——已经在新根上了，无需迁移", src)
	}

	// ---- 前置 ②：目标必须不存在，或者是个空目录 ----
	// 【为什么空目录也放行】`cardex init` 之类的动作会先把目录建出来。让一个空壳挡住迁移，
	// 人只能手工 rmdir 再来一次，纯粹的摩擦。有内容就是另一回事：那里面可能是另一份真队列。
	if fi, err := os.Lstat(dst); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("目标 %s 已存在且不是目录，拒绝迁移", dst)
		}
		empty, err := isEmptyDir(dst)
		if err != nil {
			return fmt.Errorf("检查目标目录: %w", err)
		}
		if !empty {
			return fmt.Errorf("目标 %s 已存在且非空，拒绝迁移（若确认它是废弃副本，请人工移走后重跑）", dst)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查目标 %s: %w", dst, err)
	}

	// ---- 前置 ③：不能有 running 任务 ----
	// 【为什么这条是硬门】running 卡背后是活着的执行器进程，它的 cwd/日志句柄/会话都钉在旧根的
	// 绝对路径上。目录一搬，那个进程会往一个已经不存在的路径写——写不进去还算好的，最坏是
	// 它按老路径重建出目录，于是队列一分为二：一半在新根、一半在旧根，谁也不知道哪份是真的。
	// tasksDir 不存在时 loadTasks 报"请先运行 init"——对迁移来说那不是错误，是"这个根还没派过卡"。
	var tasks []*Task
	if isExistingDir(tasksDir(src)) {
		loaded, err := loadTasks(src)
		if err != nil {
			return fmt.Errorf("读源根任务: %w", err)
		}
		tasks = loaded
	}
	var running []string
	for _, t := range tasks {
		if t.Status == statusRunning {
			running = append(running, t.ID)
		}
	}
	if len(running) > 0 {
		sort.Strings(running)
		return fmt.Errorf("源根有 %d 个 running 任务（%s），拒绝迁移——等它们跑完，或先 cardex hold/cancel",
			len(running), strings.Join(running, ", "))
	}

	// ---- 前置 ④：拿到实例锁 ----
	// 拿不到 = 有 tick/daemon 正在这个根上干活。它随时会写卡、写事件，搬到一半就是撕裂。
	// 【为什么读不出 config 也照样往下走】迁移要救的恰恰可能是一个 config 损坏/缺失的根。
	// 拿不到配置就用一个保守的锁 TTL，不让"读不出配置"变成迁移的拦路虎。
	cfg, cfgErr := loadConfig(src)
	ttl := 3 * time.Hour
	if cfgErr == nil {
		ttl = lockTTL(cfg)
	}
	if !acquireLock(src, ttl) {
		return fmt.Errorf("拿不到 %s 的实例锁——有 tick/daemon 正在跑，拒绝迁移（等本轮排空或先卸 launchd）", src)
	}
	lockedRoot := src
	defer func() { releaseLock(lockedRoot) }()

	// ---- 迁移前对账快照 ----
	before, err := tallyRoot(src)
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Printf("【dry-run】前置检查全部通过，未改动任何东西。\n源根: %s\n目标: %s\n", src, dst)
		printTally("迁移前对账", before)
		if cfgErr == nil {
			printConfigRewritePreview(src, dst)
		}
		printPostSteps(src, dst)
		return nil
	}

	// ---- 搬 ----
	movedByRename, err := moveRoot(src, dst)
	if err != nil {
		return err
	}
	lockedRoot = dst // 锁文件随目录一起搬走了，release 要对新路径来

	// ---- 迁移后对账：不符即回滚 ----
	after, err := tallyRoot(dst)
	if err != nil {
		rollback(src, dst, movedByRename)
		return fmt.Errorf("迁移后点数失败，已回滚: %w", err)
	}
	if diff, ok := tallyEqual(before, after); !ok {
		rollback(src, dst, movedByRename)
		lockedRoot = src
		return fmt.Errorf("零丢失对账不通过，已回滚到 %s —— %s", src, diff)
	}

	// ---- 改写 config.json 里指向旧根的路径 ----
	rewritten, err := rewriteConfigPaths(filepath.Join(dst, "config.json"), src, dst)
	if err != nil {
		// 【为什么这里不回滚】数据已经完整搬到新根且对账通过——回滚等于把好好的数据再搬一次，
		// 徒增一次风险窗口。config 路径改写失败是**可以手工修**的（就是几处字符串），
		// 所以如实报错、把要改的东西说清楚，让人在新根上补完。
		return fmt.Errorf("数据已迁移到 %s 且对账通过，但改写 config.json 路径失败: %w\n"+
			"请手工把 config.json 里 %s 开头的路径改成 %s 开头", dst, err, src, dst)
	}

	fmt.Printf("迁移完成: %s → %s\n", src, dst)
	printTally("零丢失对账（前后一致）", after)
	if rewritten > 0 {
		fmt.Printf("config.json: 改写了 %d 处指向旧根的路径。\n", rewritten)
	} else {
		fmt.Println("config.json: 没有指向旧根的路径，未改动。")
	}
	printPostSteps(src, dst)
	return nil
}

func printTally(title string, t map[string]dirTally) {
	fmt.Printf("%s：\n", title)
	for _, name := range tallyDirs {
		fmt.Printf("  %-9s %s\n", name+"/", t[name])
	}
}

// moveRoot 把 src 整个搬到 dst。返回 true 表示走的是 os.Rename（回滚只需 rename 回去）。
func moveRoot(src, dst string) (byRename bool, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, fmt.Errorf("建目标父目录: %w", err)
	}
	// 目标是空目录时先删掉：POSIX rename 允许覆盖空目录，但各平台边角不一，显式删更可预期。
	if fi, err := os.Lstat(dst); err == nil && fi.IsDir() {
		if err := os.Remove(dst); err != nil {
			return false, fmt.Errorf("清掉目标空目录 %s: %w", dst, err)
		}
	}
	if err := os.Rename(src, dst); err == nil {
		return true, nil
	} else if !isCrossDeviceErr(err) {
		return false, fmt.Errorf("整目录 rename 失败: %w", err)
	}
	// ---- 跨卷回退：copy + 逐文件校验 ----
	// 【为什么必须逐文件校验而不是拷完拉倒】跨卷拷贝是逐字节重写，中途 ENOSPC / IO 错误可能
	// 只毁一个文件。整目录 rename 是原子的、没有这个失败模式，copy 路径必须自己把它补上：
	// 每个文件比 sha256，全过才敢删源。
	if err := copyTree(src, dst); err != nil {
		_ = os.RemoveAll(dst)
		return false, fmt.Errorf("跨卷拷贝失败（已清掉半成品目标，源根未动）: %w", err)
	}
	if err := verifyTree(src, dst); err != nil {
		_ = os.RemoveAll(dst)
		return false, fmt.Errorf("跨卷拷贝逐文件校验失败（已清掉目标，源根未动）: %w", err)
	}
	if err := os.RemoveAll(src); err != nil {
		return false, fmt.Errorf("拷贝与校验均通过，但删除源根 %s 失败（数据已在 %s，请人工确认后删源）: %w", src, dst, err)
	}
	return false, nil
}

func rollback(src, dst string, byRename bool) {
	if byRename {
		if err := os.Rename(dst, src); err != nil {
			fmt.Fprintf(os.Stderr, "严重: 回滚失败，数据仍在 %s（原路径 %s）: %v\n", dst, src, err)
		}
		return
	}
	// 跨卷路径下源根已删，回滚只能反向拷。走到这里说明"拷贝校验都过了但对账不符"，
	// 属于极罕见的自相矛盾，如实报出来让人接手，不做更多自动动作。
	fmt.Fprintf(os.Stderr, "警告: 跨卷迁移后对账不符，源根已删除，无法自动回滚。数据在 %s，请人工核对。\n", dst)
}

// isCrossDeviceErr 判断 rename 是否因跨文件系统失败（EXDEV）。
// 不按 errno 常量判是为了不给 Windows 构建加平台分支——EXDEV 在各平台的错误串里都带
// "cross-device"/"different disk drive" 字样，链路上再叠一层 LinkError 也能匹到。
func isCrossDeviceErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "cross-device") ||
		strings.Contains(s, "cross device") ||
		strings.Contains(s, "different disk drive") ||
		strings.Contains(s, "exdev")
}

// copyTree 递归拷贝，保留权限位与符号链接本体（不跟随——跟随会把链接变内容，也可能撞上
// 无读端 FIFO 之类的非常规文件永久阻塞）。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			fi, err := os.Stat(p)
			if err != nil {
				return err
			}
			return os.MkdirAll(target, fi.Mode().Perm())
		case d.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			return copyRegular(p, target)
		default:
			// FIFO/socket/设备：数据根里不该有，跳过并留痕，不让它把整条迁移带崩。
			fmt.Fprintf(os.Stderr, "提示: 跳过非常规文件 %s（%s）\n", p, d.Type())
			return nil
		}
	})
}

func copyRegular(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// verifyTree 逐文件比对 src 与 dst：普通文件比 sha256，符号链接比目标串，条目集合必须一一对应。
func verifyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			if !isExistingDir(target) {
				return fmt.Errorf("目标缺目录 %s", target)
			}
			return nil
		case d.Type()&os.ModeSymlink != 0:
			a, err := os.Readlink(p)
			if err != nil {
				return err
			}
			b, err := os.Readlink(target)
			if err != nil {
				return fmt.Errorf("目标缺链接 %s: %w", target, err)
			}
			if a != b {
				return fmt.Errorf("链接目标不一致 %s: %q vs %q", rel, a, b)
			}
			return nil
		case d.Type().IsRegular():
			a, err := fileSHA256(p)
			if err != nil {
				return err
			}
			b, err := fileSHA256(target)
			if err != nil {
				return fmt.Errorf("目标缺文件 %s: %w", target, err)
			}
			if a != b {
				return fmt.Errorf("内容校验不一致 %s（sha256 %s vs %s）", rel, a, b)
			}
			return nil
		}
		return nil
	})
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- config.json 路径改写 ----

// pathRewrites 给出要替换的前缀对。除了绝对路径，还带上 ~ 形式：实测生产 config 里
// permissions 白名单同时存在 `/Users/x/.claudego/verify-...sh:*)` 与 `~/.claudego/verify-...sh:*)`
// 两种写法，只换绝对路径会漏掉一半，而漏掉的那一半表现是"复审偶发权限拒绝"，极难往改名上想。
func pathRewrites(src, dst string) [][2]string {
	out := [][2]string{{src + string(os.PathSeparator), dst + string(os.PathSeparator)}}
	if home, err := os.UserHomeDir(); err == nil {
		if relSrc, err := filepath.Rel(home, src); err == nil && !strings.HasPrefix(relSrc, "..") {
			if relDst, err := filepath.Rel(home, dst); err == nil && !strings.HasPrefix(relDst, "..") {
				out = append(out, [2]string{
					"~/" + filepath.ToSlash(relSrc) + "/",
					"~/" + filepath.ToSlash(relDst) + "/",
				})
			}
		}
	}
	return out
}

// rewriteConfigPaths 把 config.json 里**所有字符串值**中指向旧根的路径前缀换成新根，返回改写处数。
//
// 【为什么不走 Config 结构体】saveConfig 那条路会把结构体里没有的字段整个丢掉——生产 config
// 里躺着多少手写的、当前版本还不认识的键，谁也说不准。解成 map[string]any 递归走，
// 认不认识都原样带过去。
//
// 【为什么必须 UseNumber】默认解码把 JSON 数字一律变成 float64。超过 2^53 的整数（纳秒
// 时间戳这类）经这一道中转就丢精度，写回盘上是**另一个数字**——不报错、不崩溃，只是账不对了。
// 一次只跑一遍的迁移命令最不该留这种静默损坏。UseNumber 让数字字面量原样保留。
func rewriteConfigPaths(path, src, dst string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // 没有 config.json 的根是合法的（还没 init 过）
		}
		return 0, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return 0, fmt.Errorf("解析 %s: %w", path, err)
	}
	rules := pathRewrites(src, dst)
	n := 0
	doc = rewriteStrings(doc, rules, &n)
	if n == 0 {
		return 0, nil
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return 0, err
	}
	return n, atomicWrite(path, append(out, '\n'))
}

// rewriteStrings 递归替换任意 JSON 值里的字符串前缀，返回替换后的值；n 累计替换处数。
func rewriteStrings(v any, rules [][2]string, n *int) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = rewriteStrings(val, rules, n)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = rewriteStrings(val, rules, n)
		}
		return t
	case string:
		out := t
		for _, r := range rules {
			if strings.Contains(out, r[0]) {
				*n += strings.Count(out, r[0])
				out = strings.ReplaceAll(out, r[0], r[1])
			}
		}
		return out
	default:
		return v
	}
}

func printConfigRewritePreview(src, dst string) {
	n := 0
	raw, err := os.ReadFile(filepath.Join(src, "config.json"))
	if err != nil {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if dec.Decode(&doc) != nil {
		return
	}
	rewriteStrings(doc, pathRewrites(src, dst), &n)
	fmt.Printf("config.json: 将改写 %d 处指向旧根的路径。\n", n)
}

// printPostSteps 打印迁移之后仍需人工完成的动作。
//
// 【为什么这些不由本命令做】见文件头。这里只做一件代码能做好的事：把**现场实际存在**的
// 隐患查出来点名——尤其是数据根里那些硬编码了旧根路径的 shell 脚本：它们跟着目录搬到新根，
// 但内部还写着 $HOME/<旧根名>/...，跑起来就是"文件不存在"。这类断裂不查出来，
// 表现是复审同步偶发失败，没人会往改名上联想。
func printPostSteps(src, dst string) {
	fmt.Println("\n后续人工步骤（本命令**不**代做）：")
	fmt.Printf("  1. 装新二进制并铺兼容软链: make install install-shim（shim 仍是旧名 claudego → %s）\n", binName)
	fmt.Printf("  2. 重装定时器: %s install-launchd；旧的 %s 请手工卸掉\n", binName, legacyLaunchdLabel)
	fmt.Println("     launchctl unload ~/Library/LaunchAgents/" + legacyLaunchdLabel + ".plist")
	fmt.Printf("  3. 旧根 symlink（%s → %s）由你按现场决定要不要建，本命令不代建。\n", src, dst)
	fmt.Println("  4. 钥匙串/TCC 授权是按二进制路径给的，换名后 quota 的 oauth 用量读取需重新批准。")

	if hits := scanHardcodedOldRoot(dst, src); len(hits) > 0 {
		fmt.Printf("  5. 【必须处理】以下脚本已随目录搬到新根，但内部仍硬编码旧根路径 %s：\n", src)
		for _, h := range hits {
			fmt.Printf("       %s\n", h)
		}
		fmt.Println("     它们属跨机指纹/同步工件族，本轮按裁决冻结在旧名，故未自动改写；")
		fmt.Println("     不改则复审同步链会在'文件不存在'上静默失败。")
	}
}

// scanHardcodedOldRoot 在新根顶层的脚本里找仍指向旧根的字面量。只扫顶层、只扫脚本、
// 只读不改：这是一份给人看的清单，不是自动修复。
func scanHardcodedOldRoot(dst, src string) []string {
	ents, err := os.ReadDir(dst)
	if err != nil {
		return nil
	}
	needles := []string{src + "/"}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, src); err == nil && !strings.HasPrefix(rel, "..") {
			needles = append(needles, "$HOME/"+filepath.ToSlash(rel)+"/", "~/"+filepath.ToSlash(rel)+"/")
		}
	}
	var hits []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sh") && !strings.HasSuffix(name, ".ps1") {
			continue
		}
		data, err := readRegularFileNoBlock(filepath.Join(dst, name))
		if err != nil {
			continue
		}
		for _, nd := range needles {
			if bytes.Contains(data, []byte(nd)) {
				hits = append(hits, filepath.Join(dst, name)+"（含 "+nd+"）")
				break
			}
		}
	}
	sort.Strings(hits)
	return hits
}
