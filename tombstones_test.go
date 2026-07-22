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
