package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 【BD-44 改名】数据根解析顺序与 env 双读的回归。
//
// 为什么这组用例值钱：改名当天全世界的数据都还在 ~/.claudego。解析顺序写错一格，cardex 就会在
// 一个空的 ~/.cardex 上开张——队列看着零卡、launchd 照常 tick、旧根里在跑的活没人管。这是"看起来
// 正常、实则整队丢失"的失败模式，比崩掉难查得多。故逐档钉死顺序，每一档配一个反例。

// captureStderr 跑 fn 并返回它写到 os.Stderr 的内容。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// resetLegacyWarnState 清掉"只提示一次"的去重态，让每个用例都能独立观察提示。
func resetLegacyWarnState() {
	legacyEnvWarnMu.Lock()
	legacyEnvWarnSeen = map[string]bool{}
	legacyEnvWarnMu.Unlock()
	legacyRootWarnOnce = sync.Once{}
}

// clearRootEnv 把两个根变量都清空，避免真实环境（或前一个用例）泄进来。
func clearRootEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envRoot, "")
	t.Setenv(envRootLegacy, "")
}

func TestDefaultRootResolutionOrder(t *testing.T) {
	// ① CARDEX_ROOT 最高优先：即便旧名也设了、即便 home 下两个根都在，也认它。
	t.Run("CARDEX_ROOT 优先于 CLAUDEGO_ROOT", func(t *testing.T) {
		resetLegacyWarnState()
		home := t.TempDir()
		t.Setenv("HOME", home)
		mustMkdir(t, filepath.Join(home, rootDirName))
		mustMkdir(t, filepath.Join(home, legacyRootDirName))
		t.Setenv(envRoot, "/tmp/new-root")
		t.Setenv(envRootLegacy, "/tmp/old-root")
		if got := defaultRoot(); got != "/tmp/new-root" {
			t.Fatalf("CARDEX_ROOT 必须压过 CLAUDEGO_ROOT 与 home 探测, got %q", got)
		}
	})

	// ② 只设旧名：兼容读命中，且必须在 stderr 提示（不提示 = 用户永远不知道自己在用旧名）。
	t.Run("只设 CLAUDEGO_ROOT 则兼容读并提示", func(t *testing.T) {
		resetLegacyWarnState()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv(envRoot, "")
		t.Setenv(envRootLegacy, "/tmp/old-root")
		var got string
		errOut := captureStderr(t, func() { got = defaultRoot() })
		if got != "/tmp/old-root" {
			t.Fatalf("CLAUDEGO_ROOT 兼容读失效, got %q", got)
		}
		if !strings.Contains(errOut, envRootLegacy) || !strings.Contains(errOut, envRoot) {
			t.Fatalf("命中旧名必须在 stderr 提示新旧两个名字, stderr=%q", errOut)
		}
	})

	// ③ 无 env + ~/.cardex 存在 → 用新根（哪怕旧根也在）。
	t.Run("两根都在时优先新根", func(t *testing.T) {
		resetLegacyWarnState()
		home := t.TempDir()
		t.Setenv("HOME", home)
		clearRootEnv(t)
		newRoot := filepath.Join(home, rootDirName)
		mustMkdir(t, newRoot)
		mustMkdir(t, filepath.Join(home, legacyRootDirName))
		if got := defaultRoot(); got != newRoot {
			t.Fatalf("~/.cardex 存在时必须用它, got %q want %q", got, newRoot)
		}
	})

	// ④ 无 env + 只有 ~/.claudego → 认旧根并提示 migrate。
	//    这是本组最关键的一条：若这里返回 ~/.cardex，改名当天就是整队"消失"。
	t.Run("只有旧根则认旧根并提示 migrate", func(t *testing.T) {
		resetLegacyWarnState()
		home := t.TempDir()
		t.Setenv("HOME", home)
		clearRootEnv(t)
		legacy := filepath.Join(home, legacyRootDirName)
		mustMkdir(t, legacy)
		var got string
		errOut := captureStderr(t, func() { got = defaultRoot() })
		if got != legacy {
			t.Fatalf("只有 ~/.claudego 时必须认它(否则改名当天整队看着为空), got %q want %q", got, legacy)
		}
		if !strings.Contains(errOut, "migrate") {
			t.Fatalf("用 legacy root 必须提示 cardex migrate, stderr=%q", errOut)
		}
	})

	// ⑤ 全新装机（两个都不存在）→ 新名默认。
	t.Run("全新装机落 ~/.cardex", func(t *testing.T) {
		resetLegacyWarnState()
		home := t.TempDir()
		t.Setenv("HOME", home)
		clearRootEnv(t)
		want := filepath.Join(home, rootDirName)
		if got := defaultRoot(); got != want {
			t.Fatalf("全新装机应落 %q, got %q", want, got)
		}
	})

	// ⑥ -root flag 压过一切 env / home 探测。
	t.Run("-root flag 最高优先", func(t *testing.T) {
		resetLegacyWarnState()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv(envRoot, "/tmp/new-root")
		t.Setenv(envRootLegacy, "/tmp/old-root")
		want := filepath.Join(home, "explicit")
		if got := resolveRoot(want); got != want {
			t.Fatalf("-root 必须压过所有 env, got %q want %q", got, want)
		}
	})

	// ⑦ 同名文件（非目录）不算"存在"：~/.cardex 若是个文件，必须继续往下认旧根，
	//    否则会把数据根指到一个 MkdirAll 必然失败的路径上。
	t.Run("同名文件不算数据根", func(t *testing.T) {
		resetLegacyWarnState()
		home := t.TempDir()
		t.Setenv("HOME", home)
		clearRootEnv(t)
		if err := os.WriteFile(filepath.Join(home, rootDirName), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		legacy := filepath.Join(home, legacyRootDirName)
		mustMkdir(t, legacy)
		var got string
		_ = captureStderr(t, func() { got = defaultRoot() })
		if got != legacy {
			t.Fatalf("~/.cardex 是普通文件时应继续认旧根, got %q", got)
		}
	})
}

// TestGetenvCompatWarnsOnce 钉死"提示一次"：每次调用都打会把 stderr 淹掉，人就不看了。
func TestGetenvCompatWarnsOnce(t *testing.T) {
	resetLegacyWarnState()
	t.Setenv(envSyncMirrorMode, "")
	t.Setenv(envSyncMirrorModeLegacy, "local")
	var vals []string
	errOut := captureStderr(t, func() {
		for i := 0; i < 3; i++ {
			vals = append(vals, getenvCompat(envSyncMirrorMode, envSyncMirrorModeLegacy))
		}
	})
	for _, v := range vals {
		if v != "local" {
			t.Fatalf("旧名回落值错, got %q", v)
		}
	}
	if n := strings.Count(errOut, envSyncMirrorModeLegacy); n != 1 {
		t.Fatalf("旧名提示必须恰好一次, got %d 次\nstderr=%q", n, errOut)
	}
}

// TestGetenvCompatPrefersNewName 新名非空时不得回落、也不得提示。
func TestGetenvCompatPrefersNewName(t *testing.T) {
	resetLegacyWarnState()
	t.Setenv(envSyncLocalMirrorRoot, "/new")
	t.Setenv(envSyncLocalMirrorRootLegacy, "/old")
	var got string
	errOut := captureStderr(t, func() {
		got = getenvCompat(envSyncLocalMirrorRoot, envSyncLocalMirrorRootLegacy)
	})
	if got != "/new" {
		t.Fatalf("新名应优先, got %q", got)
	}
	if strings.Contains(errOut, envSyncLocalMirrorRootLegacy) {
		t.Fatalf("新名命中时不应提示旧名, stderr=%q", errOut)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}
