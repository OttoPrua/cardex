package main

// retrofacts.go compiles the auditable input for a retrospective. The compiler deliberately
// stops at facts: model-written conclusions remain proposal-only and cannot overwrite these values.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const retroFactsSchema = "cardex.retro_facts.v1"
const retroReportSchema = "cardex.retrospective_report.v2"

type RetroFacts struct {
	SchemaVersion  string             `json:"schema_version"`
	Window         RetroFactsWindow   `json:"window"`
	Counts         RetroFactsCounts   `json:"counts"`
	FailureClasses map[string]int     `json:"failure_classes"`
	FixRounds      map[string]int     `json:"fix_rounds"`
	Cost           RetroFactsCost     `json:"cost"`
	ReviewVerdicts map[string]int     `json:"review_verdicts"`
	LimitEvents    RetroFactsLimits   `json:"limit_events"`
	Coverage       RetroFactsCoverage `json:"coverage"`
	Cards          []RetroCardFact    `json:"cards"`
	Gaps           []RetroFactGap     `json:"gaps"`
}

type RetroFactsWindow struct {
	RequestedCards int      `json:"requested_cards"`
	SelectedCards  int      `json:"selected_cards"`
	Watermark      int64    `json:"watermark,omitempty"`
	SelectionRule  string   `json:"selection_rule"`
	From           string   `json:"from"`
	To             string   `json:"to"`
	TaskIDs        []string `json:"task_ids"`
}

type RetroFactsCounts struct {
	ByType   map[string]int `json:"by_type"`
	ByModel  map[string]int `json:"by_model"`
	ByRunner map[string]int `json:"by_runner"`
	ByStakes map[string]int `json:"by_stakes"`
	ByEffort map[string]int `json:"by_effort"`
}

type RetroFactsCost struct {
	AvailableCards   int                `json:"available_cards"`
	UnavailableCards int                `json:"unavailable_cards"`
	TotalUSD         *float64           `json:"total_usd"`
	MedianUSD        *float64           `json:"median_usd"`
	MaxUSD           *float64           `json:"max_usd"`
	ByModel          map[string]float64 `json:"by_model"`
	ByRunner         map[string]float64 `json:"by_runner"`
}

type RetroFactsLimits struct {
	LimitPausedEvents     int `json:"limit_paused_events"`
	OverMaxFixRoundsCards int `json:"over_max_fix_rounds_cards"`
	DivertedToCodexCards  int `json:"diverted_to_codex_cards"`
}

type RetroFactsCoverage struct {
	SelectedCards      int `json:"selected_cards"`
	EventLedgerCards   int `json:"event_ledger_cards"`
	DoneEventCards     int `json:"done_event_cards"`
	CostCards          int `json:"cost_cards"`
	ReviewCards        int `json:"review_cards"`
	ReviewVerdictCards int `json:"review_verdict_cards"`
}

type RetroCardFact struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Project          string   `json:"project,omitempty"`
	Dir              string   `json:"dir"`
	CompletedAt      string   `json:"completed_at"`
	Type             string   `json:"type"`
	Model            string   `json:"model"`
	Runner           string   `json:"runner"`
	Stakes           string   `json:"stakes"`
	Effort           string   `json:"effort"`
	FixRound         int      `json:"fix_round"`
	MaxFixRounds     int      `json:"max_fix_rounds"`
	ReviewOf         string   `json:"review_of,omitempty"`
	EmittedBy        string   `json:"emitted_by,omitempty"`
	LastSummary      string   `json:"last_summary,omitempty"`
	CostUSD          *float64 `json:"cost_usd"`
	Turns            *int     `json:"turns"`
	CostSource       string   `json:"cost_source"`
	FailureClasses   []string `json:"failure_classes"`
	ReviewVerdict    string   `json:"review_verdict,omitempty"`
	LimitPaused      int      `json:"limit_paused_events"`
	OverMaxFixRounds bool     `json:"over_max_fix_rounds"`
}

type RetroFactGap struct {
	Code   string `json:"code"`
	TaskID string `json:"task_id,omitempty"`
	Detail string `json:"detail"`
}

type RetroFactsEnvelope struct {
	FactsSHA256 string      `json:"facts_sha256"`
	Facts       *RetroFacts `json:"facts"`
}

type retroFactCandidate struct {
	task        *Task
	events      []TaskEvent
	completedAt string
	completed   time.Time
	hasDone     bool
	gaps        []RetroFactGap
}

