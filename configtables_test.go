package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ---- 缺陷类：config 的 map[string]<struct> 覆写按键整体替换，条目内字段不与内置表合并 ----
//
// 这一类的失败形态是**护栏静默失效**：结构合法、语义被截断，被清零的字段在下游正好是"宽松"语义，
// 于是既不报错也看不出差别。本文件按位点逐个立靶：命中本类的位点钉住"部分覆写后护栏仍在"，
// 判定为不命中的位点钉住**判定所依据的事实本身**（判据一旦被改动推翻，这里就转红，逼人重新分类）。

// TestConfigMapTablesRegistered 钉住登记表：Config 上每一个 map 类型字段都必须在此登记并写明分类。
// 【突变致死】新增一个 map[string]<struct> 配置表而不在这里登记 → 本测试直接红，
// 逼人对它做同一套"部分覆写会不会静默解除护栏"的判定，而不是让它默默长在类之外。
// 它也是"已登记 N 处"这句话的唯一凭据——N 由本表现算，不是记忆。
func TestConfigMapTablesRegistered(t *testing.T) {
	registry := map[string]string{
		// 命中本类，已做字段级回落：
		"stakes_policy": "命中: 档内留空字段由 stakesRule 回落内置表(TestStakesRuleFieldLevelFallback)",
		"type_defaults": "命中: 条目内留空字段由 typeDefaultsFor 回落内置表(TestTypeDefaultsFieldLevelFallback)",
		// 判定为不命中，各自的判据在下方测试里钉住：
		"cross_profiles": "不命中: 零值引擎 kind 为空 → applyCrossEngine 报错(TestCrossProfilePartialOverrideFailsLoudly)",
		"remote_hosts":   "不命中: 内置表不预置任何主机, 无内置值可被截断(TestDefaultConfigShipsNoBuiltinRemoteHosts)",
		"model_weights":  "不命中: 值是标量无档内字段; 键缺失有 default 与硬兜底(TestModelWeightSurvivesTruncatedTable)",
		"engines":        "不命中: 内置表不预置任何引擎条目, 无内置值可被截断(TestDefaultConfigShipsNoBuiltinEngines); 档内缺字段全落保守默认或载入即拒(TestValidateEnginesRejectsBadConfigs/TestEngineProfileZeroFieldsFailClosed)",
		"model_tiers":    "不命中: 值是标量档位关键字无档内字段; 内置表不预置条目, 缺键回落内置标准线, 坏值载入即拒(TestModelTiersCustomOverride/TestModelTiersValidation)",
	}

	found := map[string]bool{}
	rt := reflect.TypeOf(Config{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.Map {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" {
			name = f.Name
		}
		found[name] = true
		if _, ok := registry[name]; !ok {
			t.Errorf("Config.%s (json:%q) 是新增的 map 配置表但未登记分类: "+
				"请判定'用户只覆写部分字段时会不会静默解除护栏', 并在 registry 里写明结论与靶测试名", f.Name, name)
		}
	}
	for name := range registry {
		if !found[name] {
			t.Errorf("registry 登记了 %q 但 Config 上已无此 map 字段: 登记表过期, 请删除该条", name)
		}
	}
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("Config 上的 map 配置表共 %d 处: %v", len(names), names)
}

// ---- 位点一: stakes_policy（本轮 P1-1 点名）----

// TestStakesRuleFieldLevelFallback 是 P1-1 的直接靶。
// 【突变致死】把 stakesRule 改回"用户规则整条返回"(不做字段级回落) → 子用例
// "只抬 high 的思考地板: 强制复审不得被顺带解除" 立即红(ReviewAfter 变 false)。
func TestStakesRuleFieldLevelFallback(t *testing.T) {
	cases := []struct {
		name       string
		policy     map[string]StakesRule
		stakes     string
		inReview   bool
		inEffort   string
		wantReview bool
		wantEffort string
	}{
		{
			// 复审报告里的证伪场景原样：只想抬思考地板，结果把 review 打成空串。
			name:       "只抬 high 的思考地板: 强制复审不得被顺带解除",
			policy:     map[string]StakesRule{stakesHigh: {DefaultEffort: "xhigh"}},
			stakes:     stakesHigh,
			wantReview: true,
			wantEffort: "xhigh",
		},
		{
			name:       "只写 high 的 review: 思考地板不得被顺带清掉",
			policy:     map[string]StakesRule{stakesHigh: {Review: stakesReviewOn}},
			stakes:     stakesHigh,
			wantReview: true,
			wantEffort: "high", // 回落内置 high 档地板
		},
		{
			name:       "只写 low 的 default_effort: 强制不配复审仍在",
			policy:     map[string]StakesRule{stakesLow: {DefaultEffort: "medium"}},
			stakes:     stakesLow,
			inReview:   true, // 命令行给了 -review-after，low 档仍须压掉
			wantReview: false,
			wantEffort: "medium",
		},
		{
			// 要"不干预"必须显式写 follow——空串不再与 follow 同义，这是本修复的取值域改动。
			name:       "显式 follow: 才是不干预(保留 -review-after 原值)",
			policy:     map[string]StakesRule{stakesHigh: {Review: stakesReviewFollow}},
			stakes:     stakesHigh,
			inReview:   false,
			wantReview: false,
			wantEffort: "high",
		},
		{
			name:       "显式 off: 用户明写的值不被内置表顶回去",
			policy:     map[string]StakesRule{stakesHigh: {Review: stakesReviewOff}},
			stakes:     stakesHigh,
			inReview:   true,
			wantReview: false,
			wantEffort: "high",
		},
		{
			// JSON 里 "stakes_policy": null 会把整张 map 打成 nil；缺档回落必须仍然兜住。
			name:       "整表为 nil: 回落内置表",
			policy:     nil,
			stakes:     stakesHigh,
			wantReview: true,
			wantEffort: "high",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := defaultConfig("claude")
			cfg.StakesPolicy = c.policy
			task := &Task{ReviewAfter: c.inReview, Effort: c.inEffort}
			if err := applyStakes(task, cfg, c.stakes, false); err != nil {
				t.Fatalf("applyStakes: %v", err)
			}
			if task.ReviewAfter != c.wantReview {
				t.Errorf("ReviewAfter = %v, 应为 %v (档内字段级回落失效 = 护栏静默解除)", task.ReviewAfter, c.wantReview)
			}
			if task.Effort != c.wantEffort {
				t.Errorf("Effort = %q, 应为 %q", task.Effort, c.wantEffort)
			}
		})
	}
}

