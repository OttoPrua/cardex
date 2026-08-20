//go:build windows

package main

import "math"

// Windows does not expose POSIX RLIMIT_NOFILE. The launchd-specific ceiling and Kimi
// fs.watch failure only apply to macOS; keep the cross-compiled runner unrestricted.
func currentOpenFileSoftLimit() (uint64, error) {
	return math.MaxUint64, nil
}
