package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 披露字段必须到达界面——按**类**守门，不是逐个字段各钉一枚。
//
// 【要防的缺陷类】后端加一个 `*_error` 契约字段、写好测试证明它挂上了 /api/overview，
// 然后前端一个字都没消费。用户在看板上看到的与"一切正常"完全一样，而这个字段存在的
// 全部意义就是不让这两种状态长得一样（BD-45 R1·P1-2 抓到的正是 project_alias_error：
// 后端有测试、有注释说"与 kind_rules 同一纪律"，app.js 里零处消费）。
//
// 【为什么用扫描而不是列清单】列清单的守门只护住写清单那一刻存在的字段，下一个新字段
// 照样能溜过去。这里直接扫本包全部非测试 .go 源码里的 json tag，任何新增的 `*_error`
// 字段自动进入被守范围——要么在 app.js 里被消费，要么进 exempt 表并写明理由。

// disclosureExempt 是**不需要**到达界面的 `*_error` json 字段，每条必须写明理由。
// 加进这张表等于声明"这个字段不是给看板用户看的"，理由会被复审逐条查。
var disclosureExempt = map[string]string{
	// runner.go transcriptEvent.IsError：Claude/Codex 会话流水的逐条事件标记，
	// 落在 transcript 文件里供排障，不进 /api/* 响应，也没有对应的看板控件。
	"is_error": "transcript 事件字段，不上看板 API",
}

// jsonTagRe 抓 struct tag 里的 json 键名（`json:"key,omitempty"` → key）。
var jsonTagRe = regexp.MustCompile(`json:"([a-zA-Z0-9_]+)[,"]`)

// goJSONErrorKeys 扫本包全部非测试 .go 文件，返回所有含 "_error" 的 json 键名。
func goJSONErrorKeys(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		data, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range jsonTagRe.FindAllStringSubmatch(string(data), -1) {
			if strings.Contains(m[1], "_error") {
				out[m[1]] = append(out[m[1]], n)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("一个 *_error json 字段都没扫到：扫描器自身失效了（正则或工作目录变了）")
	}
	return out
}

// appJSCode 返回 web/app.js 去掉注释行后的源码。
// 去注释是为了让"只在注释里提过这个字段名"不能冒充消费——本轮修复前，
// project_alias_error 在后端注释里被反复提及，界面上却一处都没渲染。
// 读的是 boardWeb 这个 embed.FS：守的是**真正打进二进制发出去**的那一份。
func appJSCode(t *testing.T) string {
	t.Helper()
	data, err := boardWeb.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "*") || strings.HasPrefix(s, "/*") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestDisclosureFieldsReachUI —— 每个 `*_error` 契约字段都必须在 app.js 里被真正读取。
//
// 杀死的突变：给 board.go / boardmodel.go 加一个新的 `*_error` 字段而不接前端；
// 或把 app.js 里现有的某处消费删掉/改名。两者都立即报红。
func TestDisclosureFieldsReachUI(t *testing.T) {
	code := appJSCode(t)
	keys := goJSONErrorKeys(t)
	for key, files := range keys {
		if why, ok := disclosureExempt[key]; ok {
			if why == "" {
				t.Errorf("%s 进了豁免表却没写理由", key)
			}
			continue
		}
		// 要求以属性读取的形式出现（d.key / p.key / t.key），而不只是字面串出现过。
		if !regexp.MustCompile(`\.` + regexp.QuoteMeta(key) + `\b`).MatchString(code) {
			t.Errorf("披露字段 %s（定义于 %s）在 web/app.js 里零处消费："+
				"坏配置时界面与一切正常完全一样，正是该字段要防的失效形态。"+
				"请在对应视图加一处告警渲染，或加进 disclosureExempt 并写明理由。",
				key, strings.Join(files, ", "))
		}
	}
	// 豁免表不得留过期条目：字段已删还留着豁免，下一个同名字段会被静默放行。
	for key := range disclosureExempt {
		if _, ok := keys[key]; !ok {
			t.Errorf("disclosureExempt 里的 %s 在源码里已不存在，请删掉这条豁免", key)
		}
	}
}

// TestProjectAliasErrorRendersAsCallout —— P1-2 本体的定点桩：
// 光"被读到"还不够，它得像 kind_rule_error 那样渲染成一条可见告警。
// 杀死的突变：把 app.js 里那段改成只 console.log / 只赋值不 append。
func TestProjectAliasErrorRendersAsCallout(t *testing.T) {
	code := appJSCode(t)
	i := strings.Index(code, "d.project_alias_error")
	if i < 0 {
		t.Fatal("app.js 未消费 project_alias_error")
	}
	// 取该处往后一小段，要求同一块里出现 callout 与 frag.append（可见告警 + 真的挂进 DOM）。
	seg := code[i:min(i+400, len(code))]
	for _, want := range []string{"callout(", "append("} {
		if !strings.Contains(seg, want) {
			t.Errorf("project_alias_error 必须渲染成可见告警并挂进 DOM（缺 %q）：\n%s", want, seg)
		}
	}
}
