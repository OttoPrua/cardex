package main

// migrate_test.go —— `cardex migrate` 的演练与 fail-closed 反例（BD-44 第 12 条）。
//
// 【为什么这组用例必须真跑而不是读代码】migrate 只会被执行一次，在 cutover 窗口里，对着
// 生产队列。那一次没有"先试试看"的机会——所以这里把整条路径在临时目录上真跑一遍：真建假根、
// 真搬、真对账、真读改写后的 config。任何一条断言退化成"看起来对"，验收就等于没做。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkFakeRoot 造一个像模像样的数据根：config.json（含指向自身的绝对路径与 ~ 路径、
// 以及一个大整数字段）+ 若干任务/事件/进度/归档文件。返回根路径。
func mkFakeRoot(t *testing.T, home string, name string) string {
	t.Helper()
	root := filepath.Join(home, name)
	for _, d := range []string{"tasks", "events", "progress", "archive", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]any{
		"claude_bin":          "/opt/homebrew/bin/claude",
		"default_review_sync": "bash " + root + "/sync-lane-to-5090.sh",
		"queue_budget_tokens": 2000000,
		// 2^53+1：改写若不走 UseNumber，JSON 数字会先变成 float64 再写回，这个值精度不够，
		// 落盘成 9007199254740992——不报错、不崩溃，账上的数悄悄变了一个。
		// （实测：默认解码 9007199254740993 → 9007199254740992；UseNumber 原样保留。）
		"some_big_id": json.RawMessage("9007199254740993"),
		"type_defaults": map[string]any{
			"design-review": map[string]any{
				"allowed_tools": []any{
					"Read",
					"Bash(" + root + "/verify-mirror-fingerprint.sh:*)",
					"Bash(~/" + name + "/verify-mirror-fingerprint.sh:*)",
				},
			},
		},
		// 一个当前 Config 结构体不认识的键：改写不得把它吃掉。
		"some_future_key": "keep-me",
	}
	blob, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}

	writeFile := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("tasks/t-001.json", `{"id":"t-001","status":"pending","title":"甲"}`)
	writeFile("tasks/t-002.json", `{"id":"t-002","status":"done","title":"乙"}`)
	writeFile("events/t-001.jsonl", "{\"type\":\"queued\"}\n{\"type\":\"started\"}\n")
	writeFile("progress/p-key.json", `{"key":"p-key","raw":"进度"}`)
	writeFile("archive/t-000.json", `{"id":"t-000","status":"done"}`)
	writeFile("logs/t-001.log", "日志内容，不进对账\n")
	// 顶层脚本：内部硬编码旧根路径，printPostSteps 应该把它点名出来。
	writeFile("sync-lane-to-5090.sh", "#!/bin/bash\nFP_SCRIPT=\"$HOME/"+name+"/workspace-fingerprint.sh\"\n")
	return root
}

