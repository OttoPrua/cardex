package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ProgressEntry 是一次进度回收的落盘格式：固定信封 + 会话给出的报告原文。
// 报告字段约定（progress-brief/progress-dump 模板）：goal / done / in_progress /
// remaining / blockers / key_files / next_prompt，但这里不做强校验，原样保存。
type ProgressEntry struct {
	Key       string         `json:"key"`
	Title     string         `json:"title,omitempty"`
	Dir       string         `json:"dir,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	UpdatedAt string         `json:"updated_at"`
	Report    map[string]any `json:"report"`
}

func progressPath(root, key string) string {
	return filepath.Join(progressDir(root), key+".json")
}

func saveProgress(root string, e *ProgressEntry) error {
	if e.Key == "" {
		return fmt.Errorf("进度报告缺少 key")
	}
	if e.UpdatedAt == "" {
		e.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	if err := os.MkdirAll(progressDir(root), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(progressPath(root, e.Key), append(data, '\n'))
}

// parseReportLoose 尽力把输出解析为进度报告：优先 json 块，其次整体 JSON；
// 都不行则以原文兜底（被 resume 的老会话常按既有风格输出散文，丢弃太可惜——
// 读报告的是协调任务，散文一样能读）。
func parseReportLoose(result string) map[string]any {
	raw := lastFencedJSON(result)
	if raw == "" {
		raw = strings.TrimSpace(result)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(raw), &report); err == nil {
		return report
	}
	text := strings.TrimSpace(result)
	if r := []rune(text); len(r) > 6000 {
		text = string(r[:6000]) + "…（截断）"
	}
	return map[string]any{"format": "raw", "raw": text}
}

// saveProgressFromResult 从任务最终输出中提取进度报告并落盘（EmitProgress 任务用）。
func saveProgressFromResult(root string, t *Task, result string) (string, error) {
	if strings.TrimSpace(result) == "" {
		return "", fmt.Errorf("输出为空，无法生成进度报告")
	}
	report := parseReportLoose(result)
	key := t.ProgressKey
	if key == "" {
		key = t.ID
	}
	e := &ProgressEntry{Key: key, Title: t.Title, Dir: t.Dir, SessionID: t.SessionID, Report: report}
	if err := saveProgress(root, e); err != nil {
		return "", err
	}
	return key, nil
}

func loadProgressEntries(root string) []*ProgressEntry {
	entries, err := os.ReadDir(progressDir(root))
	if err != nil {
		return nil
	}
	var out []*ProgressEntry
	for _, f := range entries {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(progressDir(root), f.Name()))
		if err != nil {
			continue
		}
		var e ProgressEntry
		if json.Unmarshal(data, &e) != nil || e.Key == "" {
			continue
		}
		out = append(out, &e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt < out[j].UpdatedAt })
	return out
}

// ---- 协调任务的运行时上下文注入 ----

// injectLiveContext 在派发时把 {{QUEUE}} / {{PROGRESS}} 替换为实时快照。
// 协调任务入队和真正运行之间可能隔很久，所以这两块必须运行时取。
func injectLiveContext(root string, selfID, prompt string) string {
	if strings.Contains(prompt, "{{QUEUE}}") {
		prompt = strings.ReplaceAll(prompt, "{{QUEUE}}", queueSnapshot(root, selfID))
	}
	if strings.Contains(prompt, "{{PROGRESS}}") {
		prompt = strings.ReplaceAll(prompt, "{{PROGRESS}}", progressSnapshot(root))
	}
	return prompt
}

// queueSnapshot 输出未结束任务的紧凑 JSON 快照（排除协调任务自身）。
func queueSnapshot(root, selfID string) string {
	tasks, err := loadTasks(root)
	if err != nil {
		return "（读取队列失败）"
	}
	var rows []map[string]any
	for _, t := range tasks {
		if t.terminal() || t.ID == selfID {
			continue
		}
		row := map[string]any{
			"id": t.ID, "title": t.Title, "type": t.Type, "status": t.Status,
			"priority": t.Priority, "step": fmt.Sprintf("%d/%d", t.Step, len(t.Prompts)), "dir": t.Dir,
		}
		if t.Model != "" {
			row["model"] = t.Model
		}
		if t.SessionID != "" {
			row["session_id"] = t.SessionID
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return "（队列为空）"
	}
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return "（序列化队列失败）"
	}
	return string(data)
}

func progressSnapshot(root string) string {
	entries := loadProgressEntries(root)
	if len(entries) == 0 {
		return "（暂无进度报告。可先用 cardex brief 回收各会话进度再运行协调。）"
	}
	var sb strings.Builder
	for _, e := range entries {
		data, err := json.MarshalIndent(e, "", "  ")
		if err != nil {
			continue
		}
		sb.Write(data)
		sb.WriteString("\n")
	}
	return sb.String()
}

// ---- 键名生成 ----

var slugRe = regexp.MustCompile(`[^a-z0-9\p{Han}]+`)

// progressSlug 从标题/目录生成人类可读的进度键。
func progressSlug(hint string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(hint)), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "session"
	}
	r := []rune(s)
	if len(r) > 24 {
		s = string(r[:24])
	}
	return s + time.Now().Format("-0102-1504")
}
