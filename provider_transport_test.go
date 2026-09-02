package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderChildEnvProjectsOnlyTransportAndNativeHome(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	t.Setenv("NO_PROXY", "localhost")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-leak")
	t.Setenv("OPENAI_API_KEY", "must-not-leak")

	env := providerChildEnv("/native/auth/home", map[string]string{"NO_COLOR": "1", "OPENAI_API_KEY": "extra-must-not-leak"})
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{
		"\nHOME=/native/auth/home\n",
		"\nHTTPS_PROXY=http://127.0.0.1:7890\n",
		"\nNO_PROXY=localhost\n",
		"\nNO_COLOR=1\n",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing allowlisted child environment entry %q in %q", want, env)
		}
	}
	for _, forbidden := range []string{"AWS_SECRET_ACCESS_KEY=", "OPENAI_API_KEY=", "must-not-leak", "extra-must-not-leak"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("secret-bearing parent environment leaked through provider projection: %q", env)
		}
	}
}

func TestProviderPreflightStatesAreClosedAndValueBlind(t *testing.T) {
	cases := []struct {
		name   string
		output string
		err    error
		want   providerPreflightState
	}{
		{"missing", "login required", os.ErrPermission, providerAuthMissing},
		{"expired-before-missing-substring", "Invalid or expired credentials (reason=no auth context)", os.ErrPermission, providerAuthExpiredRefreshable},
		{"proxy", "proxyconnect tcp: connection refused 127.0.0.1", os.ErrPermission, providerNetworkProxyUnreachable},
		{"rate", "HTTP 429: rate limit", os.ErrPermission, providerRateLimited},
		{"model", "model not found", os.ErrPermission, providerModelStartFailed},
		{"transport", "unexpected failure", os.ErrPermission, providerTransportFailed},
		{"ready", "ok", nil, providerReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProviderPreflight(tc.output, tc.err); got != tc.want {
				t.Fatalf("classifyProviderPreflight()=%q want %q", got, tc.want)
			}
		})
	}
}

func TestProviderParallelLimitsDefaultAndOverrideIndependently(t *testing.T) {
	cfg := &Config{
		GrokBuild:   &GrokBuildRoute{},
		KimiCLIOpus: &KimiCLIOpusRoute{},
	}
	if got := providerParallelLimit(cfg, grokBuildRunnerName); got != 24 {
		t.Fatalf("Grok default cap=%d want 24", got)
	}
	if got := providerParallelLimit(cfg, kimiCLIRunnerName); got != 24 {
		t.Fatalf("Kimi default cap=%d want 24", got)
	}
	cfg.GrokBuild.MaxParallel = 7
	cfg.KimiCLIOpus.MaxParallel = 11
	if got := providerParallelLimit(cfg, grokBuildRunnerName); got != 7 {
		t.Fatalf("Grok override cap=%d want 7", got)
	}
	if got := providerParallelLimit(cfg, kimiCLIRunnerName); got != 11 {
		t.Fatalf("Kimi override cap=%d want 11", got)
	}
}

func TestAntigravityPreflightChoosesHighestActuallyAdvertisedOpus(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agy")
	script := "#!/bin/sh\nprintf '%s\\n' 'claude-sonnet-4-6' 'claude-opus-4-6-thinking' 'claude-opus-4-10-thinking'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig("")
	cfg.AntigravityBin = bin
	cfg.Antigravity = &AntigravityRoute{Enabled: true}
	r := runAntigravityPreflight(context.Background(), cfg, &Task{}, providerChildEnv(dir, nil))
	if r.State != providerReady || r.SelectedModel != "claude-opus-4-10-thinking" {
		t.Fatalf("dynamic Antigravity route readback mismatch: %+v", r)
	}
}

func TestAntigravityPreflightDoesNotFallBackToSonnet(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' 'claude-sonnet-4-6'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig("")
	cfg.AntigravityBin = bin
	cfg.Antigravity = &AntigravityRoute{Enabled: true}
	r := runAntigravityPreflight(context.Background(), cfg, &Task{}, providerChildEnv(dir, nil))
	if r.State != providerModelUnavailable || r.SelectedModel != "" {
		t.Fatalf("missing Opus must hold MODEL_UNAVAILABLE without Sonnet substitution: %+v", r)
	}
}
