//go:build windows

package authz

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"knomit/internal/foundation/auth"
)

// testLocalListenerPath is a pipe name for one test's own listener. The pipe
// namespace is flat and machine-wide, so the name is unique per test (a hash
// of t.TempDir(), as auth's helper of the same name does).
func testLocalListenerPath(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte(t.TempDir()))
	return auth.PipePrefix + "knomit-authz-test-" + hex.EncodeToString(sum[:8])
}
