package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRecentBurnForecastSurfacesAcceleration pins the user-visible distinction between the
// two lines: the whole-cycle OLS remains the stable baseline, while the tail fit reacts to a
// recent burst in the same account-wide quota samples. The latter is therefore allowed to warn
// about exhaustion before reset even when the whole-cycle average would not.
func TestRecentBurnForecastSurfacesAcceleration(t *testing.T) {
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	reset := now.Add(6 * time.Hour).Format(time.RFC3339)
	samples := []rawSample{
		{at: now.Add(-24 * time.Hour), pct: 0, resetsAt: reset},
		{at: now.Add(-18 * time.Hour), pct: 1, resetsAt: reset},
		{at: now.Add(-12 * time.Hour), pct: 2, resetsAt: reset},
		{at: now.Add(-8 * time.Hour), pct: 3, resetsAt: reset},
		{at: now.Add(-6 * time.Hour), pct: 4, resetsAt: reset},
		{at: now.Add(-2 * time.Hour), pct: 10, resetsAt: reset},
		{at: now.Add(-1 * time.Hour), pct: 40, resetsAt: reset},
		{at: now, pct: 70, resetsAt: reset},
	}

	src := buildBurnSource("codex:secondary:a", "codex", "a", "Codex A",
		"secondary", "周窗口", 7*24*60, samples, now, 90*time.Minute)
	if src == nil || src.BurnRatePctPerH == nil || src.RecentBurnRatePctPerH == nil {
		t.Fatalf("两种速率都应可算，got %+v", src)
	}
	if *src.RecentBurnRatePctPerH <= *src.BurnRatePctPerH {
		t.Fatalf("近期加速应高于本周期均速：recent=%v cycle=%v",
			*src.RecentBurnRatePctPerH, *src.BurnRatePctPerH)
	}
	if src.RecentSpanMinutes == nil || *src.RecentSpanMinutes != 360 {
		t.Fatalf("周窗口近期线应取实际 6h 尾部，got %v", src.RecentSpanMinutes)
	}
	if src.ExhaustBefore {
		t.Fatalf("本周期均速不应在 6h 内耗尽：rate=%v exhaust=%v", *src.BurnRatePctPerH, src.ExhaustAt)
	}
	if !src.RecentExhaustBefore || src.RecentExhaustAt == nil {
		t.Fatalf("近期综合线应提示重置前耗尽：rate=%v exhaust=%v",
			*src.RecentBurnRatePctPerH, src.RecentExhaustAt)
	}
	if src.Verdict != "将在重置前烧完" {
		t.Fatalf("任一可信趋势会先触底时结论必须预警，got %q", src.Verdict)
	}

	b, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"recent_burn_rate_pct_per_hour"`, `"recent_span_minutes"`,
		`"recent_exhaust_at"`, `"recent_exhaust_before_reset":true`,
	} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("JSON 契约缺 %s：%s", key, b)
		}
	}
}

// TestRecentBurnPeriodDisclosesBoundaryExtension covers low-frequency Claude samples. A sample
// five minutes outside the nominal 1h lookback is useful enough to fit the tail, but the API must
// report the real 65-minute span rather than claiming it was exactly one hour.
func TestRecentBurnPeriodDisclosesBoundaryExtension(t *testing.T) {
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	reset := now.Add(4 * time.Hour).Format(time.RFC3339)
	samples := []rawSample{
		{at: now.Add(-125 * time.Minute), pct: 10, resetsAt: reset},
		{at: now.Add(-65 * time.Minute), pct: 20, resetsAt: reset},
		{at: now, pct: 30, resetsAt: reset},
	}
	src := buildBurnSource("claude:session:a", "claude", "a", "Claude A",
		"session", "5 小时窗口", 300, samples, now, 90*time.Minute)
	if src == nil || src.RecentSpanMinutes == nil {
		t.Fatalf("边界扩展后近期趋势应可算，got %+v", src)
	}
	if *src.RecentSpanMinutes != 65 {
		t.Fatalf("必须披露实际 65 分钟跨度，got %v", *src.RecentSpanMinutes)
	}
}

// TestRecentBurnForecastNeverCrossesReset keeps the new line under the same hard boundary as the
// original one. A 90% sample from the prior reset would turn the fit negative or wildly optimistic;
// only the two points sharing the latest reset may survive.
func TestRecentBurnForecastNeverCrossesReset(t *testing.T) {
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	oldReset := now.Add(-2 * time.Hour).Format(time.RFC3339)
	newReset := now.Add(3 * time.Hour).Format(time.RFC3339)
	samples := []rawSample{
		{at: now.Add(-3 * time.Hour), pct: 90, resetsAt: oldReset},
		{at: now.Add(-60 * time.Minute), pct: 10, resetsAt: newReset},
		{at: now, pct: 20, resetsAt: newReset},
	}
	src := buildBurnSource("codex:primary:a", "codex", "a", "Codex A",
		"primary", "5 小时窗口", 300, samples, now, 90*time.Minute)
	if src == nil || len(src.Series) != 2 {
		t.Fatalf("当前周期只应保留两点，got %+v", src)
	}
	if src.RecentBurnRatePctPerH == nil || *src.RecentBurnRatePctPerH != 10 {
		t.Fatalf("近期线应只按新周期 10%%/h 拟合，got %v", src.RecentBurnRatePctPerH)
	}
}
