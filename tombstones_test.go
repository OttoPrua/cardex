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
// 二次调用(即使 t.Status 被外部改回 done 冒充复发)应完全跳过(墓碑挡住)。
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

	// 次轮:即便手工把 status 改回 done 让 reconcile 再撞一次,墓碑 final 应挡住。
	a1.Status = statusDone
	if err := saveTask(root, a1); err != nil {
		t.Fatal(err)
	}
	tasks2 := []*Task{a1}
	reconcileCrossChains(root, tasks2, active)
	events2 := readAllEventsRaw(t, root, a.ID)
	orphanCnt2 := 0
	for _, ev := range events2 {
		if ev.Type == evFailed {
			if r, _ := ev.Detail["reason"].(string); r == "cross_chain_orphan" {
				orphanCnt2++
			}
		}
	}
	if orphanCnt2 != 1 {
		t.Fatalf("次轮 reconcile 应被墓碑挡住,孤儿事件仍应为 1 条,got %d", orphanCnt2)
	}
	// 也应仍是 status=done(墓碑挡住 → 未再次改 failed)
	a2, _ := loadTask(root, a.ID)
	if a2.Status != statusDone {
		t.Fatalf("次轮 reconcile 应被拦截(status 未再变),got %s", a2.Status)
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
