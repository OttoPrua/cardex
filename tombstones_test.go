package main

// tombstones_test.go —— CG-4 幂等墓碑续接（注入至多一次）验收测试。
//
// 每条测试映射到 spec 的一项验收：
//  1) 崩溃窗口注入：mock 在"提示已发送、终稿未落盘"处 kill → 重启后总注入 ≤ 2（无上限则报红）。
//  2) 同一步连续两轮恢复对账 → 第二轮零注入（mock 计数断言 = 0）。
//  3) 反例注入：损坏字节 → 按无墓碑处理并披露，不 crash、不静默跳步（静默跳步即报红）。
// 另外三条附加覆盖：reset-at-entry 语义、archive-with-task、runTask/reconcile 整合。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkTombRoot 只需保证 root 目录存在；tombstones/ 在 writeTombstoneJournal 内按需 mkdir。
func mkTombRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// ---------- 单元层：injectAtMostOnce 语义 ----------

// TestInjectAtMostOnceCrashBoundedAtTwo 验收 #1（崩溃窗口注入）：
// 连续 5 轮 inject 全部"crash"（返回错误 → 终稿未落盘），mock 计数总注入 ≤ 2；断言恰为 2。
// 若代码没有上限，5 轮会跑满 5 次 inject → 该断言直接红。
func TestInjectAtMostOnceCrashBoundedAtTwo(t *testing.T) {
	root := mkTombRoot(t)
	calls := 0
	crash := func() error {
		calls++
		return errors.New("模拟崩溃：提示已发送但终稿未落盘")
	}
	for i := 0; i < 5; i++ {
		skipped, corrupted, err := injectAtMostOnce(root, "task-crash", "resume:0", crash)
		if corrupted {
			t.Fatalf("首轮不应报损坏: round=%d", i)
		}
		if i < 2 {
			// 前 2 轮:inject 被调用,err 透传;墓碑 pending 递增
			if err == nil || skipped {
				t.Fatalf("崩溃前两轮应透传错误且不跳过: round=%d skipped=%v err=%v", i, skipped, err)
			}
		} else {
			// 3-5 轮:bound=2 已耗尽,跳过、inject 不调用、err=nil
			if !skipped || err != nil {
				t.Fatalf("bound 耗尽后应静默跳过: round=%d skipped=%v err=%v", i, skipped, err)
			}
		}
	}
	if calls != 2 {
		t.Fatalf("总注入次数应恰为 2(bound=2 上限),got %d", calls)
	}
	// 墓碑状态应停在 pending(2):证明"bound 已耗尽,未落 final"——诚实揭示崩溃风暴。
	j, corrupted, err := readTombstoneJournal(root, "task-crash")
	if err != nil || corrupted {
		t.Fatalf("读账本失败: corrupted=%v err=%v", corrupted, err)
	}
	entry := j.Entries["resume:0"]
	if entry.Attempt != 2 || entry.Phase != tombstonePhasePending {
		t.Fatalf("墓碑应停 pending(2),got attempt=%d phase=%q", entry.Attempt, entry.Phase)
	}
}

// TestInjectAtMostOnceSecondRoundZero 验收 #2（同一步连续两轮零注入）：
// 第一轮 inject 成功、落 final;第二轮再调用 → skipped=true、inject 不调用、mock 计数不涨。
func TestInjectAtMostOnceSecondRoundZero(t *testing.T) {
	root := mkTombRoot(t)
	calls := 0
	ok := func() error { calls++; return nil }

	// 第一轮:正常注入
	skipped, corrupted, err := injectAtMostOnce(root, "task-2rounds", "resume:3", ok)
	if err != nil || skipped || corrupted {
		t.Fatalf("首轮应成功注入,got skipped=%v corrupted=%v err=%v", skipped, corrupted, err)
	}
	if calls != 1 {
		t.Fatalf("首轮 mock 计数应 1,got %d", calls)
	}

	// 第二轮:必须零注入
	skipped2, corrupted2, err2 := injectAtMostOnce(root, "task-2rounds", "resume:3", ok)
	if err2 != nil || !skipped2 || corrupted2 {
		t.Fatalf("次轮应静默跳过(final 已在),got skipped=%v corrupted=%v err=%v", skipped2, corrupted2, err2)
	}
	if calls != 1 {
		t.Fatalf("次轮 mock 计数应仍为 1(零注入),got %d", calls)
	}
	// 验证墓碑确实落到 final
	j, _, _ := readTombstoneJournal(root, "task-2rounds")
	if j.Entries["resume:3"].Phase != tombstonePhaseFinal {
		t.Fatalf("首轮结束后墓碑应 final,got %+v", j.Entries["resume:3"])
	}
}

// TestInjectAtMostOnceCorruptedFileDisclosedNotSilent 验收 #3（反例注入）：
// 向墓碑文件写入损坏字节 → 返回 corrupted=true(披露信号),inject 被调用(不静默跳过),
// 落盘后文件重归有效 JSON。若代码把损坏当 final(静默跳过),第二个断言直接红。
func TestInjectAtMostOnceCorruptedFileDisclosedNotSilent(t *testing.T) {
	root := mkTombRoot(t)
	// 手工造出损坏字节:半截 JSON,末尾无 } 也无 \n——极端场景模拟崩溃在 atomicWrite tmp→rename 之间。
	if err := os.MkdirAll(tombstonesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tombstonePath(root, "task-corrupt"), []byte(`{"version":1,"entries":{"resume:0":{"kin`), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := 0
	inject := func() error { calls++; return nil }
	skipped, corrupted, err := injectAtMostOnce(root, "task-corrupt", "resume:0", inject)
	if err != nil {
		t.Fatalf("损坏字节不应导致 IO 错误(应吞掉+披露): %v", err)
	}
	if skipped {
		t.Fatal("损坏字节被静默跳过:验收明令'不静默跳步'——测试报红")
	}
	if !corrupted {
		t.Fatal("损坏字节必须披露 corrupted=true,caller 才能加业务日志")
	}
	if calls != 1 {
		t.Fatalf("损坏字节应按无墓碑处理→inject 被调用一次,got %d", calls)
	}
	// 落盘后账本应重归有效:能被再次读到,包含刚写的 final。
	j, corrupted2, err := readTombstoneJournal(root, "task-corrupt")
	if err != nil || corrupted2 {
		t.Fatalf("写入 final 后账本应回到有效态,corrupted=%v err=%v", corrupted2, err)
	}
	if j.Entries["resume:0"].Phase != tombstonePhaseFinal {
		t.Fatalf("最终应有 final 墓碑,got %+v", j.Entries["resume:0"])
	}
}

// TestInjectAtMostOnceCrashThenRecoverySucceeds 补充覆盖:第一轮崩溃、第二轮成功 → 落 final,不再重派。
// 是"bound=2 意味着允许一次崩溃后重试成功"的正向验证。
func TestInjectAtMostOnceCrashThenRecoverySucceeds(t *testing.T) {
	root := mkTombRoot(t)
	calls := 0
	// 第一次崩溃,后续成功
	fn := func() error {
		calls++
		if calls == 1 {
			return errors.New("首次崩溃")
		}
		return nil
	}
	// 轮 1:崩溃
	if skipped, _, err := injectAtMostOnce(root, "task-recover", "resume:0", fn); err == nil || skipped {
		t.Fatalf("首轮应崩(err 非 nil、不跳过): skipped=%v err=%v", skipped, err)
	}
	// 轮 2:重试成功
	if skipped, _, err := injectAtMostOnce(root, "task-recover", "resume:0", fn); err != nil || skipped {
		t.Fatalf("次轮应成功(err=nil、不跳过): skipped=%v err=%v", skipped, err)
	}
	// 轮 3:final 已落,跳过
	if skipped, _, err := injectAtMostOnce(root, "task-recover", "resume:0", fn); err != nil || !skipped {
		t.Fatalf("三轮应静默跳过: skipped=%v err=%v", skipped, err)
	}
	if calls != 2 {
		t.Fatalf("总注入次数应 2(崩 1+成 1),got %d", calls)
	}
}

// TestInjectAtMostOnceKindsIndependent 不同 kind 的 bound 不能互相污染。
func TestInjectAtMostOnceKindsIndependent(t *testing.T) {
	root := mkTombRoot(t)
	cnt1, cnt2 := 0, 0
	// resume:0 崩满 bound
	for i := 0; i < 5; i++ {
		_, _, _ = injectAtMostOnce(root, "task-multi", "resume:0", func() error { cnt1++; return errors.New("x") })
	}
	if cnt1 != 2 {
		t.Fatalf("resume:0 计数应 2,got %d", cnt1)
	}
	// reconcile:cross 不受 resume:0 影响,首轮就能进 inject
	skipped, _, _ := injectAtMostOnce(root, "task-multi", reconcileCrossKind(), func() error { cnt2++; return nil })
	if skipped || cnt2 != 1 {
		t.Fatalf("reconcile:cross 应独立成功,skipped=%v cnt=%d", skipped, cnt2)
	}
}

// TestResetTombstoneKindClearsOne 验证 resetTombstoneKind 只清指定 kind,其他 kind 完好。
func TestResetTombstoneKindClearsOne(t *testing.T) {
	root := mkTombRoot(t)
	// 让 resume:0 落 final、reconcile:cross 落 final
	_, _, _ = injectAtMostOnce(root, "task-reset", "resume:0", func() error { return nil })
	_, _, _ = injectAtMostOnce(root, "task-reset", reconcileCrossKind(), func() error { return nil })
	if err := resetTombstoneKind(root, "task-reset", "resume:0"); err != nil {
		t.Fatal(err)
	}
	j, corrupted, err := readTombstoneJournal(root, "task-reset")
	if err != nil || corrupted {
		t.Fatalf("读账本失败: corrupted=%v err=%v", corrupted, err)
	}
	if _, ok := j.Entries["resume:0"]; ok {
		t.Fatal("reset 应清 resume:0")
	}
	if _, ok := j.Entries[reconcileCrossKind()]; !ok {
		t.Fatal("reset 不应影响其他 kind")
	}
}

// TestResetTombstoneKindEmptyJournalRemovesFile 清空后应删整个墓碑文件,
// 避免空 JSON({"entries":{}}) 长期残留占位。
func TestResetTombstoneKindEmptyJournalRemovesFile(t *testing.T) {
	root := mkTombRoot(t)
	_, _, _ = injectAtMostOnce(root, "task-empty", "resume:0", func() error { return nil })
	if _, err := os.Stat(tombstonePath(root, "task-empty")); err != nil {
		t.Fatalf("首次注入后应有墓碑文件: %v", err)
	}
	if err := resetTombstoneKind(root, "task-empty", "resume:0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tombstonePath(root, "task-empty")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("空账本应删文件,stat err=%v", err)
	}
}

// TestArchiveTaskTombstonesMovesFile 单元层验证归档函数搬迁墓碑到 archive/tombstones/。
func TestArchiveTaskTombstonesMovesFile(t *testing.T) {
	root := mkTombRoot(t)
	_, _, _ = injectAtMostOnce(root, "task-arch", "resume:0", func() error { return nil })
	if err := archiveTaskTombstones(root, "task-arch"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tombstonePath(root, "task-arch")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("原墓碑应被移走,stat err=%v", err)
	}
	if _, err := os.Stat(archivedTombstonePath(root, "task-arch")); err != nil {
		t.Fatalf("归档墓碑应存在: %v", err)
	}
}

// TestArchiveTaskAlsoArchivesTombstones 集成层:archiveTask 应把墓碑一并搬到 archive/tombstones/。
func TestArchiveTaskAlsoArchivesTombstones(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "归档带墓碑", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	// 用 injectAtMostOnce 产出真实的墓碑账本
	_, _, _ = injectAtMostOnce(root, tk.ID, "resume:0", func() error { return nil })
	if err := archiveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tombstonePath(root, tk.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("活动墓碑应被移走: %v", err)
	}
	if _, err := os.Stat(archivedTombstonePath(root, tk.ID)); err != nil {
		t.Fatalf("归档墓碑应存在: %v", err)
	}
}

// ---------- 集成层:runTask 与 reconcileCrossChains 的整合验证 ----------

// TestRunTaskWritesResumeTombstoneOnMidStep 集成:MidStep=true+SessionID 非空进入 runTask →
// 触发 resume 提示注入 → 墓碑账本应落 final(kind=resume:<step>)。
func TestRunTaskWritesResumeTombstoneOnMidStep(t *testing.T) {
	root := testRoot(t)
	claudeBin := fakeClaudeBin(t, mkOKResultJSON("sess-A"), "", 0)
	cfg := runTaskCfg(t, claudeBin)

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "resume 墓碑", work, []string{"p1", "p2"}, 5)
	tk.MidStep = true
	tk.SessionID = "sess-A" // 让 resuming=true
	// 让第一步显式指标状态:上一轮 limit_paused 触发本次 runTask 是"合法新一轮尝试"。
	tk.Status = statusLimitPaused
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}

	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}
	j, corrupted, err := readTombstoneJournal(root, tk.ID)
	if err != nil || corrupted {
		t.Fatalf("读墓碑失败: corrupted=%v err=%v", corrupted, err)
	}
	// resume:0 应有 final(第 0 步续跑注入)
	e0 := j.Entries[resumeKind(0)]
	if e0.Phase != tombstonePhaseFinal {
		t.Fatalf("resume:0 应 final,got %+v", e0)
	}
}

