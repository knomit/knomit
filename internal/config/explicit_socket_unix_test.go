//go:build !windows

package config

import (
	"path/filepath"
	"testing"
)

// ExplicitSocket is an explicit local listener value this platform accepts:
// an absolute socket path on unix. Nothing listens on it.
func ExplicitSocket(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name+".sock")
}
