package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// configStructKeys parses config.go and returns every JSON field name found in any struct tag.
// Using all structs (not just Config) keeps the allowed set a superset and avoids false positives
// for nested fields like codex_model (used in XFrozenEngine too).
func configStructKeys(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	// Match json:"key" or json:"key,omitempty" — capture stops at first non-key char (, or ")
	re := regexp.MustCompile(`json:"([a-z][a-z0-9_]*)`)
	keys := map[string]bool{}
	for _, m := range re.FindAllSubmatch(data, -1) {
		keys[string(m[1])] = true
	}
	return keys
}

// readmeConfigSectionKeys extracts backtick-wrapped snake_case identifiers (must contain ≥1 underscore)
// from the config quick-reference section of a README. Restricted to that section so we don't
// accidentally flag task/status field names that appear in the surrounding prose.
func readmeConfigSectionKeys(content string) []string {
	// Works for both the Chinese and English README (different section headers).
	for _, hdr := range []string{"## 配置速查", "## Config quick reference"} {
		idx := strings.Index(content, hdr)
		if idx == -1 {
			continue
		}
		rest := content[idx+len(hdr):]
		// Section ends at the next top-level ## heading.
		if end := strings.Index(rest, "\n## "); end != -1 {
			rest = rest[:end]
		}
		// `key_name` — must start with letter, contain at least one underscore group, no dots/stars.
		re := regexp.MustCompile("`([a-z][a-z0-9]*(?:_[a-z0-9]+)+)`")
		seen := map[string]bool{}
		var out []string
		for _, m := range re.FindAllStringSubmatch(rest, -1) {
			k := m[1]
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
		return out
	}
	return nil
}

// TestReadmeConfigKeys asserts that:
//  1. The five required items (board + four config keys) appear literally in both README files.
//  2. Every config key cited in each README's config-reference section actually exists in config.go.
//  3. The four required config keys also exist in config.go (guards against drift in config.go itself).
func TestReadmeConfigKeys(t *testing.T) {
	cfgKeys := configStructKeys(t)

	// Terms that MUST appear in both READMEs after this commit.
	// "board" is the command; the rest are config.go JSON field names.
	required := []string{
		"board",
		"default_review_host",
		"remote_mirror_root",
		"default_review_sync",
		"codex_fallback_model",
	}
	// Subset of required[] that must ALSO exist in config.go.
	requiredCfgKeys := []string{
		"default_review_host",
		"remote_mirror_root",
		"default_review_sync",
		"codex_fallback_model",
	}

	readmes := []string{"README.md", "README.en.md"}
	for _, name := range readmes {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		content := string(data)

		// 1. Required terms appear in file.
		for _, term := range required {
			if !strings.Contains(content, term) {
				t.Errorf("%s: required term %q not found", name, term)
			}
		}

		// 2. Config-section keys ⊆ config.go key set.
		for _, k := range readmeConfigSectionKeys(content) {
			if !cfgKeys[k] {
				t.Errorf("%s: config section references key %q which is not in config.go", name, k)
			}
		}
	}

	// 3. Required config keys exist in config.go.
	for _, k := range requiredCfgKeys {
		if !cfgKeys[k] {
			t.Errorf("config.go: required key %q not found (config.go may be out of sync with README)", k)
		}
	}
}

// TestReadmeConfigKeys_FakeKey is a counter-example / fixture test.
// It verifies that readmeConfigSectionKeys() would detect a misspelled key (codex_fallback_modle)
// and that such a key is correctly absent from config.go — proving the main test WOULD fail red
// if someone introduced that typo into a real README's config-reference section.
func TestReadmeConfigKeys_FakeKey(t *testing.T) {
	cfgKeys := configStructKeys(t)

	// Minimal fixture: a config-reference section containing a deliberately misspelled key.
	fixture := "## Config quick reference (~/.claudego/config.json)\n" +
		"| `codex_fallback_modle` | \"\" | deliberately misspelled — should be caught |\n"

	keys := readmeConfigSectionKeys(fixture)

	// The extractor must find the misspelled key.
	found := false
	for _, k := range keys {
		if k == "codex_fallback_modle" {
			found = true
		}
	}
	if !found {
		t.Fatal("readmeConfigSectionKeys did not extract 'codex_fallback_modle' from fixture — extraction logic needs review")
	}

	// The misspelled key must NOT be in config.go.
	if cfgKeys["codex_fallback_modle"] {
		t.Fatal("'codex_fallback_modle' appears in config.go — either fix the typo in config.go or update the test fixture")
	}

	// Simulate what TestReadmeConfigKeys would do: any extracted key not in cfgKeys → violation.
	var violations []string
	for _, k := range keys {
		if !cfgKeys[k] {
			violations = append(violations, k)
		}
	}
	if len(violations) == 0 {
		t.Fatal("expected at least one violation (the misspelled key) but found none")
	}
}
