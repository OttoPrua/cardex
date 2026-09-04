package main

// extractEmitTasks 容错阶梯回归测试。
// 用例形状来自三次实战 emit 失败：模型漏写 json 围栏标签、把 JSON 混进中文叙述、
// 只把清单写进 _WAVE-N-TASKS.json 文件而不回显（错误面目是 invalid character 'ç'/'å'——
// 整段中文叙述被喂进 json.Unmarshal 后 CJK 首字节的样子）。

import (
	"os"
	"path/filepath"
	"testing"
)

const tasksJSON = `{"tasks":[{"title":"实现 任务甲","type":"sequence","priority":7,"prompt":"做甲"}]}`

func TestExtractFencedJSONTagged(t *testing.T) {
	out := "说明文字\n```json\n" + tasksJSON + "\n```\n收尾"
	tasks, err := extractEmitTasks(out, "")
	if err != nil || len(tasks) != 1 || tasks[0].Title != "实现 任务甲" {
		t.Fatalf("json 标签围栏应命中: tasks=%v err=%v", tasks, err)
	}
}

func TestExtractFencedBare(t *testing.T) {
	// 模型漏写 json 标签，裸围栏。
	out := "第四批排卡已产出，清单如下：\n```\n" + tasksJSON + "\n```"
	tasks, err := extractEmitTasks(out, "")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("裸围栏应命中: tasks=%v err=%v", tasks, err)
	}
}

func TestExtractLastFenceWins(t *testing.T) {
	// 前面是解说用的示例块，最后才是操作块。
	out := "示例：\n```json\n{\"tasks\":[]}\n```\n正式：\n```json\n" + tasksJSON + "\n```"
	tasks, err := extractEmitTasks(out, "")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("应取最后一个非空围栏: tasks=%v err=%v", tasks, err)
	}
}

func TestExtractUnfencedBalancedScan(t *testing.T) {
	// 无围栏，JSON 直接混在中文叙述里（历史 'å' 失败形状）。
	out := "上一批核实完成，本波不放量。产出如下 " + tasksJSON + " 以上请核对。"
	tasks, err := extractEmitTasks(out, "")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("平衡扫描应命中: tasks=%v err=%v", tasks, err)
	}
}

func TestExtractUnfencedNestedPrefix(t *testing.T) {
	// "tasks" 前还有别的键（最近的 '{' 属于嵌套对象），需外推回溯。
	out := `结论如下 {"meta":{"wave":4},"tasks":[{"title":"卡A","prompt":"做A"}]} 完`
	tasks, err := extractEmitTasks(out, "")
	if err != nil || len(tasks) != 1 || tasks[0].Title != "卡A" {
		t.Fatalf("嵌套前缀应外推命中: tasks=%v err=%v", tasks, err)
	}
}

func TestExtractBareArray(t *testing.T) {
	out := "```json\n[{\"title\":\"卡B\",\"prompt\":\"做B\"}]\n```"
	tasks, err := extractEmitTasks(out, "")
	if err != nil || len(tasks) != 1 || tasks[0].Title != "卡B" {
		t.Fatalf("裸数组应命中: tasks=%v err=%v", tasks, err)
	}
}

func TestExtractFileRescue(t *testing.T) {
	// 模型只把清单写进任务目录下的文件、输出纯叙述（模型只写文件不回显的实战失败形状）。
	dir := t.TempDir()
	sub := filepath.Join(dir, "plans", "cards")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "_BATCH-4-TASKS.json"), []byte(tasksJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	out := "第四批排卡已产出:`plans/cards/_BATCH-4-TASKS.json`。核心结论——上一批根本没跑,故本波不放量、只结转。"
	tasks, err := extractEmitTasks(out, dir)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("文件救援应命中: tasks=%v err=%v", tasks, err)
	}
}

func TestExtractFileRescueRejectsTraversal(t *testing.T) {
	// 越界路径（../）绝不读取。
	parent := t.TempDir()
	dir := filepath.Join(parent, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "evil.json"), []byte(tasksJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	out := "清单在 ../evil.json 里。"
	if tasks, err := extractEmitTasks(out, dir); err == nil {
		t.Fatalf("越界文件不应被读取: tasks=%v", tasks)
	}
}

func TestExtractPureNarrativeFails(t *testing.T) {
	// 纯叙述必须报错（而不是 mojibake panic 或误解析）。
	out := "本批排卡已完成核实，本波不放量，详见文档。"
	if tasks, err := extractEmitTasks(out, ""); err == nil {
		t.Fatalf("纯叙述应报错: tasks=%v", tasks)
	}
}

// TestEmittedChildrenInheritReplyRoute 钉住 R8 谱系的第一层：装配/协调卡派出的子卡
// 必须带着同一条回报路由，否则"谁派的卡回报给谁"在 emit 处就断了——父卡的终态回到
// requester，子卡的终态却掉回配置订阅（或无人接收），而卡面上看不出差别。
// 【突变致死】把 enqueueEmitted 里的 nt.ReplyRoute 赋值删掉 → 本测试红。
func TestEmittedChildrenInheritReplyRoute(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	parent := newTask(root, cfg, typeAssembly, "装配父", t.TempDir(), []string{"拆"}, 5)
	parent.EmitTasks = true
	parent.ReplyRoute = &TaskReplyRoute{
		Schema:         taskReplyRouteSchemaV1,
		RequesterID:    "cardex-control-plane",
		EndpointKind:   replyEndpointCodexThread,
		EndpointThread: "019fe484-12c5-78b2-986c-6a8bf0d2b079",
		EscalateToRoot: true,
	}
	if err := saveTask(root, parent); err != nil {
		t.Fatal(err)
	}
	ids, err := enqueueEmitted(root, cfg, parent, "产出：\n```json\n"+tasksJSON+"\n```")
	if err != nil || len(ids) != 1 {
		t.Fatalf("emit 应入队一张子卡: ids=%v err=%v", ids, err)
	}
	child, err := loadTask(root, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if child.ReplyRoute == nil || *child.ReplyRoute != *parent.ReplyRoute {
		t.Fatalf("emit 子卡未继承回报路由: %+v", child.ReplyRoute)
	}
	if child.ReplyRoute == parent.ReplyRoute {
		t.Fatal("继承必须深拷贝: 共享同一结构时改子卡会静默改掉父卡的回报坐标")
	}
}

func TestExtractTasksInsidePromptString(t *testing.T) {
	// 合法外层 JSON 的 prompt 字符串里也出现 "tasks" 字样，不应干扰解析。
	out := "产出：\n```json\n{\"tasks\":[{\"title\":\"卡C\",\"prompt\":\"读 tasks JSON 后执行\"}]}\n```"
	tasks, err := extractEmitTasks(out, "")
	if err != nil || len(tasks) != 1 || tasks[0].Title != "卡C" {
		t.Fatalf("字符串内 tasks 字样不应干扰: tasks=%v err=%v", tasks, err)
	}
}