// TestRunTaskResetsResumeTombstoneOnFreshEntry 集成:两轮合法 limit 恢复 → 每轮 fresh entry
// 都会 reset 上一轮的 resume 墓碑,业务不因 bound 被误跳过。
func TestRunTaskResetsResumeTombstoneOnFreshEntry(t *testing.T) {
	root := testRoot(t)
	// 先造出一份 resume:0 已达 bound 的墓碑(模拟上一轮撞过 bound)。
	_, _, _ = injectAtMostOnce(root, "t-fresh", "resume:0", func() error { return errors.New("x") })
	_, _, _ = injectAtMostOnce(root, "t-fresh", "resume:0", func() error { return errors.New("x") })
	j, _, _ := readTombstoneJournal(root, "t-fresh")
	if j.Entries["resume:0"].Attempt != 2 || j.Entries["resume:0"].Phase != tombstonePhasePending {
		t.Fatalf("前置状态应 bound 已耗尽: %+v", j.Entries["resume:0"])
	}
	// runTask 顶部的 reset 逻辑:限额恢复的合法新一轮应清墓碑。手工触发 reset:
	if err := resetTombstoneKind(root, "t-fresh", "resume:0"); err != nil {
		t.Fatal(err)
	}
	// 再次 inject 应能重新走通(bound 从零起算)
	calls := 0
	skipped, _, err := injectAtMostOnce(root, "t-fresh", "resume:0", func() error { calls++; return nil })
	if err != nil || skipped {
		t.Fatalf("reset 后应能新开一轮 bound,skipped=%v err=%v", skipped, err)
	}
	if calls != 1 {
		t.Fatalf("reset 后应正常 inject 一次,got %d", calls)
	}
}

