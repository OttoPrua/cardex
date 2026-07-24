package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// claude CLI 与桌面端共用 ~/.claude/projects 存储会话（每个会话一个 <id>.jsonl），
// 所以这里列出的会话不论来自哪一端，都可以被 --resume / adopt / brief -auto 接管。

func claudeProjectsRoot() string {
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".claude")
	}
	return filepath.Join(base, "projects")
}

var nonAlnumRe = regexp.MustCompile(`[^a-zA-Z0-9]`)

// encodeProjectDir 按 claude 的规则把工作目录编码为存储目录名（非字母数字一律替换为 -）。
func encodeProjectDir(dir string) string { return nonAlnumRe.ReplaceAllString(dir, "-") }

type sessionInfo struct {
	ID       string
	Modified time.Time
	Title    string
}

func listSessions(dir string) ([]sessionInfo, string, error) {
	pd := filepath.Join(claudeProjectsRoot(), encodeProjectDir(dir))
	entries, err := os.ReadDir(pd)
	if err != nil {
		return nil, pd, fmt.Errorf("该目录还没有 claude 会话记录（%s）", pd)
	}
	var out []sessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, sessionInfo{
			ID:       strings.TrimSuffix(e.Name(), ".jsonl"),
			Modified: fi.ModTime(),
			Title:    sessionTitle(filepath.Join(pd, e.Name())),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, pd, nil
}

// sessionTitle 取会话里第一条用户文本消息当标题（跳过工具结果等非文本行）。
func sessionTitle(path string) string {
	// 【CG-R3b R1 类闭合】path 来自 ~/.claude/projects/<proj>/ 的目录枚举(claude CLI 的地盘,
	// 属域外):listSessions 只滤了 IsDir + .jsonl 后缀,而 DirEntry.IsDir() 对 symlink 恒为 false
	// ——一条 <id>.jsonl → FIFO 的链接就能让 os.Open 永久阻塞,`claudego sessions` 整条命令挂死。
	f, _, err := openRegularFileNoBlock(path)
	if err != nil {
		return "-"
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	for i := 0; i < 200; i++ {
		line, err := r.ReadString('\n')
		if line != "" && strings.Contains(line, `"type":"user"`) {
			var row struct {
				Type    string `json:"type"`
				Message struct {
					Content any `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(line), &row) == nil && row.Type == "user" {
				if s, ok := row.Message.Content.(string); ok && strings.TrimSpace(s) != "" {
					return truncate(strings.Join(strings.Fields(s), " "), 44)
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
	}
	return "-"
}

// cmdSessions 列出某项目目录最近的 claude 会话（桌面端与 CLI 同池），方便 brief/adopt 接管。
func cmdSessions(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	dir := fs.String("dir", "", "项目目录（默认当前目录）")
	n := fs.Int("n", 10, "最多显示条数")
	_ = fs.Parse(args)
	wd, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	sessions, pd, err := listSessions(wd)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return fmt.Errorf("目录 %s 下没有会话文件", pd)
	}
	if len(sessions) > *n {
		sessions = sessions[:*n]
	}
	fmt.Printf("%s 最近的 claude 会话（桌面端与 CLI 共用）：\n", wd)
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "会话ID\t最后活动\t首条消息")
	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.ID, s.Modified.Format("01-02 15:04"), s.Title)
	}
	w.Flush()
	fmt.Println("\n回收进度: claudego brief -session <会话ID> -dir", wd, "-auto")
	fmt.Println("接管续跑: claudego adopt <会话ID> -dir", wd)
	return nil
}
