//go:build windows

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
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

// A path outside the pipe namespace must be REFUSED, not passed through.
// CreateFile would open a regular file at an ordinary path, so a server
// handed one would "listen" on a file: it would look up and accept nothing.
// The same guard on the dial side keeps a misconfigured bridge from opening
// whatever happens to be at that path.
func TestListenLocal_RefusesAPathOutsideThePipeNamespace(t *testing.T) {
	notAPipe := t.TempDir() + `\knomit.sock`
	l, err := ListenLocal(notAPipe)
	if err == nil {
		l.Close()
		t.Fatal("a non-pipe path must be refused, not opened as a file")
	}
	if !strings.Contains(err.Error(), PipePrefix) {
		t.Fatalf("the error must name the namespace it wanted; got: %v", err)
	}

	// Positive control: the SAME call succeeds on a real pipe path, so the
	// refusal above is the guard firing and not ListenLocal being broken.
	good, gerr := ListenLocal(testLocalListenerPath(t))
	if gerr != nil {
		t.Fatalf("ListenLocal on a real pipe path: %v", gerr)
	}
	good.Close()

	if _, derr := DialLocal(context.Background(), notAPipe, time.Second); derr == nil {
		t.Fatal("DialLocal must refuse a non-pipe path too")
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
