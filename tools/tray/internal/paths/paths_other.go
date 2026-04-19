//go:build !darwin

package paths

import (
	"fmt"
	"runtime"
)

func stateDir() (string, error) {
	return "", fmt.Errorf("paths: unsupported platform %s (phase 1 is macOS only)", runtime.GOOS)
}

func logsDir() (string, error) {
	return "", fmt.Errorf("paths: unsupported platform %s (phase 1 is macOS only)", runtime.GOOS)
}
