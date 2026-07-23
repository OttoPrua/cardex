package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Cooldown 是全局限额冷却：5 小时窗口用尽后，所有 claude 调用都会失败，
// 所以冷却是全局的，而不是单个任务的属性。
type Cooldown struct {
	UntilEpoch int64  `json:"until_epoch"`
	Reason     string `json:"reason"`
	SetAt      string `json:"set_at"`
}

func loadCooldown(root string) *Cooldown {
	data, err := os.ReadFile(cooldownPath(root))
	if err != nil {
		return nil
	}
	var c Cooldown
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	return &c
}

func (c *Cooldown) active(now time.Time) bool {
	return c != nil && now.Unix() < c.UntilEpoch
}

func setCooldown(root string, until int64, reason string) {
	c := Cooldown{UntilEpoch: until, Reason: reason, SetAt: time.Now().Format(time.RFC3339)}
	data, _ := json.MarshalIndent(c, "", "  ")
	_ = atomicWrite(cooldownPath(root), append(data, '\n'))
}

func clearCooldown(root string) { _ = os.Remove(cooldownPath(root)) }

type lockInfo struct {
	PID int    `json:"pid"`
	At  string `json:"at"`
}

// acquireLock 抢占单实例锁；持锁进程已死或锁超龄则视为陈旧锁清除。
// 【为什么用 tmp+os.Link 原子挂名】与 events.go:acquireEventLock 同源同类缺陷:
// 旧法"OpenFile(O_EXCL) 建空文件 → Write PID → Close"两步不原子, 另一进程在空文件窗口
// 内 staleLock 读到空 Unmarshal 失败即判 stale 强夺, 双持锁. 单实例锁虽然被两个 runner
// 同时启动的概率极低, 但缺陷同类, 一并按类闭合. tmp 里先写完 lockInfo 再 os.Link 到
// 目标路径, 保证 path 一旦存在即内容完整; 单实例锁的非阻塞语义(2 次重试后放弃)不变.
//
// 【CG-R1 修复:强夺唯一化用 os.Rename 原子搬走, 不再裸 os.Remove】旧法在判 staleLock 后
// 直接 os.Remove(path) — 若 A、B 都撞 stale 判据同时进入强夺分支: A 先 Remove、A 走下一轮
// os.Link 挂新锁成功、随即 B 也 Remove 却删掉了 A 刚挂的新锁 → 双持锁。改用 os.Rename
// 是 POSIX 原子操作, path 只能被一方成功搬走; 失败方(路径已被他人搬走)本轮直接进入下一次
// for 迭代重试 os.Link, 不会再动到已被夺权者挂上的新锁。与 events.go:216-221 同源同法。
func acquireLock(root string, ttl time.Duration) bool {
	path := lockPath(root)
	for i := 0; i < 2; i++ {
		tmp := fmt.Sprintf("%s.acq-%d-%d", path, os.Getpid(), time.Now().UnixNano())
		info, _ := json.Marshal(lockInfo{PID: os.Getpid(), At: time.Now().Format(time.RFC3339)})
		if err := os.WriteFile(tmp, info, 0o644); err != nil {
			return false
		}
		err := os.Link(tmp, path)
		_ = os.Remove(tmp)
		if err == nil {
			return true
		}
		if !staleLock(path, ttl) {
			return false
		}
		// 强夺唯一化:os.Rename 是 POSIX 原子操作, path 只能被一方成功搬走。失败者(路径已被他人
		// 搬走)本轮直接进入下一次 for 迭代, 不会再 Remove 一次导致"两方各以为夺权成功"删掉新锁。
		stale := fmt.Sprintf("%s.stale-%d-%d", path, os.Getpid(), time.Now().UnixNano())
		if renameErr := os.Rename(path, stale); renameErr == nil {
			_ = os.Remove(stale)
		}
	}
	return false
}

// staleLock 判据与 events.go:staleEventLock 严格同源:仅当(可解析&&!processAlive)或 mtime>TTL 判 stale.
// 【为什么内容空/不可解析 + mtime 未超 TTL 也判非 stale】与 events 锁同类闭合(P1-1):防御纵深——
// 即便 tmp+Link 已消除空文件窗口,若日后维护回退到 O_EXCL 两步式,此判据仍能挡"空内容→立即强夺"的
// bootstrap 竞态回归. 强夺允收边界收窄至"确定持有者已死"或"锁真的过期"两条, 内容尚不完整的窗口
// 一律等待, 不强夺.
func staleLock(path string, ttl time.Duration) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return true
	}
	if time.Since(fi.ModTime()) > ttl {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var li lockInfo
	if json.Unmarshal(data, &li) != nil || li.PID <= 0 {
		// 内容空/不可解析但 mtime 未超 TTL:视为合法写者尚未收尾, 不判 stale.
		// mtime>TTL 兜底了"永远收不完"的场景, 走上面的 return true.
		return false
	}
	return !processAlive(li.PID)
}

// releaseLock 只删属于本进程的锁文件:核 PID 匹配才 Remove。
// 【CG-R1 修复】旧法裸 os.Remove(path) 与 events.go:releaseEventLock 同类缺陷:staleLock
// 判 mtime>TTL 直接判 stale 强夺——系统睡眠/挂起跨 TTL 唤醒后,原持有者 A 的 defer release
// 无条件 Remove 会删掉强夺者 B 刚建的新锁,让第三写者 C 进临界区双持。核 PID 匹配才 Remove
// 是"只删自己名下的"最小契约,读文件失败/解析失败/PID 不匹配都不删——留给真正的持有者。
// 与 events.go:234-247 同源同法。
func releaseLock(root string) {
	path := lockPath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var li lockInfo
	if json.Unmarshal(data, &li) != nil {
		return
	}
	if li.PID != os.Getpid() {
		return
	}
	_ = os.Remove(path)
}

func lockTTL(cfg *Config) time.Duration {
	ttl := time.Duration(cfg.StepTimeoutMin*3) * time.Minute
	if min := 3 * time.Hour; ttl < min {
		ttl = min
	}
	return ttl
}

func fmtClock(epoch int64) string {
	t := time.Unix(epoch, 0)
	if time.Until(t) > 20*time.Hour || time.Since(t) > 20*time.Hour {
		return t.Format("01-02 15:04")
	}
	return t.Format("15:04")
}

func fmtIn(epoch int64, now time.Time) string {
	d := time.Unix(epoch, 0).Sub(now).Round(time.Minute)
	if d < 0 {
		return "已到期"
	}
	if d < time.Minute {
		return "<1m"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
