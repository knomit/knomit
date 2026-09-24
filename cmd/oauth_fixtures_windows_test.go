//go:build windows

package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"knomit/internal/auth"
)

// oauthLocalListenerPath is a pipe name for one test's exclusive use: the
// pipe namespace is flat and machine-wide, so uniqueness comes from hashing
// the test's own temp dir.
func oauthLocalListenerPath(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte(t.TempDir()))
	return auth.PipePrefix + "knomit-ko-" + hex.EncodeToString(sum[:8])
}