// TestReconcileCrossChainsGuardsAtMostOnce 集成:reconcileCrossChains 首次孤儿判 failed 落墓碑 final;
// 二次调用(手工把 status 改回 done 冒充"cli retry 未清墓碑就复活"这种畸形序列)必须走 skipped→held
// 披露路径:第二条 orphan 事件不涨(墓碑挡住 inject),status 升级到 held(不再冒充可采信 done)+
// evHeld(reason=reconcile_cross_tombstone_exhausted) 落账本。
//
// 【为什么本轮修订"次轮期望 status=done + 零披露"为"次轮 status=held + 披露 held 事件"】
// Round-0 审核报告 P1-1 明写:老版本把"卡留 done、零披露"当期望 → 固化了"final 挡住的孤儿卡永久
// 冒充可采信结果"的盲区。Round-1 fix 让 skipped 升级 held+emit 披露事件,本测试同步跟进(不是弱化,
// 是把测试对准新的正确契约)。此外正常 cli retry 会调 resetTombstoneKind(reconcileCrossKind()),
// 本测试用"手工强设 done 冒充复发"是极端反例场景,专门检验最后一道护栏。
func TestReconcileCrossChainsGuardsAtMostOnce(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	// 造一张 done 的 A 孤儿卡(没有 B 后继)
	a := newTask(root, cfg, typeCrossCheck, "A卡", "/tmp", []string{"p"}, 5)
	a.XRole = "A"
	a.XKey = "xkey123"
	a.Status = statusDone
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}

	tasks := []*Task{a}
	active := map[string]bool{}

	// 首轮:应改 failed + 落墓碑 final。
	reconcileCrossChains(root, tasks, active)
	a1, err := loadTask(root, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a1.Status != statusFailed {
		t.Fatalf("首轮 reconcile 应把孤儿改 failed,got %s", a1.Status)
	}
	j, _, err := readTombstoneJournal(root, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := j.Entries[reconcileCrossKind()]
	if e.Phase != tombstonePhaseFinal {
		t.Fatalf("首轮 reconcile 应落 final 墓碑,got %+v", e)
	}
	// 数事件:期望恰好 1 条 failed 事件(reason=cross_chain_orphan)。
	events := readAllEventsRaw(t, root, a.ID)
	orphanCnt := 0
	for _, ev := range events {
		if ev.Type == evFailed {
			if r, _ := ev.Detail["reason"].(string); r == "cross_chain_orphan" {
				orphanCnt++
			}
		}
	}
	if orphanCnt != 1 {
		t.Fatalf("首轮应 1 条孤儿 failed 事件,got %d, all=%+v", orphanCnt, events)
	}

	// 次轮:手工把 status 改回 done 冒充"未清墓碑就复发"的畸形。
	a1.Status = statusDone
	if err := saveTask(root, a1); err != nil {
		t.Fatal(err)
	}
	tasks2 := []*Task{a1}
	reconcileCrossChains(root, tasks2, active)
	events2 := readAllEventsRaw(t, root, a.ID)

	// (1) inject 被墓碑挡住:cross_chain_orphan 事件不涨(仍为 1)。
	orphanCnt2 := 0
	// (2) 但必须披露 held(不再冒充可采信 done)——skipped 分支的新契约。
	heldCnt := 0
	for _, ev := range events2 {
		if ev.Type == evFailed {
			if r, _ := ev.Detail["reason"].(string); r == "cross_chain_orphan" {
				orphanCnt2++
			}
		}
		if ev.Type == evHeld && ev.Actor == "runner:reconcile-tombstone" {
			if r, _ := ev.Detail["reason"].(string); r == "reconcile_cross_tombstone_exhausted" {
				heldCnt++
			}
		}
	}
	if orphanCnt2 != 1 {
		t.Fatalf("次轮 reconcile 应被墓碑挡住 inject,孤儿事件仍应为 1 条,got %d 序列=%v", orphanCnt2, eventTypes(events2))
	}
	if heldCnt != 1 {
		t.Fatalf("次轮 reconcile skipped 分支必须 emit 1 条 held 披露,got %d 序列=%v", heldCnt, eventTypes(events2))
	}
	// (3) status 应升级到 held(不再冒充可采信 done)。
	a2, _ := loadTask(root, a.ID)
	if a2.Status != statusHeld {
		t.Fatalf("次轮 reconcile skipped 分支应把 status 升级 held,got %s", a2.Status)
	}
}

// TestReadTombstoneJournalIgnoresEmptyFile 空文件应等同于损坏字节(json 解析失败) → corrupted=true。
// 保证 resetTombstoneKind 删完空文件的策略不会因残留 0 字节文件误导后续读取。
func TestReadTombstoneJournalIgnoresEmptyFile(t *testing.T) {
	root := mkTombRoot(t)
	if err := os.MkdirAll(tombstonesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tombstonePath(root, "empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, corrupted, err := readTombstoneJournal(root, "empty")
	if err != nil {
		t.Fatalf("空文件不应报 IO 错误: %v", err)
	}
	if !corrupted {
		t.Fatal("空文件应被披露为 corrupted(空字节 json.Unmarshal 失败)")
	}
}

// TestArchiveTombstoneRoundtrip 联合测试:归档后墓碑文件搬到 archive/tombstones/,
// 且原始账本内容与归档后一致(binary equal)。
func TestArchiveTombstoneRoundtrip(t *testing.T) {
	root := mkTombRoot(t)
	_, _, _ = injectAtMostOnce(root, "task-round", "resume:1", func() error { return nil })
	orig, err := os.ReadFile(tombstonePath(root, "task-round"))
	if err != nil {
		t.Fatal(err)
	}
	if err := archiveTaskTombstones(root, "task-round"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(archivedTombstonePath(root, "task-round"))
	if err != nil {
		t.Fatal(err)
	}
	if string(orig) != string(got) {
		t.Fatalf("归档前后内容应一致\norig=%s\ngot=%s", string(orig), string(got))
	}
	// 归档目录路径应正确
	if got := archivedTombstonesDir(root); !strings.HasSuffix(got, filepath.Join("archive", "tombstones")) {
		t.Fatalf("归档目录后缀错: %s", got)
	}
}

// TestInjectAtMostOnceEmptyIDNoOp 空 taskID 或空 kind 应静默跳过(caller 兜底不该走到这)。
func TestInjectAtMostOnceEmptyIDNoOp(t *testing.T) {
	root := mkTombRoot(t)
	calls := 0
	inc := func() error { calls++; return nil }
	// 空 id:一次都不该调 inject
	skipped, corrupted, err := injectAtMostOnce(root, "", "resume:0", inc)
	if err != nil || skipped || corrupted || calls != 0 {
		t.Fatalf("空 id 应静默 no-op,got skipped=%v corrupted=%v err=%v calls=%d", skipped, corrupted, err, calls)
	}
	// 空 kind:同理
	skipped, corrupted, err = injectAtMostOnce(root, "x", "", inc)
	if err != nil || skipped || corrupted || calls != 0 {
		t.Fatalf("空 kind 应静默 no-op,got skipped=%v corrupted=%v err=%v calls=%d", skipped, corrupted, err, calls)
	}
}

// ---------- 兜底探针 ----------

// TestTombstoneNonceMonotonic 墓碑 Nonce 应单调递增,便于人工审计时看清楚每次尝试的先后顺序。
func TestTombstoneNonceMonotonic(t *testing.T) {
	root := mkTombRoot(t)
	// 让第一轮崩溃,pending 落一次 nonce
	_, _, _ = injectAtMostOnce(root, "t-mono", "resume:0", func() error { return fmt.Errorf("x") })
	j1, _, _ := readTombstoneJournal(root, "t-mono")
	n1 := j1.Entries["resume:0"].Nonce
	if n1 <= 0 {
		t.Fatalf("首轮 nonce 应 >0,got %d", n1)
	}
	// 二轮成功,pending→final 期间 nonce 再落一次
	_, _, _ = injectAtMostOnce(root, "t-mono", "resume:0", func() error { return nil })
	j2, _, _ := readTombstoneJournal(root, "t-mono")
	n2 := j2.Entries["resume:0"].Nonce
	if n2 <= n1 {
		t.Fatalf("次轮 nonce 应严格递增,got %d <= %d", n2, n1)
	}
}

// 兜底:确保 tombstoneJournal 结构在 JSON 序列化后仍能被无损反序列化(防未来改字段名忘同步)。
func TestTombstoneJournalRoundtripJSON(t *testing.T) {
	j := tombstoneJournal{
		Version: 1,
		Entries: map[string]Tombstone{
			"resume:0": {Kind: "resume:0", Attempt: 1, Phase: tombstonePhaseFinal, Nonce: 1234, TS: "2026-07-23T01:00:00Z"},
		},
	}
	data, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var back tombstoneJournal
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	got := back.Entries["resume:0"]
	if got.Phase != tombstonePhaseFinal || got.Attempt != 1 || got.Nonce != 1234 {
		t.Fatalf("roundtrip 字段丢失: %+v", got)
	}
}

// ---------- Round-1 修复回红反例（每个 P1 一个,反例注入即报红） ----------

// TestRunTaskHoldsWhenRunningSideTombstoneExhausted (Round-1 P1-2 running 承重分支反例)
// 验证 runner.go:551 的"if t.Status != statusRunning"承重分支的 running 侧:上一轮 runTask
// 中途崩溃遗留(Status=running + MidStep=true + SessionID 非空 + resume:0 attempt=2 pending)
// 再入 runTask 时,reset 不得触发,墓碑 bound 挡住 inject → fakeClaude 零调用 → 升级 statusHeld
// + emit evHeld(reason=resume_tombstone_exhausted, actor=runner:tombstone)。
//
// 【为什么必须真调 runTask 而非手工验证 helper】原有 TestRunTaskResetsResumeTombstoneOnFreshEntry
// 只手工调 resetTombstoneKind,把 runner.go:551 的条件整个删掉(无条件 reset、崩溃风暴保护完全
// 失效)全套测试仍绿——承重分支等于零测试覆盖。本测试直接调 runTask,删条件即 reset 总触发 →
// tombstone 清空 → inject 正常调 fakeClaude → 事件不再是 [dispatched, held] 而是 [dispatched,
// step_ok(+ ...)] → 断言直接报红。
func TestRunTaskHoldsWhenRunningSideTombstoneExhausted(t *testing.T) {
	root := testRoot(t)
	claudeBin := fakeClaudeBin(t, mkOKResultJSON("sess-A"), "", 0)
	cfg := runTaskCfg(t, claudeBin)

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "崩溃残留-running", work, []string{"p1", "p2"}, 5)
	tk.MidStep = true
	tk.SessionID = "sess-A"         // 让 resuming=true
	tk.Status = statusRunning       // 关键:running 侧入场——模拟"上一轮 runTask 中途崩溃遗留"
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	// 预置 resume:0 attempt=2 pending 墓碑(模拟上一轮已耗尽 bound):两轮崩溃 inject 即成。
	_, _, _ = injectAtMostOnce(root, tk.ID, resumeKind(0), func() error { return errors.New("crash-round-1") })
	_, _, _ = injectAtMostOnce(root, tk.ID, resumeKind(0), func() error { return errors.New("crash-round-2") })
	preJ, _, err := readTombstoneJournal(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preJ.Entries[resumeKind(0)].Attempt != 2 || preJ.Entries[resumeKind(0)].Phase != tombstonePhasePending {
		t.Fatalf("前置状态应 attempt=2 pending,got %+v", preJ.Entries[resumeKind(0)])
	}

	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}

	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) != 2 {
		t.Fatalf("running 侧墓碑挡住应恰 [dispatched, held] 两条事件,got %d: %v", len(events), eventTypes(events))
	}
	if events[0].Type != evDispatched {
		t.Fatalf("首条应 dispatched,got %v", eventTypes(events))
	}
	if events[1].Type != evHeld {
		t.Fatalf("次条应 held,got %v", eventTypes(events))
	}
	if reason, _ := events[1].Detail["reason"].(string); reason != "resume_tombstone_exhausted" {
		t.Fatalf("held.reason 应 resume_tombstone_exhausted,got %q", reason)
	}
	if events[1].Actor != "runner:tombstone" {
		t.Fatalf("held.actor 应 runner:tombstone,got %q", events[1].Actor)
	}
	// 关键反例守卫:fakeClaude 零调用——若 inject 被调,fakeClaude 返回 mkOKResultJSON 会让
	// runTask 推 Step→1、emit evStepOK。若事件里出现 step_ok 直接说明 reset 被误触发。
	for _, ev := range events {
		if ev.Type == evStepOK || ev.Type == evDone {
			t.Fatalf("running 侧墓碑挡住路径不应出现 step_ok/done(fakeClaude 被调),got 序列=%v", eventTypes(events))
		}
	}
	// 盘上状态应转 held。
	tk2, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk2.Status != statusHeld {
		t.Fatalf("running 侧墓碑挡住应 status=held,got %s", tk2.Status)
	}
	// 墓碑仍应停 attempt=2 pending(bound 未被误清):证明 reset-at-entry 没触发。
	postJ, _, err := readTombstoneJournal(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if postJ.Entries[resumeKind(0)].Attempt != 2 || postJ.Entries[resumeKind(0)].Phase != tombstonePhasePending {
		t.Fatalf("墓碑应仍停 attempt=2 pending(reset 未触发),got %+v", postJ.Entries[resumeKind(0)])
	}
}

// TestRunTaskResetsAndInjectsWhenLimitPausedFreshEntry (Round-1 P1-2 对照组)
// 承接上一测试构造的等价墓碑,把入场状态改为 statusLimitPaused → reset-at-entry 触发、bound 清零、
// inject 恰调 1 次 → 事件流出现 evStepOK。这是 running/limit_paused 两侧的双向对照:
// 承重条件的 running 侧走 held(上一测试),limit_paused 侧走正常续跑。
//
// 【为什么这两个测试要同一份墓碑构造】只测 running 侧不能证明"条件是 statusRunning";只测 limit_paused
// 也不能证明差异。两个一起做才把承重分支的语义锁死:同墓碑、不同入场状态,恰好走不同路径。
func TestRunTaskResetsAndInjectsWhenLimitPausedFreshEntry(t *testing.T) {
	root := testRoot(t)
	claudeBin := fakeClaudeBin(t, mkOKResultJSON("sess-A"), "", 0)
	cfg := runTaskCfg(t, claudeBin)

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "限额恢复-fresh", work, []string{"p1", "p2"}, 5)
	tk.MidStep = true
	tk.SessionID = "sess-A"
	tk.Status = statusLimitPaused // 关键:合法限额恢复入场——reset-at-entry 应触发
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	// 同样预置 resume:0 attempt=2 pending 墓碑。
	_, _, _ = injectAtMostOnce(root, tk.ID, resumeKind(0), func() error { return errors.New("crash-round-1") })
	_, _, _ = injectAtMostOnce(root, tk.ID, resumeKind(0), func() error { return errors.New("crash-round-2") })

	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}

	// fresh entry → reset → inject 成功 → 墓碑应为 final,attempt=1(而非仍是 pending 2)。
	postJ, _, err := readTombstoneJournal(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := postJ.Entries[resumeKind(0)]
	if e.Phase != tombstonePhaseFinal {
		t.Fatalf("fresh-entry limit_paused 侧墓碑应 final,got %+v", e)
	}
	if e.Attempt != 1 {
		t.Fatalf("fresh-entry attempt 应从 1 起算(reset 后新一轮 bound),got attempt=%d", e.Attempt)
	}
	// 事件流应有 step_ok(证明 fakeClaude 被调用了 1 次);不得升级 held。
	events := readAllEventsRaw(t, root, tk.ID)
	sawStepOK := false
	for _, ev := range events {
		if ev.Type == evStepOK {
			sawStepOK = true
		}
		if ev.Type == evHeld {
			t.Fatalf("fresh entry 不应升级 held,got 事件序列=%v", eventTypes(events))
		}
	}
	if !sawStepOK {
		t.Fatalf("fresh-entry inject 后应至少 1 条 step_ok,got 事件序列=%v", eventTypes(events))
	}
}

// TestReconcileCrossChainsHoldsOnTombstoneSkipped (Round-1 P1-1 skipped-discard 类反例)
// 验证 runner.go:1676 injectAtMostOnce 返回 skipped=true 时:reconcileCrossChains 必须升级 statusHeld
// 并 emit evHeld(reason=reconcile_cross_tombstone_exhausted, actor=runner:reconcile-tombstone),
// 而不是把 skipped 用 `_` 静默丢弃让单腿 done 卡永久冒充可采信结果。
//
// 【构造场景】预置 reconcile:cross final 墓碑 → 手工把孤儿 A 卡的状态从 failed 复位回 done(冒充
// "cli retry 复活再次孤儿") → reconcileCrossChains 调 → injectAtMostOnce 见 phase=final 返回 skipped
// → 期望:统一升级 held + emit 披露事件,不再让"final 挡住"的孤儿卡零披露永久冒充可采信。
//
// 【反例】把 runner.go 的 skipped 分支删掉(退回原 `_, corrupted, tombErr :=` 静默丢弃):
// 卡留 status=done、零披露事件——本测试的 held 断言直接报红。
func TestReconcileCrossChainsHoldsOnTombstoneSkipped(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	a := newTask(root, cfg, typeCrossCheck, "A孤儿-skipped 复活", "/tmp", []string{"p"}, 5)
	a.XRole = "A"
	a.XKey = "xkey-skipped-1"
	a.Status = statusDone
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}
	// 预置 reconcile:cross final 墓碑——模拟上一轮 reconcile 已经判过 failed 并落 final。
	_, _, _ = injectAtMostOnce(root, a.ID, reconcileCrossKind(), func() error { return nil })
	preJ, _, _ := readTombstoneJournal(root, a.ID)
	if preJ.Entries[reconcileCrossKind()].Phase != tombstonePhaseFinal {
		t.Fatalf("前置应 reconcile:cross final,got %+v", preJ.Entries[reconcileCrossKind()])
	}

	// 触发 reconcile:卡仍是 done+孤儿,injectAtMostOnce 应返回 skipped=true。
	reconcileCrossChains(root, []*Task{a}, map[string]bool{})

	// 期望:卡升级挂 held,不再冒充 done。
	a2, err := loadTask(root, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a2.Status != statusHeld {
		t.Fatalf("skipped 应升级 held(不再冒充 done),got status=%s", a2.Status)
	}
	// 事件流应有 evHeld,actor=runner:reconcile-tombstone,reason=reconcile_cross_tombstone_exhausted。
	events := readAllEventsRaw(t, root, a.ID)
	sawHeld := false
	for _, ev := range events {
		if ev.Type == evHeld && ev.Actor == "runner:reconcile-tombstone" {
			sawHeld = true
			if reason, _ := ev.Detail["reason"].(string); reason != "reconcile_cross_tombstone_exhausted" {
				t.Fatalf("held.reason 应 reconcile_cross_tombstone_exhausted,got %q", reason)
			}
			if kind, _ := ev.Detail["kind"].(string); kind != reconcileCrossKind() {
				t.Fatalf("held.kind 应 %s,got %q", reconcileCrossKind(), kind)
			}
			if role, _ := ev.Detail["role"].(string); role != "A" {
				t.Fatalf("held.role 应 A,got %q", role)
			}
			if miss, _ := ev.Detail["missing_next"].(string); miss != "B" {
				t.Fatalf("held.missing_next 应 B,got %q", miss)
			}
		}
	}
	if !sawHeld {
		t.Fatalf("skipped 分支必须 emit evHeld(runner:reconcile-tombstone),got 事件序列=%v", eventTypes(events))
	}
}

// TestReconcileCrossChainsHoldsOnBoundExhausted (Round-1 P1-1 skipped-discard 类反例——bound 侧)
// 验证 skipped=true 的第二条触发路径:bound 已耗尽(attempt=2 pending)。
// 【构造场景】saveTask 连续两轮瞬时失败(如磁盘满)攒出 attempt=2 pending → 磁盘恢复后
// 该卡仍是 done+孤儿 → reconcile 撞上时 injectAtMostOnce 返回 skipped=true(bound 耗尽) →
// 期望:升级 held + emit 披露,不再无限 stderr 刷屏。
func TestReconcileCrossChainsHoldsOnBoundExhausted(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	a := newTask(root, cfg, typeCrossCheck, "A孤儿-bound 耗尽", "/tmp", []string{"p"}, 5)
	a.XRole = "A"
	a.XKey = "xkey-bound-1"
	a.Status = statusDone
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}
	// 构造 reconcile:cross attempt=2 pending(两轮 inject 都返错)——模拟 saveTask 连续失败。
	_, _, _ = injectAtMostOnce(root, a.ID, reconcileCrossKind(), func() error { return errors.New("saveTask 失败-1") })
	_, _, _ = injectAtMostOnce(root, a.ID, reconcileCrossKind(), func() error { return errors.New("saveTask 失败-2") })
	preJ, _, _ := readTombstoneJournal(root, a.ID)
	pre := preJ.Entries[reconcileCrossKind()]
	if pre.Phase != tombstonePhasePending || pre.Attempt != 2 {
		t.Fatalf("前置应 reconcile:cross pending attempt=2,got %+v", pre)
	}

	reconcileCrossChains(root, []*Task{a}, map[string]bool{})

	a2, err := loadTask(root, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a2.Status != statusHeld {
		t.Fatalf("bound 耗尽应升级 held,got status=%s", a2.Status)
	}
	// 事件流必须含 held,不能只靠 stderr 刷屏。
	events := readAllEventsRaw(t, root, a.ID)
	sawHeld := false
	for _, ev := range events {
		if ev.Type == evHeld && ev.Actor == "runner:reconcile-tombstone" {
			sawHeld = true
		}
	}
	if !sawHeld {
		t.Fatalf("bound 耗尽应 emit evHeld,got 事件序列=%v", eventTypes(events))
	}
}

// TestCmdRetryResetsReconcileCrossTombstone (Round-1 P1-1 no-reset-on-retry 类反例)
// 验证 main.go 的 cmdSetStatus retry 分支必须显式重置 reconcile:cross 墓碑:
// 场景 A 卡已被 reconcile 判 failed(final 墓碑落盘)→ 人工 claudego retry → 期望:reconcile:cross
// 墓碑被清空 → 下一轮孤儿判可以重新起 bound=2 保护。
//
// 【反例】去掉 main.go retry 分支的 resetTombstoneKind(reconcileCrossKind()) 那行:
// 墓碑 final 保留 → 复活后的 A 卡再次成为孤儿会被 final 静默挡住,单腿 done 永久冒充可采信结果。
func TestCmdRetryResetsReconcileCrossTombstone(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	a := newTask(root, cfg, typeCrossCheck, "retry-reset A", "/tmp", []string{"p"}, 5)
	a.XRole = "A"
	a.XKey = "xkey-retry-1"
	a.Status = statusFailed // 已被 reconcile 判过 failed 的孤儿
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}
	// 造 reconcile:cross final 墓碑(模拟上一轮判 failed 时落的 final)。
	_, _, _ = injectAtMostOnce(root, a.ID, reconcileCrossKind(), func() error { return nil })
	// 顺手在同一账本里造 resume:0 final 观察 retry 不误伤(反例守卫)。
	_, _, _ = injectAtMostOnce(root, a.ID, resumeKind(0), func() error { return nil })

	if err := cmdSetStatus([]string{"-root", root, a.ID}, "retry"); err != nil {
		t.Fatal(err)
	}

	j, corrupted, err := readTombstoneJournal(root, a.ID)
	if err != nil {
		t.Fatalf("读账本失败: err=%v corrupted=%v", err, corrupted)
	}
	if _, exists := j.Entries[reconcileCrossKind()]; exists {
		t.Fatalf("retry 分支必须清 reconcile:cross 墓碑,got %+v", j.Entries[reconcileCrossKind()])
	}
	// 反例守卫:retry 只清 reconcile:cross,resume 侧留给 runTask reset-at-entry。
	// 若 retry 无脑清全部,resume:0 会被误伤——本断言就该保住 resume 侧的归属。
	if _, exists := j.Entries[resumeKind(0)]; !exists {
		t.Fatalf("retry 不应触碰 resume:0 墓碑(reset-at-entry 归属 runTask),got 账本被清空 %+v", j.Entries)
	}
}

// TestCmdReleaseResetsReconcileCrossTombstone (Round-1 P1-1 同类闭合——release 侧)
// 同类闭合探针:审核报告只点 retry,但 release 是从 held→queued 的另一条 cli 路径,
// 逻辑同源。若不闭合,新加的 skipped→held 路径会造出"ops release → runTask 走完 no_more_prompts
// → 又 done+孤儿 → reconcile 再次 skipped → 再次挂 held"的 held↔release 无穷震荡。
// 【反例】去掉 main.go release 分支的 resetTombstoneKind(reconcileCrossKind()) 那行:
// 释放后墓碑仍 final/pending(2),孤儿再次撞 skipped 分支——本测试断言"释放后墓碑清空"直接报红。
func TestCmdReleaseResetsReconcileCrossTombstone(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	a := newTask(root, cfg, typeCrossCheck, "release-reset A", "/tmp", []string{"p"}, 5)
	a.XRole = "A"
	a.XKey = "xkey-release-1"
	a.Status = statusHeld // 已经被 reconcile skipped 分支挂到 held
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}
	// 造 reconcile:cross final 墓碑(模拟 reconcile 之前已经落 final 挡住)。
	_, _, _ = injectAtMostOnce(root, a.ID, reconcileCrossKind(), func() error { return nil })
	// resume:1 用来验证 release 不误伤其他 kind。
	_, _, _ = injectAtMostOnce(root, a.ID, resumeKind(1), func() error { return nil })

	if err := cmdSetStatus([]string{"-root", root, a.ID}, "release"); err != nil {
		t.Fatal(err)
	}

	j, corrupted, err := readTombstoneJournal(root, a.ID)
	if err != nil {
		t.Fatalf("读账本失败: err=%v corrupted=%v", err, corrupted)
	}
	if _, exists := j.Entries[reconcileCrossKind()]; exists {
		t.Fatalf("release 分支必须清 reconcile:cross 墓碑,got %+v", j.Entries[reconcileCrossKind()])
	}
	// 同类闭合的反例守卫:release 只清 reconcile:cross,resume 侧留给 runTask reset-at-entry。
	if _, exists := j.Entries[resumeKind(1)]; !exists {
		t.Fatalf("release 不应触碰 resume:1 墓碑(归属 runTask),got 账本被清空 %+v", j.Entries)
	}
}