// buildRetroFacts selects runner-completed business cards. File modification time is never evidence
// of completion: clean/migration/touch operations may change it without running a task. An optional
// in-memory overlay covers the runner's emit-done -> retrospective -> final-save ordering.
func buildRetroFacts(root string, n int, watermark int64, overlays ...*Task) (*RetroFacts, error) {
	if n <= 0 {
		return nil, fmt.Errorf("复盘张数必须大于 0, got %d", n)
	}
	facts := &RetroFacts{
		SchemaVersion: retroFactsSchema,
		Window: RetroFactsWindow{
			RequestedCards: n,
			Watermark:      watermark,
			SelectionRule:  "latest status=done non-retrospective tasks by last done-event timestamp; task updated_at is an explicit fallback",
			TaskIDs:        []string{},
		},
		Counts: RetroFactsCounts{
			ByType: map[string]int{}, ByModel: map[string]int{}, ByRunner: map[string]int{}, ByStakes: map[string]int{}, ByEffort: map[string]int{},
		},
		FailureClasses: map[string]int{},
		FixRounds:      map[string]int{},
		Cost: RetroFactsCost{
			ByModel: map[string]float64{}, ByRunner: map[string]float64{},
		},
		ReviewVerdicts: map[string]int{},
		Cards:          []RetroCardFact{},
		Gaps:           []RetroFactGap{},
	}

	tasks, gaps := loadRetroFactTasks(root)
	tasks = overlayRetroFactTasks(tasks, overlays)
	facts.Gaps = append(facts.Gaps, gaps...)
	candidates := make([]retroFactCandidate, 0, len(tasks))
	for _, task := range tasks {
		if task.Status != statusDone || isRetroTask(task) {
			continue
		}
		var candidateGaps []RetroFactGap
		events, corrupt, err := loadTaskEvents(root, task.ID)
		if err != nil {
			candidateGaps = append(candidateGaps, RetroFactGap{Code: "event_read_failed", TaskID: task.ID, Detail: err.Error()})
			events = nil
		}
		if corrupt {
			candidateGaps = append(candidateGaps, RetroFactGap{Code: "event_corruption", TaskID: task.ID, Detail: "one or more malformed event lines were ignored"})
		}
		completedAt, completed, hasDone := retroCompletion(task, events)
		if !hasDone {
			candidateGaps = append(candidateGaps, RetroFactGap{Code: "done_event_missing", TaskID: task.ID, Detail: "selected by task status=done using updated_at fallback"})
		}
		candidates = append(candidates, retroFactCandidate{
			task: task, events: events, completedAt: completedAt, completed: completed, hasDone: hasDone, gaps: candidateGaps,
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].completed.Equal(candidates[j].completed) {
			return candidates[i].completed.After(candidates[j].completed)
		}
		if candidates[i].completedAt != candidates[j].completedAt {
			return candidates[i].completedAt > candidates[j].completedAt
		}
		return candidates[i].task.ID > candidates[j].task.ID
	})
	if len(candidates) > n {
		candidates = candidates[:n]
	}
	if len(candidates) < n {
		facts.Gaps = append(facts.Gaps, RetroFactGap{
			Code: "cohort_shortfall", Detail: fmt.Sprintf("requested %d done cards, found %d", n, len(candidates)),
		})
	}

	facts.Window.SelectedCards = len(candidates)
	facts.Coverage.SelectedCards = len(candidates)
	if len(candidates) > 0 {
		facts.Window.To = candidates[0].completedAt
		facts.Window.From = candidates[len(candidates)-1].completedAt
	}
	costs := make([]float64, 0, len(candidates))
	for _, candidate := range candidates {
		task, events := candidate.task, candidate.events
		facts.Gaps = append(facts.Gaps, candidate.gaps...)
		facts.Window.TaskIDs = append(facts.Window.TaskIDs, task.ID)
		if len(events) > 0 {
			facts.Coverage.EventLedgerCards++
		}
		if candidate.hasDone {
			facts.Coverage.DoneEventCards++
		}

		model := retroDimension(task.Model, "unspecified")
		runner := retroDimension(task.Runner, "claude")
		stakes := retroDimension(task.Stakes, "unspecified")
		effort := retroDimension(task.Effort, "unspecified")
		typ := retroDimension(task.Type, "unspecified")
		facts.Counts.ByType[typ]++
		facts.Counts.ByModel[model]++
		facts.Counts.ByRunner[runner]++
		facts.Counts.ByStakes[stakes]++
		facts.Counts.ByEffort[effort]++
		facts.FixRounds[strconv.Itoa(task.FixRound)]++

		failureClasses := retroFailureClasses(task, events)
		for _, class := range failureClasses {
			facts.FailureClasses[class]++
		}
		cost, turns, source, costGap := retroTaskCost(task, events)
		if costGap != "" {
			facts.Gaps = append(facts.Gaps, RetroFactGap{Code: "cost_unavailable", TaskID: task.ID, Detail: costGap})
		}
		if cost != nil {
			value := retroRound(*cost)
			cost = &value
			costs = append(costs, value)
			facts.Coverage.CostCards++
			facts.Cost.AvailableCards++
			facts.Cost.ByModel[model] = retroRound(facts.Cost.ByModel[model] + value)
			facts.Cost.ByRunner[runner] = retroRound(facts.Cost.ByRunner[runner] + value)
		} else {
			facts.Cost.UnavailableCards++
		}

		verdict := retroReviewVerdict(task, events)
		if task.Type == typeReview || task.XRole == "C" {
			facts.Coverage.ReviewCards++
			if verdict != "" {
				facts.Coverage.ReviewVerdictCards++
				facts.ReviewVerdicts[verdict]++
			} else {
				facts.Gaps = append(facts.Gaps, RetroFactGap{
					Code: "review_verdict_missing", TaskID: task.ID,
					Detail: "review card completed without a structured verdict in its event ledger",
				})
			}
		}
		limitPaused, overMax := retroLimitFacts(events)
		facts.LimitEvents.LimitPausedEvents += limitPaused
		if overMax {
			facts.LimitEvents.OverMaxFixRoundsCards++
		}
		if task.Runner == "codex" {
			facts.LimitEvents.DivertedToCodexCards++
		}
		facts.Cards = append(facts.Cards, RetroCardFact{
			ID: task.ID, Title: task.Title, Project: task.Project, Dir: task.Dir,
			CompletedAt: candidate.completedAt, Type: typ, Model: model, Runner: runner,
			Stakes: stakes, Effort: effort, FixRound: task.FixRound, MaxFixRounds: task.MaxFixRounds,
			ReviewOf: task.ReviewOf, EmittedBy: task.EmittedBy, LastSummary: retroTrim(task.LastSummary, 240),
			CostUSD: cost, Turns: turns, CostSource: source,
			FailureClasses: failureClasses, ReviewVerdict: verdict, LimitPaused: limitPaused, OverMaxFixRounds: overMax,
		})
	}
	retroFinishCosts(&facts.Cost, costs)
	return facts, nil
}

