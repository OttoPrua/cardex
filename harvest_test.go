package main

// 「结果在手早收割」回归测试。
// 场景来自实战:远端命令把完整结果 JSON 打回 stdout 后,远端孙进程吊住 ssh 管道
// 致 EOF 永不到来,任务空挂到 step_timeout(150 分钟)——完成品不被收割,还占着
// 同目录串行锁。看门狗应在两拍内整组击杀,上层救援把退出码洗白。

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

const harvestResJSON = `{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s1","num_turns":1}`

func TestHarvestKillsHungPipeAfterResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 sh")
	}
	old := remoteHarvestPoll
	remoteHarvestPoll = 100 * time.Millisecond
	defer func() { remoteHarvestPoll = old }()

	// 打完结果后长眠不退,模拟孙进程吊管道。
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "printf '%s\\n' '"+harvestResJSON+"'; sleep 60")
	setupProcGroup(cmd)
	var out syncBuffer
	cmd.Stdout = &out

	start := time.Now()
	runErr := runCmdRegisteredHarvest(cmd, func() bool {
		return parseClaudeJSON(out.String()) != nil
	})
	elapsed := time.Since(start)

	if elapsed > 15*time.Second {
		t.Fatalf("早收割未生效,等了 %v(应两拍内击杀)", elapsed)
	}
	if runErr == nil {
		t.Fatal("被击杀应返回非 nil 错误(由上层「结果在手即成功」救援洗白)")
	}
	res := parseClaudeJSON(out.String())
	if res == nil || res.IsError || res.Result != "ok" {
		t.Fatalf("结果应完整在手: %+v", res)
	}
	// 复刻 invokeRemoteClaude 的救援判定:结果在手且非错 → 洗白。
	if res != nil && runErr != nil && !res.IsError {
		runErr = nil
	}
	if runErr != nil {
		t.Fatalf("救援应洗白击杀退出码: %v", runErr)
	}
}

func TestHarvestNormalExitUnaffected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 sh")
	}
	old := remoteHarvestPoll
	remoteHarvestPoll = 100 * time.Millisecond
	defer func() { remoteHarvestPoll = old }()

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "printf '%s\\n' '"+harvestResJSON+"'")
	setupProcGroup(cmd)
	var out syncBuffer
	cmd.Stdout = &out
	if err := runCmdRegisteredHarvest(cmd, func() bool {
		return parseClaudeJSON(out.String()) != nil
	}); err != nil {
		t.Fatalf("正常退出不应被看门狗波及: %v", err)
	}
	if res := parseClaudeJSON(out.String()); res == nil || res.Result != "ok" {
		t.Fatalf("结果应在手: %+v", res)
	}
}

func TestHarvestNoResultRunsToCompletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 sh")
	}
	old := remoteHarvestPoll
	remoteHarvestPoll = 50 * time.Millisecond
	defer func() { remoteHarvestPoll = old }()

	// 没有结果 JSON 的慢命令:看门狗永不武装,进程自然跑完。
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 1; echo not-a-result")
	setupProcGroup(cmd)
	var out syncBuffer
	cmd.Stdout = &out
	if err := runCmdRegisteredHarvest(cmd, func() bool {
		return parseClaudeJSON(out.String()) != nil
	}); err != nil {
		t.Fatalf("无结果时不应击杀: %v", err)
	}
}
