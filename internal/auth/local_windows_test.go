//go:build windows

package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// testLocalListenerPath is a pipe name for one test's own listener. Pipe
// names have no sun_path-style length cap, but they DO share one flat
// machine-wide namespace, so it has to be unique per test: t.TempDir() is
// unique per test and per run, and hashing it keeps the name short and free
// of characters the namespace does not take.
func testLocalListenerPath(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte(t.TempDir()))
	return PipePrefix + "knomit-test-" + hex.EncodeToString(sum[:8])
}

// The pipe namespace is flat and machine-wide, so two tests sharing a name
// would have the second fail to listen (or, worse, connect to the first).
// This is the positive control on the fixture above.
func TestTestLocalListenerPath_IsUniquePerTest(t *testing.T) {
	a := testLocalListenerPath(t)
	var b string
	t.Run("sub", func(t *testing.T) { b = testLocalListenerPath(t) })
	if a == b {
		t.Fatalf("two tests got the same pipe name %q", a)
	}
	if !strings.HasPrefix(a, PipePrefix) {
		t.Fatalf("pipe name %q does not start with %q", a, PipePrefix)
	}
}

// The SDDL is the credential. It must name THIS user's SID and SYSTEM, be
// protected against inherited ACEs, and contain no deny ace.
func TestOwnerOnlySDDL_IsProtectedAndNamesUsAndSystem(t *testing.T) {
	sddl, err := ownerOnlySDDL()
	if err != nil {
		t.Fatal(err)
	}
	sid, err := ownSID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sid, "S-1-") {
		t.Fatalf("ownSID returned %q, which is not a SID", sid)
	}
	if !strings.HasPrefix(sddl, "D:P(") {
		t.Fatalf("the DACL must be protected (D:P) so no inherited ACE can widen it; got %q", sddl)
	}
	if !strings.Contains(sddl, "(A;;GA;;;"+sid+")") {
		t.Fatalf("the SDDL %q does not grant our own SID %q", sddl, sid)
	}
	if !strings.Contains(sddl, "(A;;GA;;;SY)") {
		t.Fatalf("the SDDL %q does not grant SYSTEM", sddl)
	}
	if strings.Contains(sddl, "(D;") {
		t.Fatalf("a deny ACE is evaluated first and could lock us out; got %q", sddl)
	}
}
