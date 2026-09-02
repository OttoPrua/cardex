package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultProviderMaxParallel = 24

type providerPreflightState string

const (
	providerAuthMissing             providerPreflightState = "AUTH_MISSING"
	providerAuthExpiredRefreshable  providerPreflightState = "AUTH_EXPIRED_REFRESHABLE"
	providerNetworkProxyUnreachable providerPreflightState = "NETWORK_PROXY_UNREACHABLE"
	providerRateLimited             providerPreflightState = "RATE_LIMITED"
	providerTransportFailed         providerPreflightState = "TRANSPORT_FAILED"
	providerModelStartFailed        providerPreflightState = "MODEL_START_FAILED"
	providerModelUnavailable        providerPreflightState = "MODEL_UNAVAILABLE"
	providerReady                   providerPreflightState = "READY"
)

type ProviderPreflightReadback struct {
	Runner        string                 `json:"runner"`
	State         providerPreflightState `json:"state"`
	SelectedModel string                 `json:"selected_model,omitempty"`
	Reason        string                 `json:"reason,omitempty"`
	CircuitOpen   bool                   `json:"circuit_open,omitempty"`
	CheckedAt     string                 `json:"checked_at"`
}

// providerChildEnv is the only environment projection used by native provider processes. It reads
// only the named allowlist and never enumerates the parent environment, so credential values cannot
// be accidentally copied into a child or a diagnostic. Provider-specific non-secret overrides are
// added by the caller.
func providerChildEnv(home string, extra map[string]string) []string {
	keys := []string{
		"PATH", "TMPDIR", "TMP", "TEMP", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "NO_COLOR",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
		kimiLegacyEngineEnvFlag,
	}
	values := make(map[string]string, len(keys)+len(extra)+1)
	extraAllowed := map[string]bool{
		"NO_COLOR": true, "KIMI_CODE_NO_AUTO_UPDATE": true, "KIMI_CODE_HOME": true,
		"KIMI_MODEL_THINKING_EFFORT": true,
	}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	if strings.TrimSpace(home) != "" {
		values["HOME"] = home
	}
	for key, value := range extra {
		if extraAllowed[key] {
			values[key] = value
		}
	}
	keys = keys[:0]
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func providerParallelLimit(cfg *Config, runner string) int {
	limit := 0
	switch runner {
	case grokBuildRunnerName:
		if cfg != nil && cfg.GrokBuild != nil {
			limit = cfg.GrokBuild.MaxParallel
		}
	case kimiCLIRunnerName:
		if cfg != nil && cfg.KimiCLIOpus != nil {
			limit = cfg.KimiCLIOpus.MaxParallel
		}
	}
	if limit <= 0 {
		return defaultProviderMaxParallel
	}
	return limit
}

func classifyProviderPreflight(output string, runErr error) providerPreflightState {
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "expired credential"), strings.Contains(lower, "token expired"),
		strings.Contains(lower, "invalid or expired credentials"),
		strings.Contains(lower, "reauth"), strings.Contains(lower, "refresh token"),
		strings.Contains(lower, "401 unauthorized"):
		return providerAuthExpiredRefreshable
	case strings.Contains(lower, "not logged in"), strings.Contains(lower, "login required"),
		strings.Contains(lower, "no auth context"), strings.Contains(lower, "credentials missing"):
		return providerAuthMissing
	case strings.Contains(lower, "proxyconnect"), strings.Contains(lower, "proxy connection"),
		strings.Contains(lower, "proxy unreachable"),
		(strings.Contains(lower, "connection refused") && strings.Contains(lower, "127.0.0.1")):
		return providerNetworkProxyUnreachable
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "too many requests"),
		strings.Contains(lower, "quota exceeded"), strings.Contains(lower, "http 429"):
		return providerRateLimited
	case strings.Contains(lower, "model unavailable"), strings.Contains(lower, "model not found"),
		strings.Contains(lower, "failed to start model"):
		return providerModelStartFailed
	case runErr != nil:
		return providerTransportFailed
	default:
		return providerReady
	}
}

func modelListed(output, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	for _, field := range strings.FieldsFunc(strings.ToLower(output), func(r rune) bool {
		switch r {
		case ' ', '\t', '\r', '\n', ',', ';', '(', ')', '[', ']', '{', '}', '"', '\'':
			return true
		}
		return false
	}) {
		if strings.TrimSpace(field) == model {
			return true
		}
	}
	return false
}

