//go:build windows

package config

import (
	"testing"

	"knomit/internal/auth"
)

// ExplicitSocket is an explicit local listener value this platform accepts:
// a named pipe on Windows. Nothing listens on it.
func ExplicitSocket(t *testing.T, name string) string {
	t.Helper()
	return auth.PipePrefix + "knomit-test-" + name
}