// TestReconcileCrossChainsAfterRetryReAdjudicates (Round-1 P1-1 端到端契约反例)
// 端到端串起 fix P1-1a + P1-1b:一张被 reconcile 判过 failed 的孤儿卡,人工 retry 后再次成为
// 孤儿时应能被 reconcile 重新裁决(而非被 final 墓碑静默挡住)。
//
// 【为什么这条测试关键】P1-1a(retry 清墓碑)与 P1-1b(skipped 挂 held)单独测都不能证明端到端契约成立:
// P1-1a 只保证墓碑被清、不管清完能否推进业务;P1-1b 只保证挡住时会披露、不管挡的正当性。串起来才
// 证明:retry 复活 → 再次孤儿 → 不被误挡 → 事件账本再记一条 failed(reason=cross_chain_orphan)。
//
// 【反例】任何一条修复被回退,这条端到端就会报红:回退 P1-1a → 第二轮 reconcile 被 final 挡住 →
// 事件不再是 [..., failed] 而是 [..., held];回退 P1-1b → skipped 静默丢弃 → 卡还留 done、事件里
// 完全无第二条 failed/held。
func TestReconcileCrossChainsAfterRetryReAdjudicates(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	a := newTask(root, cfg, typeCrossCheck, "retry+reconcile 端到端", "/tmp", []string{"p"}, 5)
	a.XRole = "A"
	a.XKey = "xkey-e2e-1"
	a.Status = statusDone
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}
	// 第一轮 reconcile:判 failed + 落 final 墓碑 + 一条 evFailed(cross_chain_orphan)。
	reconcileCrossChains(root, []*Task{a}, map[string]bool{})
	a1, _ := loadTask(root, a.ID)
	if a1.Status != statusFailed {
		t.Fatalf("首轮 reconcile 应判 failed,got %s", a1.Status)
	}

	// 人工 retry:期望墓碑被清,状态转 queued。
	if err := cmdSetStatus([]string{"-root", root, a.ID}, "retry"); err != nil {
		t.Fatal(err)
	}
	a2, _ := loadTask(root, a.ID)
	if a2.Status != statusQueued {
		t.Fatalf("retry 应转 queued,got %s", a2.Status)
	}

	// 模拟"复活后再次成为孤儿":把状态设回 done,再触发 reconcile。
	a2.Status = statusDone
	if err := saveTask(root, a2); err != nil {
		t.Fatal(err)
	}
	reconcileCrossChains(root, []*Task{a2}, map[string]bool{})
	a3, _ := loadTask(root, a.ID)
	if a3.Status != statusFailed {
		t.Fatalf("复活后再次孤儿,reconcile 应能重新判 failed(而非被 final 挡住),got %s", a3.Status)
	}

	events := readAllEventsRaw(t, root, a.ID)
	orphanCnt := 0
	heldCnt := 0
	for _, ev := range events {
		if ev.Type == evFailed {
			if r, _ := ev.Detail["reason"].(string); r == "cross_chain_orphan" {
				orphanCnt++
			}
		}
		if ev.Type == evHeld && ev.Actor == "runner:reconcile-tombstone" {
			heldCnt++
		}
	}
	// 端到端契约:两轮孤儿裁决 = 两条 cross_chain_orphan 事件(而非被 final 挡成 1 条+1 条 held)。
	if orphanCnt != 2 {
		t.Fatalf("retry 复活后再次判孤儿应恰 2 条 cross_chain_orphan(P1-1a 让 retry 清 final、P1-1b 备而未用),got %d 序列=%v", orphanCnt, eventTypes(events))
	}
	if heldCnt != 0 {
		t.Fatalf("端到端不应触发 tombstone-held(retry 已把墓碑清干净),got %d", heldCnt)
	}
}

