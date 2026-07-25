package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

//go:embed templates/*.md
var embeddedTemplates embed.FS

// writeDefaultTemplates 把内置模板复制到数据目录（已存在的不覆盖，用户可自行修改）。
func writeDefaultTemplates(root string) error {
	dir := templatesDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := embeddedTemplates.ReadDir("templates")
	if err != nil {
		return err
	}
	for _, e := range entries {
		dst := filepath.Join(dir, e.Name())
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		data, err := embeddedTemplates.ReadFile("templates/" + e.Name())
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// loadTemplate 优先读数据目录里（可能被用户改过的）模板，退回内置版本。
func loadTemplate(root, name string) (string, error) {
	path := filepath.Join(templatesDir(root), name+".md")
	if data, err := os.ReadFile(path); err == nil {
		return string(data), nil
	}
	data, err := embeddedTemplates.ReadFile("templates/" + name + ".md")
	if err != nil {
		return "", fmt.Errorf("找不到模板 %s: %w", name, err)
	}
	return string(data), nil
}

var templateVarRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

// renderTemplate 单遍替换 {{KEY}} 占位符。单遍(而非按 map 循环 ReplaceAll)有两个必须:
// ①注入值里若含字面 {{OTHER}} 不会被二次替换(否则甲结论含 {{B}} 会被乙结论顶掉=注入);
// ②与 vars 的 map 迭代顺序无关(Go map 迭代随机,循环替换会让 C 的合并 prompt 跨运行非确定)。
// 未在 vars 里的 {{X}} 原样保留(同旧行为)。
func renderTemplate(tpl string, vars map[string]string) string {
	return templateVarRe.ReplaceAllStringFunc(tpl, func(m string) string {
		if v, ok := vars[m[2:len(m)-2]]; ok {
			return v
		}
		return m
	})
}
