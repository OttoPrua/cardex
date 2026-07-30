package main

import "testing"

// applyDefaultReviewDivert：本地实现卡在 config 三件齐备时自动把复审分流到第二台机器，
// 显式声明/远端卡/配置不全 一律不套——均衡两侧额度的默认路由不得误伤显式意图。
func TestApplyDefaultReviewDivert(t *testing.T) {
	full := &Config{
		DefaultReviewHost: "qmthost",
		RemoteMirrorRoot:  "D:/Project/PO-lanes",
		DefaultReviewSync: "bash ~/.cardex/sync-lane-to-5090.sh",
	}

	t.Run("本地卡+config齐备→自动分流+推导镜像目录", func(t *testing.T) {
		task := &Task{Dir: "/Users/x/Projects/PO-lanes/wt-l1-edge"}
		applyDefaultReviewDivert(task, full)
		if task.ReviewHost != "qmthost" {
			t.Fatalf("ReviewHost 应=qmthost, got %q", task.ReviewHost)
		}
		if task.ReviewDir != "D:/Project/PO-lanes/wt-l1-edge" {
			t.Fatalf("ReviewDir 应推导为 <root>/<worktree名>, got %q", task.ReviewDir)
		}
		if task.ReviewSync != full.DefaultReviewSync {
			t.Fatalf("ReviewSync 应挂默认同步命令, got %q", task.ReviewSync)
		}
	})

	t.Run("显式ReviewHost→不覆盖(尊重任务级意图)", func(t *testing.T) {
		task := &Task{Dir: "/x/wt-mem", ReviewHost: "otherhost", ReviewDir: "D:/mine"}
		applyDefaultReviewDivert(task, full)
		if task.ReviewHost != "otherhost" || task.ReviewDir != "D:/mine" {
			t.Fatalf("显式值被默认覆盖了: host=%q dir=%q", task.ReviewHost, task.ReviewDir)
		}
	})

	t.Run("远端实现卡(RemoteHost非空)→不套默认", func(t *testing.T) {
		task := &Task{Dir: "/x/wt-evo", RemoteHost: "qmthost"}
		applyDefaultReviewDivert(task, full)
		if task.ReviewHost != "" {
			t.Fatalf("远端卡不该被套默认复审分流, got ReviewHost=%q", task.ReviewHost)
		}
	})

	t.Run("config不全(缺同步命令)→不套(否则分流到未同步镜像审旧态)", func(t *testing.T) {
		partial := &Config{DefaultReviewHost: "qmthost", RemoteMirrorRoot: "D:/Project/PO-lanes"}
		task := &Task{Dir: "/x/wt-panel"}
		applyDefaultReviewDivert(task, partial)
		if task.ReviewHost != "" {
			t.Fatalf("config 不全不该分流, got ReviewHost=%q", task.ReviewHost)
		}
	})

	t.Run("显式ReviewDir→保留,只补 host/sync", func(t *testing.T) {
		task := &Task{Dir: "/x/wt-vault", ReviewDir: "D:/custom-mirror"}
		applyDefaultReviewDivert(task, full)
		if task.ReviewHost != "qmthost" || task.ReviewDir != "D:/custom-mirror" {
			t.Fatalf("应补 host 但保留显式 dir: host=%q dir=%q", task.ReviewHost, task.ReviewDir)
		}
	})
}