func listedModels(output string) []string {
	var out []string
	seen := map[string]bool{}
	for _, field := range strings.FieldsFunc(strings.ToLower(output), func(r rune) bool {
		switch r {
		case ' ', '\t', '\r', '\n', ',', ';', '(', ')', '[', ']', '{', '}', '"', '\'':
			return true
		}
		return false
	}) {
		field = strings.TrimSpace(field)
		if field != "" && !seen[field] {
			seen[field] = true
			out = append(out, field)
		}
	}
	return out
}

func highestAdvertisedOpus(output string) (string, bool) {
	best := ""
	var bestVersion []int
	for _, model := range listedModels(output) {
		if !strings.HasPrefix(model, "claude-opus-") {
			continue
		}
		version := opusModelVersion(model)
		if best == "" || compareNumericVersion(version, bestVersion) > 0 ||
			(compareNumericVersion(version, bestVersion) == 0 && model > best) {
			best = model
			bestVersion = version
		}
	}
	return best, best != ""
}

func opusModelVersion(model string) []int {
	parts := strings.Split(strings.TrimPrefix(strings.ToLower(model), "claude-opus-"), "-")
	version := make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			break
		}
		version = append(version, n)
	}
	return version
}

func compareNumericVersion(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func runAntigravityPreflight(ctx context.Context, cfg *Config, t *Task, env []string) ProviderPreflightReadback {
	checked := time.Now().UTC().Format(time.RFC3339Nano)
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, cfg.AntigravityBin, "models")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if probeCtx.Err() == context.DeadlineExceeded {
		err = probeCtx.Err()
	}
	state := classifyProviderPreflight(stdout.String()+"\n"+stderr.String(), err)
	r := ProviderPreflightReadback{Runner: antigravityRunnerName, State: state, CheckedAt: checked}
	if state != providerReady {
		return r
	}
	if t != nil && strings.TrimSpace(t.AgyModel) != "" {
		if modelListed(stdout.String(), t.AgyModel) {
			r.SelectedModel = strings.TrimSpace(t.AgyModel)
			return r
		}
		r.State = providerModelUnavailable
		return r
	}
	if model, ok := highestAdvertisedOpus(stdout.String()); ok {
		r.SelectedModel = model
		return r
	}
	r.State = providerModelUnavailable
	return r
}

func runProviderPreflight(ctx context.Context, root string, cfg *Config, t *Task, runner string) ProviderPreflightReadback {
	home, _ := os.UserHomeDir()
	switch runner {
	case grokBuildRunnerName:
		model := resolveGrokBuildModel(cfg, t)
		r := ProviderPreflightReadback{Runner: runner, SelectedModel: model, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		err := ensureGrokBuildAuth(ctx, root, cfg, model)
		if err == nil {
			r.State = providerReady
			return r
		}
		var authErr *grokBuildAuthProbeError
		if errors.As(err, &authErr) {
			r.State = providerAuthExpiredRefreshable
			r.Reason = authErr.reason
			r.CircuitOpen = authErr.circuitOpen
			return r
		}
		if strings.Contains(err.Error(), "模型清单缺") {
			r.State = providerModelUnavailable
			return r
		}
		r.State = classifyProviderPreflight(err.Error(), err)
		return r
	case kimiCLIRunnerName:
		r := ProviderPreflightReadback{Runner: runner, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if cfg == nil || strings.TrimSpace(cfg.KimiCLIBin) == "" {
			r.State = providerTransportFailed
			return r
		}
		sourceHome := strings.TrimSpace(cfg.KimiCLIHome)
		if sourceHome == "" {
			sourceHome = home + string(os.PathSeparator) + ".kimi-code"
		}
		if _, err := os.Stat(sourceHome + string(os.PathSeparator) + "credentials"); err != nil {
			r.State = providerAuthMissing
			return r
		}
		if limit, err := currentOpenFileSoftLimit(); err != nil || validateKimiCLIOpenFileLimit(limit) != nil {
			r.State = providerTransportFailed
			return r
		}
		r.State = providerReady
		r.SelectedModel = resolveKimiCLIModel(cfg, t)
		return r
	case antigravityRunnerName:
		return runAntigravityPreflight(ctx, cfg, t, providerChildEnv(home, nil))
	default:
		return ProviderPreflightReadback{Runner: runner, State: providerReady, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	}
}

func providerPreflightReady(t *Task, runner string) bool {
	return t != nil && t.LastProviderPreflight != nil && t.LastProviderPreflight.Runner == runner &&
		t.LastProviderPreflight.State == providerReady
}