func overlayRetroFactTasks(tasks []*Task, overlays []*Task) []*Task {
	index := make(map[string]int, len(tasks))
	for i, task := range tasks {
		index[task.ID] = i
	}
	for _, overlay := range overlays {
		if overlay == nil || overlay.ID == "" {
			continue
		}
		copy := *overlay
		if i, ok := index[copy.ID]; ok {
			tasks[i] = &copy
			continue
		}
		index[copy.ID] = len(tasks)
		tasks = append(tasks, &copy)
	}
	return tasks
}

// cmdRetro is intentionally read-only. It gives operators and tests the same compiler used by
// automatic retrospective cards, without enqueuing a task or writing a progress report.
func cmdRetro(args []string) error {
	fs := flag.NewFlagSet("retro", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	rootFlag := fs.String("root", "", "Cardex 数据目录")
	n := fs.Int("n", 10, "最近完成卡数量")
	watermark := fs.Int64("watermark", 0, "可选的复盘触发水位")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("retro 不接受位置参数: %s", strings.Join(fs.Args(), " "))
	}
	return writeRetroFacts(os.Stdout, resolveRoot(*rootFlag), *n, *watermark)
}

func writeRetroFacts(w io.Writer, root string, n int, watermark int64) error {
	facts, err := buildRetroFacts(root, n, watermark)
	if err != nil {
		return err
	}
	_, hash, err := marshalRetroFacts(facts)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(RetroFactsEnvelope{FactsSHA256: hash, Facts: facts})
}