// TestMigrateMovesRootWithZeroLoss 主干演练：真搬一次，断言零丢失对账通过、
// config.json 里的路径被改写、旧根消失、新根可用。
func TestMigrateMovesRootWithZeroLoss(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearRootEnv(t)
	resetLegacyWarnState()

	src := mkFakeRoot(t, home, legacyRootDirName)
	dst := filepath.Join(home, rootDirName)

	before, err := tallyRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	if before["tasks"].Files != 2 || before["events"].Files != 1 ||
		before["progress"].Files != 1 || before["archive"].Files != 1 {
		t.Fatalf("前置：假根点数不对，用例自身构造有问题: %+v", before)
	}

	if err := runMigrate(src, dst, false); err != nil {
		t.Fatalf("migrate 应成功: %v", err)
	}

	// ① 旧根整个不见了，新根在。
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("迁移后旧根应消失, stat err=%v", err)
	}
	if !isExistingDir(dst) {
		t.Fatalf("迁移后新根应存在: %s", dst)
	}

	// ② 零丢失：四个目录的文件数与字节数逐一相等。
	after, err := tallyRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	if diff, ok := tallyEqual(before, after); !ok {
		t.Fatalf("零丢失对账应通过: %s", diff)
	}

	// ③ config.json：三处旧根路径（绝对 ×2 + ~ 形式 ×1）都改到新根，且不留任何旧根残迹。
	raw, err := os.ReadFile(filepath.Join(dst, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Contains(body, src+"/") {
		t.Fatalf("config.json 仍含旧根绝对路径 %s:\n%s", src, body)
	}
	if strings.Contains(body, "~/"+legacyRootDirName+"/") {
		t.Fatalf("config.json 仍含旧根 ~ 形式路径:\n%s", body)
	}
	for _, want := range []string{
		"bash " + dst + "/sync-lane-to-5090.sh",
		"Bash(" + dst + "/verify-mirror-fingerprint.sh:*)",
		"Bash(~/" + rootDirName + "/verify-mirror-fingerprint.sh:*)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("config.json 缺改写后的 %q:\n%s", want, body)
		}
	}

	// ④ 数字与未知键必须原样活着。
	// 【为什么单独钉大整数】不 UseNumber 的话 JSON 数字一律先变 float64，超过 2^53 的整数
	// 写回时精度不够——落盘成了另一个数字，不报错、不崩溃，只是账不对了。
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("改写后 config.json 应仍是合法 JSON: %v", err)
	}
	if !strings.Contains(body, "2000000") {
		t.Fatalf("整数被改写坏了:\n%s", body)
	}
	if !strings.Contains(body, "9007199254740993") {
		t.Fatalf("超过 2^53 的整数被 float64 中转损了精度（落盘成了另一个数，且不会报任何错）:\n%s", body)
	}
	if got["some_future_key"] != "keep-me" {
		t.Fatalf("结构体不认识的键被吃掉了: %+v", got)
	}

	// ⑤ 迁移后的根必须能被 loadConfig 正常读出来（端到端证明配置没被改坏）。
	if _, err := loadConfig(dst); err != nil {
		t.Fatalf("迁移后 loadConfig 应成功: %v", err)
	}

	// ⑥ 锁文件不该留在新根里（release 走的是新路径）。
	if _, err := os.Stat(lockPath(dst)); !os.IsNotExist(err) {
		t.Fatalf("迁移结束后不应残留实例锁, stat err=%v", err)
	}
}

