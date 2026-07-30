package main

// saferead.go —— CG-R3b R1:"阻塞式 open"统一闸门(按类闭合)。
//
// 【缺陷类定义】当一条路径来自 cardex 控制域**之外**——业务仓工作树枚举(git ls-files)、
// 模型输出里指名的文件、外部 CLI 自己写的 transcript 目录——它可能是 FIFO、设备节点,或指向
// 这两者的 symlink。对这类路径直接 os.Open / os.ReadFile 有两重致命性:
//   ① 打开**无写端的 FIFO** 按 POSIX 永久阻塞(实测 `timeout 2 cat link-to-pipe` → 124);
//      阻塞点在纯 Go syscall 里,exec.CommandContext 的 Cancel、killProcGroup、patrol 的
//      ctx cancel(patrol.go)一路都解不开 —— 调用它的 goroutine(runTask 泳道 / tick /
//      web handler)就此永久占死,只能整进程重启;
//   ② "先 Open 再判 IsRegular/IsDir" 的写法**防不住**:阻塞发生在判型之前,判型代码根本执行不到。
// 这正是 CG-R3b R1 必修清单 P1-1 的第二面(copyFile 先 os.Open 后判 IsRegular)。域外路径的
// 读入口统一走本文件,别再各自 os.Open。
//
// 【为什么是 O_NONBLOCK + fstat 两道,缺一不可】
//   ① O_NONBLOCK 让 FIFO/设备的 open 立即返回而非挂住 —— 这是唯一能防住"open 自身阻塞"的手段。
//      只在调用前 lstat 判型不够:判过型到 open 之间路径可被换成 FIFO(TOCTOU),窗口小但不为零。
//   ② 拿到 fd 后再 fstat 复核 —— 判的是"这个 fd 指向的东西",不存在 TOCTOU;非普通文件立刻关掉
//      并报 errNotRegularFile,让调用侧按"跳过"而不是"读到脏数据"处理。
// 对普通文件 O_NONBLOCK 是 no-op(POSIX 规定其只影响管道/终端/设备),故正常路径零行为变化。
//
// 【域内路径(<root> 下 cardex 自建的 events/tasks/progress/tombstones 等)为何不在本类】
// 那些文件的唯一写者是 cardex 自己,且一律 atomicWrite(tmp+rename)落盘,不可能是 FIFO;
// 要在那里出现管道只能是有人蓄意破坏 cardex 自有状态目录——那种威胁模型下 config.json 与任务
// JSON 每一处读都同形,得靠统一 read 门面一次性收(另卡)。本卡按"路径来源是否可信"划线,
// 见收工汇报的位点表。例外:副本目录 <root>/tmp/codex-review-work/**/ 的内容**部分来自业务仓
// untracked 面**,故它虽在 <root> 下仍属域外,marker 读取也走本文件(见 readCodexWorkMarker)。

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// errNotRegularFile 是"这条路径不是普通文件"的哨兵错。调用侧用 errors.Is 判它来区分
// "该跳过"(FIFO/socket/设备/目录)与"真 IO 故障"(权限/介质错)——前者静默跳过,后者该报。
var errNotRegularFile = errors.New("不是普通文件")

// openRegularFileNoBlock 打开 path 供读取,保证两件事:绝不在 open 上阻塞、返回的 fd 必是普通文件。
// 返回的 *os.File 由调用侧负责 Close。非普通文件返回的 err 满足 errors.Is(err, errNotRegularFile)。
func openRegularFileNoBlock(path string) (*os.File, os.FileInfo, error) {
	// syscall.O_NONBLOCK 在 darwin/linux/windows 三平台的 syscall 包里都有定义(已实测交叉编译);
	// Windows 的 syscall.Open 忽略它不认识的标志位,故此处平台安全,无需 build tag 分叉。
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s(mode=%s): %w", path, fi.Mode(), errNotRegularFile)
	}
	return f, fi, nil
}

// readRegularFileNoBlock 是 os.ReadFile 的域外安全版:非普通文件报 errNotRegularFile 而不是挂住。
func readRegularFileNoBlock(path string) ([]byte, error) {
	f, _, err := openRegularFileNoBlock(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
