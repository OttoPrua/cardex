package main

import (
	"strings"
	"testing"
)

func TestManagementTemplatesBudgetReviewByRisk(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path  string
		wants []string
	}{
		{
			path: "templates/coordinate.md",
			wants: []string{
				"发卡前先做收益/风险预算",
				"低风险",
				"复审预算",
				`"review_after":false`,
			},
		},
		{
			path: "templates/prompt-assembly.md",
			wants: []string{
				"先按影响面给目标定级",
				"未启用功能的理论加固不得抢占当前 MVP",
				"步数默认 1~3 步",
				`"review_after":false`,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			data, err := embeddedTemplates.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			for _, want := range tc.wants {
				if !strings.Contains(text, want) {
					t.Fatalf("%s 缺少任务预算规则 %q", tc.path, want)
				}
			}
		})
	}
}
