package main

// 限额识别与重置时间解析回归测试。
// 场景来自实战:远端账号限额提示是 "hit your session limit"(比本地多个 session 词),
// 旧 limitRe 不匹配 → 走普通失败路径烧 attempts,三次后任务假失败。

import (
	"strconv"
	"testing"
	"time"
)

func TestIsLimitHitPhrasings(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"远端 session limit 措辞", "You've hit your session limit · resets 8:20pm (Asia/Singapore)", true},
		{"headless 带 epoch", "Claude AI usage limit reached|1751600000", true},
		{"resets at 钟点", "You've reached your usage limit ... resets at 3pm", true},
		{"5 小时窗口", "5-hour limit reached", true},
		{"hit your limit 原形", "You've hit your limit", true},
		{"weekly 变体", "You've hit your weekly limit · resets Tuesday", true},
		{"普通错误不误判", "connection refused while fetching model list", false},
	}
	for _, c := range cases {
		res := &claudeResult{Type: "result", Subtype: "success", IsError: true, Result: c.text}
		if got := isLimitHit(res, ""); got != c.want {
			t.Errorf("%s: isLimitHit=%v, want %v (text=%q)", c.name, got, c.want, c.text)
		}
	}
}

func TestIsLimitHitGuardsOnSuccess(t *testing.T) {
	// 成功结果里出现限额字样（如任务产出讨论限额机制）不得误判为限额命中。
	res := &claudeResult{Type: "result", Subtype: "success", IsError: false,
		Result: "本工具围绕 usage limit 做队列调度……"}
	if isLimitHit(res, "") {
		t.Fatal("IsError=false 时不应判为限额命中")
	}
}

func TestParseResetEpochClockPhrase(t *testing.T) {
	cfg := &Config{LimitFallbackMin: 30, CooldownMarginSec: 90}
	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 7, 10, 18, 47, 0, 0, loc)

	got := parseResetEpoch("You've hit your session limit · resets 8:20pm (Asia/Singapore)", cfg, now)
	want := time.Date(2026, 7, 10, 20, 20, 0, 0, loc).Unix() + 90
	if got != want {
		t.Errorf("钟点措辞解析: got %d (%s), want %d (%s)",
			got, time.Unix(got, 0).In(loc), want, time.Unix(want, 0).In(loc))
	}

	// 已过钟点滚到次日
	lateNow := time.Date(2026, 7, 10, 21, 0, 0, 0, loc)
	got = parseResetEpoch("resets 8:20pm", cfg, lateNow)
	want = time.Date(2026, 7, 11, 20, 20, 0, 0, loc).Unix() + 90
	if got != want {
		t.Errorf("次日滚动: got %s, want %s", time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}

	// epoch 形态优先
	epoch := now.Add(3 * time.Hour).Unix()
	got = parseResetEpoch("Claude AI usage limit reached|"+strconv.FormatInt(epoch, 10), cfg, now)
	if got != epoch+90 {
		t.Errorf("epoch 形态: got %d, want %d", got, epoch+90)
	}

	// 全都解析不到 → 配置回退等待
	got = parseResetEpoch("some opaque failure", cfg, now)
	want = now.Add(30*time.Minute).Unix() + 90
	if got != want {
		t.Errorf("回退等待: got %d, want %d", got, want)
	}
}

// 跨天（周限额）措辞：实战 429 "resets Jul 16 at 1am"——旧 resetTimeRe 跨不过 "Jul 16 at"
// → 落 30min 回退，每 30min 空转到真解冻。回归此坑。
func TestParseResetEpochCrossDay(t *testing.T) {
	cfg := &Config{LimitFallbackMin: 30, CooldownMarginSec: 90}
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Date(2026, 7, 13, 14, 23, 0, 0, loc)

	got := parseResetEpoch("You've hit your limit · resets Jul 16 at 1am (Asia/Shanghai)", cfg, now)
	want := time.Date(2026, 7, 16, 1, 0, 0, 0, loc).Unix() + 90
	if got != want {
		t.Errorf("跨天解析: got %s, want %s", time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}

	// 带 :MM 与序数日 + "on" + 跨月（窗口内）
	julEndNow := time.Date(2026, 7, 30, 10, 0, 0, 0, loc)
	got = parseResetEpoch("resets on Aug 3rd at 9:30pm", cfg, julEndNow)
	want = time.Date(2026, 8, 3, 21, 30, 0, 0, loc).Unix() + 90
	if got != want {
		t.Errorf("序数日+分钟: got %s, want %s", time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}

	// 跨年：年末读到次年 1 月初的重置 → 滚到明年（1 月的日期本年已过，AddDate(1)）
	decNow := time.Date(2026, 12, 30, 12, 0, 0, 0, loc)
	got = parseResetEpoch("resets Jan 2 at 1am", cfg, decNow)
	want = time.Date(2027, 1, 2, 1, 0, 0, 0, loc).Unix() + 90
	if got != want {
		t.Errorf("跨年滚动: got %s, want %s", time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}

	// 越界（>14 天）不轻信 → 回退等待，不误锁多日
	got = parseResetEpoch("resets Sep 1 at 1am", cfg, now)
	want = now.Add(30*time.Minute).Unix() + 90
	if got != want {
		t.Errorf("越界回退: got %d, want %d", got, want)
	}
}