// validateRetroReport prevents a fluent but detached model response from becoming durable learning
// input. Legacy retrospective cards have no frozen metadata and remain readable/retryable.
func validateRetroReport(task *Task, report map[string]any) error {
	if task == nil || task.RetroFactsSHA256 == "" {
		return nil
	}
	if got := retroReportString(report, "schema_version"); got != retroReportSchema {
		return fmt.Errorf("schema_version=%q, want %q", got, retroReportSchema)
	}
	if got := retroReportString(report, "facts_sha256"); got != task.RetroFactsSHA256 {
		return fmt.Errorf("facts_sha256 与冻结事实不一致")
	}
	cohort, err := retroReportStringSlice(report, "cohort_task_ids")
	if err != nil {
		return err
	}
	if !retroEqualStrings(cohort, task.RetroCohortTaskIDs) {
		return fmt.Errorf("cohort_task_ids 与冻结 cohort 不一致")
	}
	allowed := make(map[string]bool, len(cohort))
	for _, id := range cohort {
		allowed[id] = true
	}
	conclusions, err := retroReportObjects(report, "conclusions")
	if err != nil {
		return err
	}
	for i, conclusion := range conclusions {
		if retroReportString(conclusion, "finding") == "" {
			return fmt.Errorf("conclusions[%d].finding 为空", i)
		}
		switch retroReportString(conclusion, "confidence") {
		case "high", "medium", "low":
		default:
			return fmt.Errorf("conclusions[%d].confidence 非法", i)
		}
		if err := retroValidateEvidence(conclusion, fmt.Sprintf("conclusions[%d]", i), allowed); err != nil {
			return err
		}
	}
	recommendations, err := retroReportObjects(report, "recommendations")
	if err != nil {
		return err
	}
	if len(recommendations) > 3 {
		return fmt.Errorf("recommendations=%d, 最多允许 3", len(recommendations))
	}
	for i, recommendation := range recommendations {
		for _, field := range []string{"target", "change", "expected_effect", "validation"} {
			if retroReportString(recommendation, field) == "" {
				return fmt.Errorf("recommendations[%d].%s 为空", i, field)
			}
		}
		if err := retroValidateEvidence(recommendation, fmt.Sprintf("recommendations[%d]", i), allowed); err != nil {
			return err
		}
	}
	if _, ok := report["deferred_edges"].([]any); !ok {
		return fmt.Errorf("deferred_edges 必须是数组")
	}
	return nil
}

func retroValidateEvidence(item map[string]any, label string, allowed map[string]bool) error {
	ids, err := retroReportStringSlice(item, "evidence_task_ids")
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if len(ids) == 0 {
		return fmt.Errorf("%s.evidence_task_ids 为空", label)
	}
	for _, id := range ids {
		if !allowed[id] {
			return fmt.Errorf("%s 引用了 cohort 外任务 %q", label, id)
		}
	}
	return nil
}

func retroReportString(report map[string]any, key string) string {
	value, _ := report[key].(string)
	return strings.TrimSpace(value)
}

func retroReportStringSlice(report map[string]any, key string) ([]string, error) {
	raw, ok := report[key].([]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是字符串数组", key)
	}
	out := make([]string, 0, len(raw))
	for i, value := range raw {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%s[%d] 必须是非空字符串", key, i)
		}
		out = append(out, strings.TrimSpace(text))
	}
	return out, nil
}

func retroReportObjects(report map[string]any, key string) ([]map[string]any, error) {
	raw, ok := report[key].([]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是数组", key)
	}
	out := make([]map[string]any, 0, len(raw))
	for i, value := range raw {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] 必须是对象", key, i)
		}
		out = append(out, object)
	}
	return out, nil
}

func retroEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func loadRetroFactTasks(root string) ([]*Task, []RetroFactGap) {
	seen := map[string]bool{}
	var tasks []*Task
	var gaps []RetroFactGap
	for _, dir := range []string{tasksDir(root), archiveDir(root)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				gaps = append(gaps, RetroFactGap{Code: "task_dir_read_failed", Detail: fmt.Sprintf("%s: %v", dir, err)})
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				gaps = append(gaps, RetroFactGap{Code: "task_read_failed", Detail: fmt.Sprintf("%s: %v", path, err)})
				continue
			}
			var task Task
			if err := json.Unmarshal(data, &task); err != nil || task.ID == "" {
				detail := "missing task id"
				if err != nil {
					detail = err.Error()
				}
				gaps = append(gaps, RetroFactGap{Code: "task_malformed", Detail: fmt.Sprintf("%s: %s", path, detail)})
				continue
			}
			if seen[task.ID] {
				continue // tasks/ is read first and is authoritative during a narrow archive migration window.
			}
			seen[task.ID] = true
			tasks = append(tasks, &task)
		}
	}
	return tasks, gaps
}