// ---------- Round-2 修复回红反例 (P1-1 CLI-vs-runner 同 kind 并发写) ----------

// TestInjectAtMostOnceFinalYieldsToConcurrentReset (R2 P1-1 核心反例)
// 证伪场景直接落地:模拟 CLI 侧 resetTombstoneKind 在 inject 回调运行到一半时执行——阶段 3 重读
// 应发现条目已被删除,放弃 final 重建,让 reset 语义(重新起 bound=2)保持有效。
//
// 【为什么这条测试关键】审查证伪场景明写:自动化 ops 监听 saveTask(failed) 即 claudego retry,
// reset 恰落在阶段 1 的 pending 写与阶段 3 的 final 回写之间;R1 的 final 回写走
// "final.Attempt<newAttempt 分支以 final(attempt=newAttempt) 重建条目",把 reset 静默覆盖 →
// retry 承诺的"清墓碑重新起 bound=2 自动再裁决"被作废. 本测试用 inject 回调内嵌 reset 精确复现
// 该窗口——回退修复(去掉 final 回写侧的 !exists 分支)本测试直接报红。
//
// 【反例】把 injectAtMostOnce 阶段 3 的 "if !exists { return }" 删掉、回到 R1 的"attempt<newAttempt
// 重建"路径,断言"reset 后墓碑清空"直接红——final 会以 attempt=newAttempt 重建条目.
func TestInjectAtMostOnceFinalYieldsToConcurrentReset(t *testing.T) {
	root := mkTombRoot(t)
	calls := 0
	inject := func() error {
		calls++
		// 模拟"CLI 侧 resetTombstoneKind 在阶段 2 无锁窗口内执行":inject 回调内直接调 reset,
		// 与生产 ops 脚本"监听到卡转 failed 立即 claudego retry"的时序等价 (阶段 1 已释锁,
		// reset 可拿到锁; inject 结束后阶段 3 重取锁重读).
		if err := resetTombstoneKind(root, "task-race", reconcileCrossKind()); err != nil {
			return err
		}
		return nil
	}
	skipped, corrupted, err := injectAtMostOnce(root, "task-race", reconcileCrossKind(), inject)
	if err != nil || skipped || corrupted {
		t.Fatalf("首轮应正常跑(inject 内 reset 应被吸收): skipped=%v corrupted=%v err=%v", skipped, corrupted, err)
	}
	if calls != 1 {
		t.Fatalf("inject 应被调 1 次,got %d", calls)
	}
	// 核心断言:reset 后墓碑应清空. 若 final 回写重建了条目,该断言直接红——正是 R1 bug 的样貌.
	j, corrupted, err := readTombstoneJournal(root, "task-race")
	if err != nil || corrupted {
		t.Fatalf("读账本失败: corrupted=%v err=%v", corrupted, err)
	}
	if entry, exists := j.Entries[reconcileCrossKind()]; exists {
		t.Fatalf("并发 reset 后 final 不应重建条目(否则 retry 承诺的清墓碑被作废),got %+v", entry)
	}
	// 二次防御:reset 胜出后,下一轮 injectAtMostOnce 应能重新起 bound=2 (证明 reset 语义完整).
	next := 0
	skipped2, _, err2 := injectAtMostOnce(root, "task-race", reconcileCrossKind(), func() error { next++; return nil })
	if err2 != nil || skipped2 {
		t.Fatalf("reset 胜出后下一轮应能新开 bound: skipped=%v err=%v", skipped2, err2)
	}
	if next != 1 {
		t.Fatalf("下一轮 inject 应被调 1 次(bound 从零起算),got %d", next)
	}
}

