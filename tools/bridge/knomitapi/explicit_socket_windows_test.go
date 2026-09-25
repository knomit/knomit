//go:build windows

package knomitapi

import (
	"testing"

	"knomit/internal/auth"
)

// explicitSocket is an explicit local listener value this platform accepts:
// a named pipe on Windows. Nothing listens on it.
func explicitSocket(t *testing.T, name string) string {
	t.Helper()
	return auth.PipePrefix + "knomit-test-" + name
}