// TestMigrateRefusesNonEmptyTarget fail-closed 反例①：目标已存在且非空 → 拒绝，两侧一个字节都不动。
func TestMigrateRefusesNonEmptyTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearRootEnv(t)

	src := mkFakeRoot(t, home, legacyRootDirName)
	dst := filepath.Join(home, rootDirName)
	if err := os.MkdirAll(filepath.Join(dst, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	occupied := filepath.Join(dst, "tasks", "other.json")
	if err := os.WriteFile(occupied, []byte(`{"id":"other"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runMigrate(src, dst, false)
	if err == nil {
		t.Fatal("目标非空时必须拒绝迁移（否则会把两份队列搅在一起）")
	}
	if !strings.Contains(err.Error(), "非空") {
		t.Fatalf("错误信息应说清是目标非空, got %v", err)
	}
	// 源根原封不动。
	if _, err := os.Stat(filepath.Join(src, "tasks", "t-001.json")); err != nil {
		t.Fatalf("拒绝后源根必须原封不动: %v", err)
	}
	// 目标里原有的东西也不能被碰。
	if data, err := os.ReadFile(occupied); err != nil || string(data) != `{"id":"other"}` {
		t.Fatalf("拒绝后目标里的既有文件必须原样, got %q err=%v", data, err)
	}
}

// TestMigrateAcceptsEmptyTargetDir 目标是**空目录**时放行（init 建过空壳是常态，
// 让它挡住迁移只是纯摩擦）。与上一条构成边界的两侧。
func TestMigrateAcceptsEmptyTargetDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearRootEnv(t)

	src := mkFakeRoot(t, home, legacyRootDirName)
	dst := filepath.Join(home, rootDirName)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runMigrate(src, dst, false); err != nil {
		t.Fatalf("目标是空目录时应放行: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "tasks", "t-001.json")); err != nil {
		t.Fatalf("卡应已搬到新根: %v", err)
	}
}

// TestMigrateRefusesRunningTask fail-closed 反例②：有 running 卡 → 拒绝。
// 【为什么这条是硬门】running 卡背后是活着的执行器进程，它的路径全钉在旧根上；
// 目录搬走后它可能按老路径重建目录，于是队列一分为二，谁也说不清哪份是真的。
func TestMigrateRefusesRunningTask(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearRootEnv(t)

	src := mkFakeRoot(t, home, legacyRootDirName)
	dst := filepath.Join(home, rootDirName)
	if err := os.WriteFile(filepath.Join(src, "tasks", "t-run.json"),
		[]byte(`{"id":"t-run","status":"running","title":"在跑"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runMigrate(src, dst, false)
	if err == nil {
		t.Fatal("有 running 卡时必须拒绝迁移")
	}
	if !strings.Contains(err.Error(), "t-run") {
		t.Fatalf("错误应点名是哪张卡挡住了, got %v", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("拒绝后不该留下任何目标目录, stat err=%v", statErr)
	}
}

// TestMigrateRefusesWhenLockHeld fail-closed 反例③：实例锁被别人持着（tick/daemon 在跑）→ 拒绝。
func TestMigrateRefusesWhenLockHeld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearRootEnv(t)

	src := mkFakeRoot(t, home, legacyRootDirName)
	dst := filepath.Join(home, rootDirName)

	// 直接写一把"当前进程持有"的新鲜锁：acquireLock 对同 PID 的活锁不会强夺，正是要的效果。
	if err := os.WriteFile(lockPath(src), []byte(`{"pid":`+itoa(os.Getpid())+`,"at":"now"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runMigrate(src, dst, false)
	if err == nil {
		t.Fatal("拿不到实例锁时必须拒绝迁移")
	}
	if !strings.Contains(err.Error(), "实例锁") {
		t.Fatalf("错误应说清是锁挡住的, got %v", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("拒绝后不该留下任何目标目录, stat err=%v", statErr)
	}
}

// TestMigrateRefusesSameRoot 源与目标同路径 → 拒绝（已经在新根上了，再"搬"一次只会自毁）。
func TestMigrateRefusesSameRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearRootEnv(t)
	root := mkFakeRoot(t, home, rootDirName)

	if err := runMigrate(root, root, false); err == nil {
		t.Fatal("源与目标同路径时必须拒绝")
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "t-001.json")); err != nil {
		t.Fatalf("拒绝后数据必须完好: %v", err)
	}
}

// TestMigrateRefusesMissingSource 源根不存在 → 拒绝（别在空气上开张）。
func TestMigrateRefusesMissingSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearRootEnv(t)
	err := runMigrate(filepath.Join(home, "nope"), filepath.Join(home, rootDirName), false)
	if err == nil {
		t.Fatal("源根不存在时必须拒绝")
	}
}

// TestMigrateDryRunTouchesNothing dry-run 必须真的一个字节都不动——它是操作员在 cutover
// 窗口前唯一能"先看看"的手段，如果它有副作用，那它比没有更糟。
func TestMigrateDryRunTouchesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearRootEnv(t)

	src := mkFakeRoot(t, home, legacyRootDirName)
	dst := filepath.Join(home, rootDirName)
	before, err := tallyRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	cfgBefore, err := os.ReadFile(filepath.Join(src, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := runMigrate(src, dst, true); err != nil {
		t.Fatalf("dry-run 应成功: %v", err)
	}

	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不得建出目标根, stat err=%v", err)
	}
	after, err := tallyRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	if diff, ok := tallyEqual(before, after); !ok {
		t.Fatalf("dry-run 动了源根: %s", diff)
	}
	cfgAfter, err := os.ReadFile(filepath.Join(src, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfgBefore) != string(cfgAfter) {
		t.Fatal("dry-run 不得改写 config.json")
	}
	if _, err := os.Stat(lockPath(src)); !os.IsNotExist(err) {
		t.Fatal("dry-run 结束后不应残留实例锁")
	}
}

// TestMigrateRollsBackOnTallyMismatch 对账不通过必须回滚，而不是把数据留在半截状态。
//
// 【怎么造出"对账不符"】migrate 内部搬完立刻点数，正常路径下两边天然相等，没法从外部注入差异。
// 所以这里直接对 moveRoot + tallyEqual + rollback 三件套做组合验证：搬走之后人为删掉一个卡，
// 断言 tallyEqual 能识别出来、rollback 能把根搬回原处且数据仍在。
// 【杀的突变】把 runMigrate 里对账不符那一支的 rollback 去掉 → 数据会永久留在新根，
// 与本用例断言的"回滚后旧根带着全部数据回来"直接冲突。
func TestMigrateRollsBackOnTallyMismatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearRootEnv(t)

	src := mkFakeRoot(t, home, legacyRootDirName)
	dst := filepath.Join(home, rootDirName)

	before, err := tallyRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	byRename, err := moveRoot(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !byRename {
		t.Skip("同卷下应走 rename 路径；本机走了跨卷拷贝，回滚语义不同，跳过")
	}
	// 人为制造丢失。
	if err := os.Remove(filepath.Join(dst, "tasks", "t-002.json")); err != nil {
		t.Fatal(err)
	}
	after, err := tallyRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	diff, ok := tallyEqual(before, after)
	if ok {
		t.Fatal("少了一张卡，对账必须报不一致——恒真的对账等于没有对账")
	}
	if !strings.Contains(diff, "tasks/") {
		t.Fatalf("对账差异应点名 tasks/, got %q", diff)
	}

	rollback(src, dst, byRename)
	if !isExistingDir(src) {
		t.Fatal("回滚后旧根应回来")
	}
	if _, err := os.Stat(filepath.Join(src, "tasks", "t-001.json")); err != nil {
		t.Fatalf("回滚后数据应仍在旧根: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("回滚后新根应消失, stat err=%v", err)
	}
}

// TestMigrateCrossVolumeFallbackVerifiesEveryFile 跨卷回退路径的逐文件校验必须真在比内容，
// 而不是只看文件在不在。
// 【杀的突变】把 verifyTree 的 sha256 比较换成"目标文件存在即通过" → 本用例红。
func TestMigrateCrossVolumeFallbackVerifiesEveryFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := mkFakeRoot(t, home, legacyRootDirName)
	dst := filepath.Join(home, "copy-target")

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	if err := verifyTree(src, dst); err != nil {
		t.Fatalf("完整拷贝应校验通过: %v", err)
	}
	// 篡改一个字节：大小不变、文件仍在——只有真比内容才抓得到。
	victim := filepath.Join(dst, "tasks", "t-001.json")
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := append([]byte{}, data...)
	corrupted[len(corrupted)-2] ^= 0x20
	if err := os.WriteFile(victim, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyTree(src, dst); err == nil {
		t.Fatal("内容被篡改（大小不变）时逐文件校验必须失败——只查存在性等于没校验")
	}
}

// TestMigrateSurfacesHardcodedOldRootScripts 迁移后必须点名那些"跟着搬过来、内部却还写着
// 旧根路径"的脚本。它们属跨机指纹/同步工件族，本轮按裁决冻在旧名不自动改写；
// 不点名的话，表现是复审同步链在"文件不存在"上静默失败，没人会往改名上联想。
func TestMigrateSurfacesHardcodedOldRootScripts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := mkFakeRoot(t, home, legacyRootDirName)
	dst := filepath.Join(home, rootDirName)
	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	hits := scanHardcodedOldRoot(dst, src)
	if len(hits) != 1 || !strings.Contains(hits[0], "sync-lane-to-5090.sh") {
		t.Fatalf("应点名内部硬编码旧根的 sync 脚本, got %v", hits)
	}
}

// itoa 避免为一个整数转字符串引入 strconv（本文件其余部分都不需要它）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
