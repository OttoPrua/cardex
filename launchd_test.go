package main

import (
	"strings"
	"testing"
)

func TestRenderLaunchdPlistSetsKimiOpenFileCeiling(t *testing.T) {
	got := renderLaunchdPlist("/opt/homebrew/bin/cardex", "/tmp/cardex-root", 300, "/tmp/cardex.log")
	for _, want := range []string{
		"<key>SoftResourceLimits</key>",
		"<key>NumberOfFiles</key><integer>65536</integer>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("launchd plist missing %q:\n%s", want, got)
		}
	}
	limit, err := launchdOpenFileLimit([]byte(got))
	if err != nil || limit != cardexTickOpenFileLimit {
		t.Fatalf("unexpected launchd open-file limit: limit=%d err=%v", limit, err)
	}
}

func TestLaunchdOpenFileLimitRejectsMissingOrLowValue(t *testing.T) {
	if _, err := launchdOpenFileLimit([]byte("<plist><dict></dict></plist>")); err == nil {
		t.Fatal("missing SoftResourceLimits.NumberOfFiles must fail")
	}
	low := []byte("<key>SoftResourceLimits</key><dict><key>NumberOfFiles</key><integer>256</integer></dict>")
	limit, err := launchdOpenFileLimit(low)
	if err != nil || limit != 256 {
		t.Fatalf("low limit must remain diagnosable: limit=%d err=%v", limit, err)
	}
	if err := validateKimiCLIOpenFileLimit(limit); err == nil || !strings.Contains(err.Error(), "EMFILE") {
		t.Fatalf("low limit must produce concrete EMFILE diagnosis: %v", err)
	}
}