// TestLoadConfigPartialHighTierKeepsForcedReview 走真实 config.json → loadConfig → add 的整条路，
// 用的就是审查报告里那段 JSON 字面量。单测 stakesRule 只证函数，这条证"用户真这么写"时护栏还在。
func TestLoadConfigPartialHighTierKeepsForcedReview(t *testing.T) {
	root := t.TempDir()
	body := `{"claude_bin":"claude","stakes_policy":{"high":{"default_effort":"xhigh"}}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(root)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// 前提：JSON 确实把 high 整条替换掉了（否则本测试测的不是这个洞）。
	if raw := cfg.StakesPolicy[stakesHigh]; raw.Review != "" {
		t.Fatalf("前提不成立: 期望 JSON 把 high.review 打成空串, got %q —— 若合并粒度已变, 本测试需重写", raw.Review)
	}
	task := &Task{}
	if err := applyStakes(task, cfg, stakesHigh, false); err != nil {
		t.Fatalf("applyStakes: %v", err)
	}
	if !task.ReviewAfter {
		t.Error("部分覆写 high 档后 -stakes high 的卡失去强制复审 —— 护栏静默失效")
	}
	if task.Effort != "xhigh" {
		t.Errorf("用户写的 default_effort 未生效: Effort = %q, 应为 xhigh", task.Effort)
	}
}

// ---- 位点二: type_defaults（同类兄弟洞）----

// TestTypeDefaultsFieldLevelFallback 钉住：只覆写一个字段不得清空同条目其余字段。
// 【突变致死】把 task.go 的 typeDefaultsFor 换回裸 cfg.TypeDefaults[typ] → 复审卡的 AllowedTools 变 nil，
// 本测试红。这一条比 stakes 那条更贵：runner.go:92 是 len(AllowedTools)>0 才下发 --allowedTools，
// 工具清单消失时卡面看不出差别。
func TestTypeDefaultsFieldLevelFallback(t *testing.T) {
	root := t.TempDir()
	body := `{"claude_bin":"claude","type_defaults":{"design-review":{"model":"opus"}}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(root)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if raw := cfg.TypeDefaults[typeReview]; len(raw.AllowedTools) != 0 || raw.PermissionMode != "" {
		t.Fatalf("前提不成立: 期望 JSON 把 design-review 整条替换, got %+v", raw)
	}

	card := newTask(root, cfg, typeReview, "审这一版", "/tmp/proj", []string{"p"}, 5)
	if card.Model != "opus" {
		t.Errorf("用户显式写的 model 未生效: %q", card.Model)
	}
	builtin := defaultConfig("claude").TypeDefaults[typeReview]
	if !reflect.DeepEqual(card.AllowedTools, builtin.AllowedTools) {
		t.Errorf("复审卡的只读工具集被静默清空: got %v, 应回落内置 %v", card.AllowedTools, builtin.AllowedTools)
	}
	if card.PermissionMode != builtin.PermissionMode {
		t.Errorf("permission_mode 被静默清空: got %q, 应回落内置 %q", card.PermissionMode, builtin.PermissionMode)
	}
	if card.Effort != builtin.Effort {
		t.Errorf("effort 被静默清空: got %q, 应回落内置 %q", card.Effort, builtin.Effort)
	}
}

// TestTypeDefaultsUserFieldsWin 反向护栏：回落只补空字段，不得把用户明写的值顶回内置值。
func TestTypeDefaultsUserFieldsWin(t *testing.T) {
	cfg := defaultConfig("claude")
	cfg.TypeDefaults[typeReview] = TypeDefaults{
		PermissionMode: "plan",
		AllowedTools:   []string{"Read"},
		Model:          "sonnet",
		Effort:         "max",
	}
	td, ok := typeDefaultsFor(cfg, typeReview)
	if !ok {
		t.Fatal("typeDefaultsFor 应命中 design-review")
	}
	if td.PermissionMode != "plan" || td.Model != "sonnet" || td.Effort != "max" ||
		!reflect.DeepEqual(td.AllowedTools, []string{"Read"}) {
		t.Errorf("用户明写的字段被内置表顶掉: %+v", td)
	}
}

// TestTypeDefaultsAbsentTypeStaysAbsent 钉住"类型整条缺失 ≠ 条目内字段缺失"这条边界。
// reviewdivert 的远端 codex 实现卡场景依赖它：type_defaults 无 sequence 条目时实现卡 Model 必须为空。
// 【突变致死】若有人把 typeDefaultsFor 改成"缺条目也回落内置表"，这里立刻红。
func TestTypeDefaultsAbsentTypeStaysAbsent(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{TypeDefaults: map[string]TypeDefaults{typeReview: {Model: "opus"}}}
	if _, ok := typeDefaultsFor(cfg, typeSequence); ok {
		t.Error("type_defaults 未配置 sequence 时不得回落内置条目")
	}
	impl := newTask(root, cfg, typeSequence, "实现", "/tmp/proj", []string{"p"}, 7)
	if impl.Model != "" || len(impl.AllowedTools) != 0 || impl.PermissionMode != "" {
		t.Errorf("未配置的类型不得被烘焙任何默认参数: %+v", impl)
	}
}

// TestBuiltinTypeDefaultsSkipPermissionsAllFalse 钉住 typeDefaultsFor 里那段注释的事实依据：
// SkipPermissions 是 bool，false 与"没写"不可区分，做不了字段级回落；因为内置表**当前**每一档都是
// false，"不回落它"与"回落它"结果相同，这个缺口才是无害的。
// 【突变致死】给内置表任一档加 skip_permissions:true → 这里红，逼人把该字段改成可表达三态的类型，
// 而不是让"跳过权限"这类高危位继续走一条无法回落的通道。
func TestBuiltinTypeDefaultsSkipPermissionsAllFalse(t *testing.T) {
	for typ, td := range defaultConfig("claude").TypeDefaults {
		if td.SkipPermissions {
			t.Errorf("内置 type_defaults[%s].skip_permissions = true: "+
				"该字段是 bool，用户部分覆写时无法与'没写'区分、回落不了；"+
				"请把 TypeDefaults.SkipPermissions 改成 *bool 并在 typeDefaultsFor 里补回落", typ)
		}
	}
}

// TestEffectiveModelUsesMergedTypeDefault 钉住看板展示与卡面烘焙同源。
// 【突变致死】把 boardmodel.go 的 typeDefaultsFor 换回裸查表 → 部分覆写时看板显示"无模型"、
// 实际卡跑的是内置模型，展示与执行分叉，本测试红。
func TestEffectiveModelUsesMergedTypeDefault(t *testing.T) {
	cfg := defaultConfig("claude")
	cfg.TypeDefaults[typeReview] = TypeDefaults{PermissionMode: "default"} // 用户只覆写 permission_mode
	model, source := effectiveModel(cfg, &Task{Type: typeReview})
	builtin := defaultConfig("claude").TypeDefaults[typeReview].Model
	if model != builtin || source != "type_default" {
		t.Errorf("effectiveModel = (%q,%q), 应为 (%q,\"type_default\") —— 展示层未走合并后的类型默认",
			model, source, builtin)
	}
}

// ---- 位点三/四/五: 判定为不命中，此处钉住判据 ----

// TestCrossProfilePartialOverrideFailsLoudly 是 cross_profiles 判"不命中"的依据：
// 部分覆写同样会把另一半引擎打成零值，但零值 kind 走 applyCrossEngine 的 default 分支**报错**，
// 不是静默降级——失败是响的，不属"护栏静默失效"类。
// 【突变致死】若有人给 applyCrossEngine 的未知 kind 加一条"默认按本机 claude 跑"的兜底，
// 这里立刻红：那一改会把本位点拖进本类（单腿冒充交叉验证且无声）。
func TestCrossProfilePartialOverrideFailsLoudly(t *testing.T) {
	root := t.TempDir()
	body := `{"claude_bin":"claude","codex_bin":"codex","codex_model":"gpt-x",
	          "cross_profiles":{"opus-codex":{"a":{"kind":"claude","model":"claude-opus-5","effort":"max"}}}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(root)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	prof := cfg.CrossProfiles["opus-codex"]
	if prof.B.Kind != "" {
		t.Fatalf("前提不成立: 期望部分覆写把引擎乙打成零值, got %+v", prof.B)
	}
	if err := applyCrossEngine(&Task{Type: typeCrossCheck, Prompts: []string{"p"}}, prof.B, cfg); err == nil {
		t.Error("零值交叉引擎必须报错: 静默放行会让交叉验证退化成单腿自审而账面无差别")
	}
}

// TestDefaultConfigShipsNoBuiltinRemoteHosts 是 remote_hosts 判"不命中"的依据：
// 内置表不预置任何主机，条目全部由用户自写，没有"内置值被部分覆写截断"这回事。
// 【突变致死】哪天给 defaultConfig 加了内置 remote_hosts 条目，这里红，逼人重新做分类判定。
func TestDefaultConfigShipsNoBuiltinRemoteHosts(t *testing.T) {
	if n := len(defaultConfig("claude").RemoteHosts); n != 0 {
		t.Errorf("defaultConfig 现在预置了 %d 个 remote_hosts: "+
			"remote_hosts 此前按'无内置值可被截断'判为不属'覆写截断'类, 该判据已失效, 请重新判定并补字段级回落", n)
	}
}

// TestModelWeightSurvivesTruncatedTable 是 model_weights 判"不命中"的依据：
// 值是标量、没有"条目内字段"可被截断；键整表被打成 nil 时 modelWeight 有硬兜底 1，不会算出 0
// （算出 0 会让加权用量恒为 0、预算红线永不触发——那才会是同类的静默失效）。
func TestModelWeightSurvivesTruncatedTable(t *testing.T) {
	cfg := defaultConfig("claude")
	cfg.ModelWeights = nil
	if w := modelWeight(cfg, "opus"); w != 1 {
		t.Errorf("model_weights 整表为 nil 时权重 = %v, 应硬兜底为 1 (0 会让预算红线恒不触发)", w)
	}
	cfg.ModelWeights = map[string]float64{"default": 2}
	if w := modelWeight(cfg, "opus"); w != 2 {
		t.Errorf("缺键时应回落 default: got %v, 应为 2", w)
	}
}

// TestConfigJSONRoundTripKeepsMergedShape 兜一条实际链路：init 写盘的 config.json 里
// 两张已修位点的表都必须是**写全字段**的（用户照着改一格时不会踩到本类洞的最坏情形）。
func TestConfigJSONRoundTripKeepsMergedShape(t *testing.T) {
	data, err := json.Marshal(defaultConfig("claude"))
	if err != nil {
		t.Fatal(err)
	}
	var round Config
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatal(err)
	}
	if round.StakesPolicy[stakesHigh].Review != stakesReviewOn ||
		round.StakesPolicy[stakesHigh].DefaultEffort != "high" {
		t.Errorf("stakes_policy 序列化往返丢字段: %+v", round.StakesPolicy[stakesHigh])
	}
	if td := round.TypeDefaults[typeReview]; len(td.AllowedTools) == 0 || td.PermissionMode == "" {
		t.Errorf("type_defaults 序列化往返丢字段: %+v", td)
	}
}