func retroCompletion(task *Task, events []TaskEvent) (string, time.Time, bool) {
	var latest *TaskEvent
	for i := range events {
		if events[i].Type == evDone && (latest == nil || events[i].Seq > latest.Seq) {
			latest = &events[i]
		}
	}
	if latest != nil {
		if parsed, err := time.Parse(time.RFC3339Nano, latest.TS); err == nil {
			return latest.TS, parsed, true
		}
	}
	for _, raw := range []string{task.UpdatedAt, task.CreatedAt} {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return raw, parsed, false
		}
	}
	return "", time.Time{}, false
}

func retroFailureClasses(task *Task, events []TaskEvent) []string {
	seen := map[string]bool{}
	for _, event := range events {
		if event.Type != evRetry && event.Type != evFailed {
			continue
		}
		reason := retroDetailString(event.Detail, "reason")
		if reason == "" {
			reason = retroDetailString(event.Detail, "err")
		}
		if reason == "" {
			reason = "unspecified"
		}
		seen[event.Type+":"+retroTrim(reason, 160)] = true
	}
	if len(seen) == 0 && strings.TrimSpace(task.LastError) != "" {
		seen["last_error:"+retroTrim(task.LastError, 160)] = true
	}
	out := make([]string, 0, len(seen))
	for class := range seen {
		out = append(out, class)
	}
	sort.Strings(out)
	return out
}

func retroTaskCost(task *Task, events []TaskEvent) (*float64, *int, string, string) {
	var terminal *TaskEvent
	for i := range events {
		switch events[i].Type {
		case evDone, evFailed, evCanceled, evHeld:
			if terminal == nil || events[i].Seq > terminal.Seq {
				terminal = &events[i]
			}
		}
	}
	if terminal != nil {
		if value, ok := retroNumber(terminal.Detail[evDetailCostTotal]); ok {
			turns := retroIntPointer(terminal.Detail[evDetailTurnsTotal])
			return &value, turns, "terminal_event", ""
		}
		if unavailable, _ := terminal.Detail[evDetailCostUnavailable].(bool); unavailable {
			reason := retroDetailString(terminal.Detail, evDetailCostUnavailReason)
			if reason == "" {
				reason = "terminal event explicitly marked cost_unavailable"
			}
			return nil, nil, "unavailable", reason
		}
	}
	if task.CostUSD > 0 || task.TurnsUsed > 0 {
		value, turns := task.CostUSD, task.TurnsUsed
		return &value, &turns, "task_fallback", ""
	}
	return nil, nil, "unavailable", "no terminal cost_total and no task-level usage"
}

func retroReviewVerdict(task *Task, events []TaskEvent) string {
	if task.Type != typeReview && task.XRole != "C" {
		return ""
	}
	var verdict string
	var seq int64
	for _, event := range events {
		value := strings.ToLower(strings.TrimSpace(retroDetailString(event.Detail, "verdict")))
		if value != "" && event.Seq >= seq {
			verdict, seq = value, event.Seq
		}
	}
	return verdict
}

func retroLimitFacts(events []TaskEvent) (int, bool) {
	paused, over := 0, false
	for _, event := range events {
		if event.Type == evLimitPaused {
			paused++
		}
		if event.Type == evHeld && retroDetailString(event.Detail, "reason") == "over_max_fix_rounds" {
			over = true
		}
	}
	return paused, over
}

func retroFinishCosts(cost *RetroFactsCost, values []float64) {
	if len(values) == 0 {
		return
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	total := 0.0
	for _, value := range sorted {
		total += value
	}
	total = retroRound(total)
	median := sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		median = (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}
	median, max := retroRound(median), retroRound(sorted[len(sorted)-1])
	cost.TotalUSD, cost.MedianUSD, cost.MaxUSD = &total, &median, &max
}

func marshalRetroFacts(facts *RetroFacts) ([]byte, string, error) {
	data, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func retroDetailString(detail map[string]any, key string) string {
	if detail == nil {
		return ""
	}
	value, ok := detail[key]
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func retroNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func retroIntPointer(value any) *int {
	number, ok := retroNumber(value)
	if !ok {
		return nil
	}
	result := int(number)
	return &result
}

func retroDimension(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func retroTrim(value string, max int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "…"
}

func retroRound(value float64) float64 {
	parsed, _ := strconv.ParseFloat(strconv.FormatFloat(value, 'f', 6, 64), 64)
	return parsed
}
