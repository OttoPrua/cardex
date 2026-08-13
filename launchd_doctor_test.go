package main

import (
	"strings"
	"testing"
)

func TestValidateLaunchdDoctorOutputRejectsSigningPolicyDrift(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   string
	}{
		{
			name: "codesigning rejection",
			output: `state = not running
last exit reason = OS_REASON_CODESIGNING
properties = runatload | managed LWCR | has LWCR`,
			want: "OS_REASON_CODESIGNING",
		},
		{
			name: "stale lightweight code requirement",
			output: `state = not running
properties = runatload | needs LWCR update | managed LWCR | has LWCR`,
			want: "needs LWCR update",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLaunchdDoctorOutput(tc.output)
			if err == nil {
				t.Fatalf("expected unhealthy launchd output to fail doctor")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateLaunchdDoctorOutputAcceptsIdleSuccessfulTimer(t *testing.T) {
	t.Parallel()

	output := `state = not running
runs = 1
last exit code = 0
run interval = 300 seconds
properties = runatload | inferred program`
	if err := validateLaunchdDoctorOutput(output); err != nil {
		t.Fatalf("idle periodic timer with a successful last run should be healthy: %v", err)
	}
}