// TestInjectAtMostOnceFinalYieldsOnNonceMismatch (R2 P1-1 防御纵深:nonce 校验)
// 独立于 entry-gone 分支:若在阶段 2 窗口内另一写者写了 pending (nonce 不同), 阶段 3 的 final 不
// 应覆盖别人的 pending. 【场景】reset+新 injectAtMostOnce 组合 (retry+紧接的下一轮 tick 再撞孤儿)
// 就会构造这种局面: 我们的 nonce 已过期, 应把 final 让给对方那一轮承接.
//
// 【反例】把 injectAtMostOnce 阶段 3 的 "if existing.Nonce != pendingNonce { return }" 删掉,
// final 会以我们的 nonce/phase 覆盖对方 pending, 对方后续的 final 认领会因 nonce 不匹配退让 →
// 双方结局都被吞, "别人的 pending 被我们的 final 静默替换"的骗审现场重现. 本测试断言"nonce
// 不匹配时不写 final", 回退即报红.
func TestInjectAtMostOnceFinalYieldsOnNonceMismatch(t *testing.T) {
	root := mkTombRoot(t)
	otherInjectCalled := false
	inject := func() error {
		// 模拟阶段 2 窗口内: 先 reset (清我们的 pending), 再有另一次 injectAtMostOnce 写了新 pending.
		if err := resetTombstoneKind(root, "task-nonce", reconcileCrossKind()); err != nil {
			return err
		}
		// 另一次 injectAtMostOnce 是同步的独立调用, 直接嵌套即可: nonce 会不同 (time.Now 递增).
		_, _, _ = injectAtMostOnce(root, "task-nonce", reconcileCrossKind(), func() error {
			otherInjectCalled = true
			return fmt.Errorf("模拟对方的 inject 崩溃, 留 pending 在盘上")
		})
		return nil
	}
	_, _, err := injectAtMostOnce(root, "task-nonce", reconcileCrossKind(), inject)
	if err != nil {
		t.Fatalf("外层 inject 应吸收内层错误(内层是嵌套调用): err=%v", err)
	}
	if !otherInjectCalled {
		t.Fatal("内层 inject 应被调用一次")
	}
	// 关键断言: 账本应停在"对方的 pending", 不该被我们的 final 覆盖成 phase=final.
	j, _, err := readTombstoneJournal(root, "task-nonce")
	if err != nil {
		t.Fatal(err)
	}
	entry, exists := j.Entries[reconcileCrossKind()]
	if !exists {
		t.Fatal("对方 pending 应保留在账本, got 条目已被清")
	}
	if entry.Phase != tombstonePhasePending {
		t.Fatalf("nonce 不匹配时我们不能把对方 pending 升级 final, got phase=%q", entry.Phase)
	}
	if entry.Attempt != 1 {
		t.Fatalf("对方 pending 应保持 attempt=1 (reset 后第一次), got %d", entry.Attempt)
	}
}

// TestCmdRetryResetVsInjectAtMostOnceIsSerialized (R2 P1-1 端到端串行化)
// 生产语义端到端: 在 injectAtMostOnce 的 inject 回调里模拟 CLI 侧 cmdSetStatus("retry") 调用,
// 应能观察到 CLI 侧墓碑清空 + 阶段 3 让位, 卡状态由 retry 说了算 (queued).
//
// 【反例】任何一处锁/让位分支被回退, 本测试要么断言"墓碑清空"报红 (final 重建了条目),
// 要么阶段 3 竞态死锁 (锁未释放). 是对锁/让位组合修复的端到端守卫.
func TestCmdRetryResetVsInjectAtMostOnceIsSerialized(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	a := newTask(root, cfg, typeCrossCheck, "R2 P1-1 端到端", "/tmp", []string{"p"}, 5)
	a.XRole = "A"
	a.XKey = "xkey-r2-race"
	a.Status = statusFailed
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}
	// 造前置 final: 模拟 reconcile 上一轮判 failed 已落 final (bound 用尽的等价前置状态: 已 final).
	// 本测试的重点不是 bound 触发, 而是"阶段 2 无锁窗口里 CLI retry reset 与阶段 3 final 争夺".
	// 用一次成功的 inject 制备真实 final. 结束后手工把 phase 复位 pending(1), 让下一次 injectAtMostOnce
	// 会走 inject 分支 (而非 final 直接跳过).
	_, _, _ = injectAtMostOnce(root, a.ID, reconcileCrossKind(), func() error { return nil })
	j0, _, _ := readTombstoneJournal(root, a.ID)
	e0 := j0.Entries[reconcileCrossKind()]
	e0.Phase = tombstonePhasePending
	e0.Attempt = 1
	j0.Entries[reconcileCrossKind()] = e0
	if err := writeTombstoneJournal(root, a.ID, j0); err != nil {
		t.Fatal(err)
	}
	// 本轮 injectAtMostOnce: 内嵌 CLI retry 调用 (会调 resetTombstoneKind).
	_, _, err := injectAtMostOnce(root, a.ID, reconcileCrossKind(), func() error {
		// 内嵌 cmdSetStatus retry: 走 CLI 全路径, 覆盖 main.go:1446 的 resetTombstoneKind.
		return cmdSetStatus([]string{"-root", root, a.ID}, "retry")
	})
	if err != nil {
		t.Fatalf("外层 inject 应吸收 retry: err=%v", err)
	}
	// 端到端断言 1: retry 已 reset, 墓碑清空 (不应有 final 重建).
	j, _, err := readTombstoneJournal(root, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry, exists := j.Entries[reconcileCrossKind()]; exists {
		t.Fatalf("retry reset 后墓碑不应有 reconcile:cross 条目 (final 让位失败): %+v", entry)
	}
	// 端到端断言 2: 卡态由 retry 说了算 (queued).
	a1, _ := loadTask(root, a.ID)
	if a1.Status != statusQueued {
		t.Fatalf("retry 应把状态转 queued, got %s", a1.Status)
	}
}

// TestArchiveTaskTombstonesSerializesAgainstWriters (R2 类闭合: archive 侧同类竞态)
// 归档也是墓碑文件的写者 (os.Rename src→dst). 若不上锁, 与并发 injectAtMostOnce 阶段 3/
// resetTombstoneKind 竞态: archive 恰在阶段 3 写 final 前把 src 搬走, writeTombstoneJournal 会
// 新建一个只有 final 的活动墓碑, 与归档文件并列存在 → 后续读走活动路径看到"只剩一条 final"的
// 骗审现场 (原始 attempt 历史被吞).
//
// 【反例】把 archiveTaskTombstones 里的锁去掉, 本测试通过嵌套 archive 调用触发竞态 → 断言
// "活动路径应不再存在"报红 (会存在一个只带 final 的新建活动文件).
func TestArchiveTaskTombstonesSerializesAgainstWriters(t *testing.T) {
	root := mkTombRoot(t)
	inject := func() error {
		// 阶段 2 窗口内 archive: 拿墓碑锁 → rename src→dst → 释锁.
		return archiveTaskTombstones(root, "task-arch-race")
	}
	_, _, err := injectAtMostOnce(root, "task-arch-race", reconcileCrossKind(), inject)
	if err != nil {
		t.Fatalf("inject 内 archive 应正常 (锁串行化): err=%v", err)
	}
	// 关键断言: 活动路径应不存在 (归档已搬走; 阶段 3 的 final 若无 nonce/entry 保护会新建活动).
	if _, err := os.Stat(tombstonePath(root, "task-arch-race")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive 后活动路径应不存在, stat err=%v", err)
	}
	// 归档路径应存在.
	if _, err := os.Stat(archivedTombstonePath(root, "task-arch-race")); err != nil {
		t.Fatalf("归档路径应存在: %v", err)
	}
}

