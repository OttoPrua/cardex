package main

import (
	"sort"
	"time"
)

// eligible 判断任务此刻是否可以被派发。
func eligible(t *Task, now time.Time) bool {
	if t == nil || !t.schedulingAllowed() {
		return false
	}
	switch t.Status {
	case statusQueued:
		return t.NotBeforeEpoch == 0 || now.Unix() >= t.NotBeforeEpoch
	case statusLimitPaused:
		return t.ResumeAtEpoch == 0 || now.Unix() >= t.ResumeAtEpoch
	case statusRunning:
		// 上次运行中途崩溃遗留的状态（锁已保证不会并发），当作可恢复处理。
		return true
	default:
		return false
	}
}

func typeRank(cfg *Config, typ string) int {
	for i, x := range cfg.TypeOrder {
		if x == typ {
			return i
		}
	}
	return len(cfg.TypeOrder)
}

// pickNext 派发规则：
//  1. resume_first: 被限额打断的任务优先于新排队任务（先把没做完的做完）；
//  2. 高 priority 优先；
//  3. 按 type_order 中的类型顺序（默认 审核 > 序列 > 装配）；
//  4. 先进先出。
func pickNext(cfg *Config, tasks []*Task, now time.Time) *Task {
	var cands []*Task
	for _, t := range tasks {
		if eligible(t, now) {
			cands = append(cands, t)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	interrupted := func(t *Task) bool { return t.Status == statusLimitPaused || t.Status == statusRunning }
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if cfg.ResumeFirst && interrupted(a) != interrupted(b) {
			return interrupted(a)
		}
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if ra, rb := typeRank(cfg, a.Type), typeRank(cfg, b.Type); ra != rb {
			return ra < rb
		}
		return a.CreatedAt < b.CreatedAt
	})
	return cands[0]
}

// nextWake 返回下一个任务变为可运行的时间（用于 status 展示），没有则返回零值。
func nextWake(tasks []*Task, now time.Time) time.Time {
	var best int64
	for _, t := range tasks {
		var at int64
		switch t.Status {
		case statusLimitPaused:
			at = t.ResumeAtEpoch
		case statusQueued:
			at = t.NotBeforeEpoch
		default:
			continue
		}
		if at > now.Unix() && (best == 0 || at < best) {
			best = at
		}
	}
	if best == 0 {
		return time.Time{}
	}
	return time.Unix(best, 0)
}
