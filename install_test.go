package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCleanBuildProvenanceRejectsDirtyOrUnknownBuild(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		prov buildProvenance
		want string
	}{
		{name: "dirty", prov: buildProvenance{Revision: strings.Repeat("a", 40), Modified: true, ModifiedKnown: true}, want: "vcs.modified=true"},
		{name: "missing dirty marker", prov: buildProvenance{Revision: strings.Repeat("a", 40)}, want: "vcs.modified"},
		{name: "missing revision", prov: buildProvenance{}, want: "vcs.revision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCleanBuildProvenance(tc.prov)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateCleanBuildProvenance(%+v) = %v, want error containing %q", tc.prov, err, tc.want)
			}
		})
	}

	clean := buildProvenance{Revision: strings.Repeat("b", 40), ModifiedKnown: true}
	if err := validateCleanBuildProvenance(clean); err != nil {
		t.Fatalf("clean committed build rejected: %v", err)
	}
}

func TestValidateInstallPreflightBindsSourceAndInstalledPreimage(t *testing.T) {
	t.Parallel()

	target := filepath.Join(t.TempDir(), "cardex")
	current := []byte("current production bytes")
	if err := os.WriteFile(target, current, 0o755); err != nil {
		t.Fatal(err)
	}
	currentSHA := fmt.Sprintf("%x", sha256.Sum256(current))
	revision := strings.Repeat("c", 40)
	clean := buildProvenance{Revision: revision, ModifiedKnown: true}

	if err := validateInstallPreflight(clean, target, revision, currentSHA); err != nil {
		t.Fatalf("matching clean source and target preimage rejected: %v", err)
	}
	if err := validateInstallPreflight(buildProvenance{Revision: revision, Modified: true, ModifiedKnown: true}, target, revision, currentSHA); err == nil {
		t.Fatal("dirty source build must be rejected")
	}
	if err := validateInstallPreflight(clean, target, strings.Repeat("d", 40), currentSHA); err == nil {
		t.Fatal("unexpected source revision must be rejected")
	}
	if err := validateInstallPreflight(clean, target, revision, strings.Repeat("0", 64)); err == nil {
		t.Fatal("changed installed preimage must be rejected")
	}

	absent := filepath.Join(t.TempDir(), "cardex")
	if err := validateInstallPreflight(clean, absent, revision, "absent"); err != nil {
		t.Fatalf("explicit fresh install rejected: %v", err)
	}
}

func TestMakeInstallRunsVerifiedPreflightBeforeReplacement(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	preflight := strings.Index(text, "$(BIN) install-preflight")
	replace := strings.Index(text, "rm -f $(PREFIX)/cardex")
	if preflight < 0 || replace < 0 || preflight > replace {
		t.Fatalf("install must run binary preflight before removing production target: preflight=%d replace=%d", preflight, replace)
	}
	for _, required := range []string{"CARDEX_INSTALL_EXPECTED_HEAD", "CARDEX_INSTALL_EXPECTED_CURRENT_SHA256"} {
		if !strings.Contains(text, required) {
			t.Fatalf("Makefile install is missing explicit guard %s", required)
		}
	}
	for _, required := range []string{"-X main.buildRevision", "-X main.buildModified"} {
		if !strings.Contains(text, required) {
			t.Fatalf("Makefile build is missing explicit provenance injection %s", required)
		}
	}
}