// TestArchiveTaskTombstonesBlocksOnLock (R2 类闭合: archive 侧真正持锁的证据)
// 直接持锁 → 启协程调 archive → 断言 archive 被阻塞. 若 archiveTaskTombstones 无锁, 会立即完成
// (test 报红). 补 TestArchiveTaskTombstonesSerializesAgainstWriters 的不足: 该测试用 injectAtMostOnce
// 内嵌 archive 的模式, 阶段 3 的 entry-gone 分支即可兜住, 不能证明 archive 侧的锁本身是必需的.
// 本测试直接证伪"没有 archive 锁"这一假设——archive 若无锁则不等待, 立即完成 rename.
//
// 【反例】把 archiveTaskTombstones 里的锁去掉, 断言"持锁期间 archive 不完成"立即报红.
func TestArchiveTaskTombstonesBlocksOnLock(t *testing.T) {
	root := mkTombRoot(t)
	if err := os.MkdirAll(tombstonesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tombstonePath(root, "arch-block"), []byte(`{"version":1,"entries":{"resume:0":{"kind":"resume:0","attempt":1,"phase":"final","nonce":1,"ts":"x"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// 手工持锁, 观察 archive 是否等待. 注意: 手动 acquire 只拿了文件锁, 未拿进程内 mutex ——
	// archive 里 mu.Lock() 会与 mutex 无冲突 (mu 空闲) 然后卡在 acquireTombstoneLock (文件锁被占).
	release, err := acquireTombstoneLock(root, "arch-block")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = archiveTaskTombstones(root, "arch-block")
		close(done)
	}()
	// 期望 archive 被文件锁挡住: 60ms 内不完成.
	select {
	case <-done:
		t.Fatal("archive 应在持锁期间阻塞 (若立即完成说明 archive 无锁)")
	case <-time.After(60 * time.Millisecond):
		// pass: archive 被挡住
	}
	release()
	// 释锁后 archive 应能在自旋周期 (5ms) 内完成. 500ms 给 CI 充足余量.
	select {
	case <-done:
		// pass
	case <-time.After(500 * time.Millisecond):
		t.Fatal("释锁后 archive 应能在自旋周期内完成, 500ms 内未见完成")
	}
	if _, err := os.Stat(archivedTombstonePath(root, "arch-block")); err != nil {
		t.Fatalf("archive 应成功搬到归档路径: %v", err)
	}
	if _, err := os.Stat(tombstonePath(root, "arch-block")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("活动路径应被搬走: %v", err)
	}
}

// TestResetTombstoneKindBlocksOnLock (R2 类闭合: reset 侧真正持锁的证据)
// 同样验证 reset 侧的锁存在——持锁期间 reset 应等待. 若 reset 无锁, IO 竞态可让 reset 读到 inject
// 阶段 1 刚写的 pending, 但写回 delete 时把 pending 之后的其他 kind 变更覆盖.
//
// 【反例】把 resetTombstoneKind 里的锁去掉, 本测试立即报红——reset 不等待, 立即完成.
func TestResetTombstoneKindBlocksOnLock(t *testing.T) {
	root := mkTombRoot(t)
	if err := os.MkdirAll(tombstonesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tombstonePath(root, "reset-block"), []byte(`{"version":1,"entries":{"resume:0":{"kind":"resume:0","attempt":1,"phase":"final","nonce":1,"ts":"x"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := acquireTombstoneLock(root, "reset-block")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = resetTombstoneKind(root, "reset-block", "resume:0")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("reset 应在持锁期间阻塞 (若立即完成说明 reset 无锁)")
	case <-time.After(60 * time.Millisecond):
		// pass
	}
	release()
	select {
	case <-done:
		// pass
	case <-time.After(500 * time.Millisecond):
		t.Fatal("释锁后 reset 应能在自旋周期内完成")
	}
}

// TestInjectAtMostOnceBlocksOnLockPhase1 (R2 类闭合: injectAtMostOnce 阶段 1 持锁的证据)
// 验证 injectAtMostOnce 阶段 1 (pending 写) 也被锁保护, 而不是仅阶段 3 的 final 回写.
// 若阶段 1 无锁, 与 reset 并发时会撞车:
// reset 读 → inject 阶段 1 读 → reset 写 → inject 阶段 1 写 pending → reset 的 delete 被 pending 覆盖.
//
// 关键判据: 外部持锁期间, 墓碑账本文件不应出现 pending 条目 —— 阶段 1 的 write 必须被阻塞.
// 只测 goroutine 是否完成不够: 阶段 3 也持锁, 单看完成/阻塞会误把"阶段 3 挡住了"当成"阶段 1 挡住了".
//
// 【反例】把 injectAtMostOnce 阶段 1 的 acquireTombstoneLock 去掉 (仅留 mutex 或全去), 阶段 1 的
// pending 写会在外部持锁窗口内落盘, 本测试即报红.
func TestInjectAtMostOnceBlocksOnLockPhase1(t *testing.T) {
	root := mkTombRoot(t)
	if err := os.MkdirAll(tombstonesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := acquireTombstoneLock(root, "inject-block")
	if err != nil {
		t.Fatal(err)
	}
	// inject 用 slow callback: 若阶段 1 无锁, 无锁写 pending → 进入 inject 长睡 → 我们能在此窗口
	// 观察到 pending 文件. 若阶段 1 有锁, 写 pending 被阻塞, 观察窗口内墓碑文件不存在.
	injectStarted := make(chan struct{})
	injectRelease := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_, _, _ = injectAtMostOnce(root, "inject-block", "resume:0", func() error {
			close(injectStarted)
			<-injectRelease
			return nil
		})
		close(done)
	}()
	// 在外部持锁 100ms 期间反复观察: 墓碑账本文件不应出现 (阶段 1 被锁阻塞在 pending 写之前).
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(tombstonePath(root, "inject-block")); statErr == nil {
			t.Fatal("外部持锁期间墓碑账本不该被 inject 阶段 1 写入 (说明阶段 1 无锁: 已越过锁写了 pending)")
		}
		// inject callback 也不该被触发 (阶段 1 未过, 阶段 2 就不会开始).
		select {
		case <-injectStarted:
			t.Fatal("外部持锁期间 inject callback 不该被触发 (说明阶段 1 无锁: 已越过锁进入 inject 长回调)")
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()
	// 释锁后阶段 1 应能推进, 进入 inject callback.
	select {
	case <-injectStarted:
		// pass
	case <-time.After(500 * time.Millisecond):
		t.Fatal("释锁后阶段 1 应能推进到 inject callback, 500ms 内未见触发")
	}
	// 让 inject 回归, 收尾 goroutine.
	close(injectRelease)
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("inject callback 返回后 injectAtMostOnce 应完成, 500ms 内未见退出")
	}
}

// TestAcquireTombstoneLockExcludesConcurrent (R2 类闭合: 锁本身的互斥语义)
// 直接验证锁的 exclusive 语义: 同一 task 的第一次 acquire 拿到锁后, 第二次并发 acquire 必须等待
// 直到第一次 release. 是"tmp+os.Link+staleEventLock+PID 校验"组合正确性的最小验证.
//
// 【反例】任何一处组件 (Link 变 O_EXCL / stale 判据放宽 / release 不核 PID) 回退, 都会打破
// exclusive 语义 → 本测试会看到并发 acquire 立即成功而非等待.
func TestAcquireTombstoneLockExcludesConcurrent(t *testing.T) {
	root := mkTombRoot(t)
	if err := os.MkdirAll(tombstonesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	release1, err := acquireTombstoneLock(root, "lock-excl")
	if err != nil {
		t.Fatal(err)
	}
	// 第二次 acquire 应等待: 用一个短窗口内的并发调用观察其是否立即成功.
	acquired2 := make(chan time.Time, 1)
	go func() {
		release2, err := acquireTombstoneLock(root, "lock-excl")
		if err != nil {
			return
		}
		acquired2 <- time.Now()
		release2()
	}()
	// 持锁一小段时间, 观察并发 acquire 是否被阻塞.
	holdUntil := time.Now().Add(80 * time.Millisecond)
	select {
	case at := <-acquired2:
		t.Fatalf("第二次 acquire 不应在持锁期间成功, got at=%v (hold until %v)", at, holdUntil)
	case <-time.After(60 * time.Millisecond):
		// 期望: 60ms 内第二次 acquire 未成功.
	}
	release1()
	// 释锁后, 第二次 acquire 应能在自旋周期 (5ms) 内成功.
	select {
	case <-acquired2:
		// pass
	case <-time.After(500 * time.Millisecond):
		t.Fatal("释锁后第二次 acquire 应能在自旋周期内拿到, 500ms 内未见完成")
	}
}

// TestReleaseTombstoneLockChecksPID (R2 类闭合: release 侧 PID 校验)
// 若 release 无脑 Remove, 系统睡眠/挂起跨 5s TTL 唤醒后, 原持有者会误删强夺者的新锁 → 双持锁.
// 手工制造"锁文件属于另一 PID"的场景, 断言 releaseTombstoneLock 不删.
func TestReleaseTombstoneLockChecksPID(t *testing.T) {
	root := mkTombRoot(t)
	if err := os.MkdirAll(tombstonesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	path := tombstoneLockPath(root, "pid-check")
	// 造一份属于"其他 PID"的锁文件.
	info, _ := json.Marshal(lockInfo{PID: os.Getpid() + 99999, At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(path, info, 0o644); err != nil {
		t.Fatal(err)
	}
	releaseTombstoneLock(path)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		t.Fatal("PID 不匹配的锁不应被删除, 会误删强夺者新锁")
	}
	_ = os.Remove(path)
}

// ---------- Round-3 修复回红反例 (P1-1 emit 必须先于 saveTask) ----------

// TestReconcileSkippedHeldEmitsBeforeSave (R3 P1-1 核心反例)
// 证伪场景直接落地: reconcileCrossChains 的 skipped→held 分支若 saveTask 先于 emitTaskEvent,
// 崩溃/IO 错落在两者之间时 saveTask 已落盘 held → 孤儿谓词 status==done 永久排除该卡 →
// skipped 分支永不重入 → evHeld 披露事件永久丢失且无补发路径, 账本呈现 done→held 零事件跳变.
// 正是本轮宣称消灭的"零披露"缺陷类——与 runTask resume 侧 (runner.go:688-691 emit 先 save 后,
// 崩溃可自愈) 顺序相反的 R2 遗留.
//
// 【为什么用 saveTask 失败代理崩溃】真 kill -9 在单进程测试中无法复现; 从"evHeld 事件生死"
// 视角, "save 成功但 emit 前崩溃" 与 "save 失败 continue 掉 emit" 造成的账本结果完全等价——
// 都是"卡态可能变但 evHeld 永久缺失". 用 taskPath 建成目录让 atomicWrite 的 rename 失败,
// 精确复现 R2 顺序下的账本静默.
//
// 【反例】把 runner.go 附近的 emitTaskEvent 挪回 saveTask 之后 (回到 R2 顺序), saveTask 失败
// continue 直接吞掉 emit, 事件账本无 evHeld → 本测试断言报红.
func TestReconcileSkippedHeldEmitsBeforeSave(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	a := newTask(root, cfg, typeCrossCheck, "R3 P1-1 held emit-first", "/tmp", []string{"p"}, 5)
	a.XRole = "A"
	a.XKey = "xkey-r3-held-1"
	a.Status = statusDone
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}
	// 预置 reconcile:cross final 墓碑——skipped 分支必命中 (final 挡住 inject).
	_, _, _ = injectAtMostOnce(root, a.ID, reconcileCrossKind(), func() error { return nil })
	preJ, _, _ := readTombstoneJournal(root, a.ID)
	if preJ.Entries[reconcileCrossKind()].Phase != tombstonePhaseFinal {
		t.Fatalf("前置应 reconcile:cross final, got %+v", preJ.Entries[reconcileCrossKind()])
	}
	// 把任务文件替换为目录, 让 saveTask 的 atomicWrite rename 失败——精确代理"崩溃在 save 后 emit 前".
	tp := taskPath(root, a.ID)
	if err := os.Remove(tp); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tp, 0o755); err != nil {
		t.Fatal(err)
	}
	// 触发 reconcile: skipped=true → 进 held 分支 → 新顺序应先 emit, 后 saveTask (失败 continue).
	reconcileCrossChains(root, []*Task{a}, map[string]bool{})
	// 关键断言: 即便 saveTask 失败, evHeld 已在事件账本. 若代码回退 R2 顺序 (save 先 emit 后),
	// save 失败 continue 掉 emit 调用, 账本无 evHeld → 断言直接红.
	events := readAllEventsRaw(t, root, a.ID)
	sawHeld := false
	for _, ev := range events {
		if ev.Type == evHeld && ev.Actor == "runner:reconcile-tombstone" {
			sawHeld = true
			if reason, _ := ev.Detail["reason"].(string); reason != "reconcile_cross_tombstone_exhausted" {
				t.Fatalf("held.reason 应 reconcile_cross_tombstone_exhausted, got %q", reason)
			}
			if kind, _ := ev.Detail["kind"].(string); kind != reconcileCrossKind() {
				t.Fatalf("held.kind 应 %s, got %q", reconcileCrossKind(), kind)
			}
		}
	}
	if !sawHeld {
		t.Fatalf("emit 必须先于 saveTask, 使 save 失败也不吞事件, got 事件序列=%v", eventTypes(events))
	}
}

// TestReconcileHeldDedupesOnPersistentSaveFailure (CG-R1 修复反例:emit 前账本去重)
// 证伪场景直落: reconcile 的 skipped→held 分支若不做 emit 前去重, saveTask 持续失败(磁盘只读/
// 卡文件被替成目录等)每 tick 都会重入并再 emit 一条 evHeld——一天 288 条淹活动流. 首次 emit
// 是"事件重复优于永久缺失"的必要代价 (见 TestReconcileSkippedHeldEmitsBeforeSave 依然绿);
// 之后重入应识别账本末条同因 evHeld 跳过 emit.
//
// 【为什么 saveTask 用替目录代理磁盘只读】跨 CI 平台 chmod 0o444 不总生效(macOS/Linux 差异),
// 也可能被 root 绕过. 把 taskPath 替成目录让 atomicWrite 的 rename 稳定失败, 精确复现"卡态
// 每次都写不下"的持续崩溃场景.
//
// 【反例】去掉 runner.go skipped 分支里的 loadTaskEvents 去重判据(直接 emit), 本测试会看到
// 两条 evHeld → "恰 1 条" 断言直接报红.
func TestReconcileHeldDedupesOnPersistentSaveFailure(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	a := newTask(root, cfg, typeCrossCheck, "CG-R1 held dedupe", "/tmp", []string{"p"}, 5)
	a.XRole = "A"
	a.XKey = "xkey-cgr1-held-dedupe"
	a.Status = statusDone
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}
	// 预置 reconcile:cross final 墓碑——skipped 分支必命中 (final 挡住 inject).
	_, _, _ = injectAtMostOnce(root, a.ID, reconcileCrossKind(), func() error { return nil })
	// 把任务文件替换为目录, 让 saveTask 的 atomicWrite rename 稳定失败——每次 reconcile 都触发
	// "save 失败 continue" 路径, 精确复现持续 saveTask 崩溃.
	tp := taskPath(root, a.ID)
	if err := os.Remove(tp); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tp, 0o755); err != nil {
		t.Fatal(err)
	}
	// 连调两次 reconcileCrossChains——第一次 emit(账本无同因末条), 第二次去重跳过 emit.
	reconcileCrossChains(root, []*Task{a}, map[string]bool{})
	reconcileCrossChains(root, []*Task{a}, map[string]bool{})
	events := readAllEventsRaw(t, root, a.ID)
	held := 0
	for _, ev := range events {
		if ev.Type == evHeld && ev.Actor == "runner:reconcile-tombstone" {
			if reason, _ := ev.Detail["reason"].(string); reason == "reconcile_cross_tombstone_exhausted" {
				held++
			}
		}
	}
	if held != 1 {
		t.Fatalf("持续 saveTask 失败下两次 reconcile 应仅留 1 条 evHeld(去重), got %d, 事件序列=%v", held, eventTypes(events))
	}
}

// TestReconcileFailedEmitsBeforeSave (R3 P1-1 类闭合反例: reconcile 内 injectAtMostOnce
// 闭包侧的 failed 分支同样要求 emit 先于 save; 闭合审查 P2-2 提示的 pre-existing 同源缺陷)
// 若闭包内 save 先于 emit, 崩溃/IO 错落两者之间时——pending 已 +1, saveTask(failed) 已落盘,
// 但 evFailed 永久丢失 (孤儿谓词 status==done 永久排除已 failed 卡, 无补发路径). 与本轮 P1-1
// held 分支同构; 一并按类闭合. 用 saveTask 失败代理"崩溃在 save 后 emit 前".
//
// 【反例】把 runner.go 闭包内 emit 挪回 saveTask 之后 (R2 顺序), saveTask 失败 return err 前
// emit 没机会调用, 事件账本无 evFailed → 本测试断言报红.
func TestReconcileFailedEmitsBeforeSave(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	a := newTask(root, cfg, typeCrossCheck, "R3 P1-1 failed emit-first", "/tmp", []string{"p"}, 5)
	a.XRole = "A"
	a.XKey = "xkey-r3-fail-1"
	a.Status = statusDone
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}
	// 无前置墓碑 → 闭包会真的走 inject: 修 t.Status=failed → emit evFailed → saveTask 失败 return err.
	tp := taskPath(root, a.ID)
	if err := os.Remove(tp); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tp, 0o755); err != nil {
		t.Fatal(err)
	}
	reconcileCrossChains(root, []*Task{a}, map[string]bool{})
	// 关键断言: 即便 saveTask 失败, evFailed 已在事件账本 (证明 emit 先于 save 落).
	events := readAllEventsRaw(t, root, a.ID)
	sawFailed := false
	for _, ev := range events {
		if ev.Type == evFailed {
			if r, _ := ev.Detail["reason"].(string); r == "cross_chain_orphan" {
				sawFailed = true
			}
		}
	}
	if !sawFailed {
		t.Fatalf("emit 必须先于 saveTask, 使 save 失败也不吞 evFailed, got 事件序列=%v", eventTypes(events))
	}
	// 二次防御: pending 已 +1 (证明 inject 被真的调过, 不是被墓碑挡在门外).
	// saveTask 失败 → inject 返回 err → 阶段 3 未触发 → 墓碑停 pending(1), 未落 final.
	j, _, err := readTombstoneJournal(root, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := j.Entries[reconcileCrossKind()]
	if e.Phase != tombstonePhasePending || e.Attempt != 1 {
		t.Fatalf("saveTask 失败应保留 pending 不落 final, got %+v", e)
	}
}

// TestResumeHeldSourceOrder (R3 P1-1 类闭合: resume 侧源码顺序静态守卫)
// resume 侧 (runner.go:685-691) 从 R1 起就是正确顺序 (emit 先, save 后); 单元/集成测试要跑到
// 那段分支需要真起 fakeClaude+完整 runTask 环境, 成本高且脆弱. 更稳的守卫是: 读源码文件, 断言
// resume held 分支里 emitTaskEvent 出现在 return saveTask 之前——保证未来任何一次重构不留神
// 反转顺序会被本测试即刻抓住.
//
// 【反例】把 runner.go resume 侧的 emitTaskEvent 挪到 return saveTask 之后, 或者删掉 emit 直接
// return saveTask, 本测试立即报红.
func TestResumeHeldSourceOrder(t *testing.T) {
	data, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	// resume 侧 held 分支的 emit actor "runner:tombstone" 全仓唯一 (grep 已验证):
	// reconcile 侧用 "runner:reconcile-tombstone", 不冲突.
	anchor := `evHeld, "runner:tombstone"`
	idx := strings.Index(src, anchor)
	if idx < 0 {
		t.Fatalf("找不到 resume 侧 held emit 锚点 %q", anchor)
	}
	// 从 emit 之后往后扫首个 `return saveTask(root, t)` —— 属于同一段代码.
	tail := src[idx:]
	saveIdx := strings.Index(tail, "return saveTask(root, t)")
	if saveIdx < 0 {
		t.Fatalf("resume 侧 held emit 之后应有 return saveTask(root, t)")
	}
	// 关键断言: emit 与 return saveTask 之间不应再有 saveTask 调用——防"emit 在两次 save 之间"
	// 的奇葩变体; 更本质是防 emit 被挪到 saveTask 之后 (那样 saveIdx 会指向 emit 之前另一处 save).
	between := tail[:saveIdx]
	if strings.Contains(between, "saveTask(root, t)") {
		t.Fatalf("resume 侧 held 分支 emit 与 return saveTask 之间不应再有 saveTask, got between=%q", between)
	}
	// 二次防御: emit 锚点之后 300 字符内应命中 return saveTask —— 若代码回退把 emit 挪去分支
	// 尾部 (emit 与 return saveTask 距离拉远/顺序颠倒), 此距离会显著变大或找不到.
	if saveIdx > 300 {
		t.Fatalf("resume 侧 held emit 与 return saveTask 距离异常 (%d 字符), 疑似顺序被挪, got tail head=%q", saveIdx, tail[:min(300, len(tail))])
	}
}
