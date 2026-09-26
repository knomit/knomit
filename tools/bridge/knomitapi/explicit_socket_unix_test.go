//go:build !windows

package knomitapi

import (
	"path/filepath"
	"testing"
)

// explicitSocket is an explicit local listener value this platform accepts:
// an absolute socket path on unix. Nothing listens on it.
func explicitSocket(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name+".sock")
}
