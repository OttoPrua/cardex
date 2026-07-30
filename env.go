package main

import (
	"fmt"
	"os"
	"sync"
)

// 【BD-44 改名过渡期的 env 双读】claudego → cardex 改名后，环境变量族一律以 CARDEX_* 为主名、
// CLAUDEGO_* 为兼容回落名。双读逻辑**只在本文件落一处**：散落到各调用点的话，某个点漏写回落
// 就是"同一份配置在 A 处生效、B 处不生效"的静默半迁移——比直接断名更难查。
//
// 【为什么不留 BIDE_* 一档】bide 是 BD-44 首裁、同日被附记改裁为 cardex 的短命名，从未装机、
// 从未有人在 shell 里设过 BIDE_ROOT（仅在本仓存活数小时）。为一个零存量的名字挂兼容分支，
// 是给后来者留一条永远走不到、却必须被读懂和维护的死路。
//
// 命中旧名时对 stderr 提示一次（按变量名去重）。为什么是"一次"而不是每次：defaultRoot 这类
// 函数在单次命令里会被调多轮，每轮打一行会把 stderr 淹掉，人反而不看了。
// 为什么是 stderr 而不是 stdout：`cardex list -json` 的 stdout 要能直接喂给解析器，
// 往里掺一行中文提示会把下游 json.Unmarshal 打红。
const (
	envRoot       = "CARDEX_ROOT"
	envRootLegacy = "CLAUDEGO_ROOT"

	envRequireSyncScripts       = "CARDEX_REQUIRE_SYNC_SCRIPTS"
	envRequireSyncScriptsLegacy = "CLAUDEGO_REQUIRE_SYNC_SCRIPTS"

	envSyncMirrorMode       = "CARDEX_SYNC_MIRROR_MODE"
	envSyncMirrorModeLegacy = "CLAUDEGO_SYNC_MIRROR_MODE"

	envSyncLocalMirrorRoot       = "CARDEX_SYNC_LOCAL_MIRROR_ROOT"
	envSyncLocalMirrorRootLegacy = "CLAUDEGO_SYNC_LOCAL_MIRROR_ROOT"
)

var (
	legacyEnvWarnMu   sync.Mutex
	legacyEnvWarnSeen = map[string]bool{}
)

// getenvCompat 取改名后的环境变量：优先新名，为空时回落旧名并提示一次。
// 返回值语义与 os.Getenv 一致（未设置与设为空串不区分）——本族变量没有"设成空串"的用法。
func getenvCompat(name, legacyName string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	v := os.Getenv(legacyName)
	if v != "" {
		warnLegacyEnvOnce(name, legacyName)
	}
	return v
}

func warnLegacyEnvOnce(name, legacyName string) {
	legacyEnvWarnMu.Lock()
	defer legacyEnvWarnMu.Unlock()
	if legacyEnvWarnSeen[legacyName] {
		return
	}
	legacyEnvWarnSeen[legacyName] = true
	fmt.Fprintf(os.Stderr, "提示: 环境变量 %s 是改名前的旧名（仍兼容读取），请改用 %s。\n", legacyName, name)
}
