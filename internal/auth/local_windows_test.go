//go:build windows

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
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
	if !strings.Contains(sddl, "D:P(") {
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

// The pipe must NAME its owner. Without the O: part the owner is the creating
// token's TokenOwner, which for a UAC-elevated server is
// BUILTIN\Administrators, and DialLocal's owner check expects the user.
//
// This is the discriminator that holds under EVERY token. The listener test
// below discriminates only when the suite runs UAC-elevated (measured: without
// O: it then reads S-1-5-32-544); from an unelevated shell, or an elevated one
// that is not a UAC split token, TokenOwner already IS the user and a pipe
// created without O: would still come out owned by us.
func TestOwnerOnlySDDL_NamesTheOwner(t *testing.T) {
	sddl, err := ownerOnlySDDL()
	if err != nil {
		t.Fatal(err)
	}
	if want := "O:" + mustOwnSID(t) + "D:"; !strings.HasPrefix(sddl, want) {
		t.Fatalf("the SDDL must name our own SID as owner (prefix %q); got %q", want, sddl)
	}
}

// What the client actually sees: the owner read off the CLIENT's handle, the
// same call verifyPipeOwner makes, is our own SID. See the test above for
// which tokens this can tell apart from a pipe with no O: part.
func TestListenLocal_PipeIsOwnedByTheCaller(t *testing.T) {
	path := testLocalListenerPath(t)
	ln, cleanup, err := ListenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	conn, err := winio.DialPipeAccessImpLevel(context.Background(), path,
		uint32(windows.GENERIC_READ|windows.GENERIC_WRITE), winio.PipeImpLevelIdentification)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fder, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		t.Fatalf("a dialled pipe (%T) exposes no Fd()", conn)
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(fder.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := owner.String(), mustOwnSID(t); got != want {
		t.Fatalf("the pipe is owned by %s, want our own SID %s", got, want)
	}
}

// narrowPipeOwners replaces the accepted-owner set for one test.
func narrowPipeOwners(t *testing.T, accept func(owner *windows.SID, self string) bool) {
	t.Helper()
	orig := acceptedPipeOwner
	acceptedPipeOwner = accept
	t.Cleanup(func() { acceptedPipeOwner = orig })
}

// The squatting case of knomit#265. A pipe owned by ANOTHER account cannot be
// created without privileges this suite does not have, so the accepted set is
// narrowed to nothing instead: our own real listener then stands in for a
// foreign one, and everything downstream of the owner read is the real path.
func TestDialLocal_RejectsAForeignOwner(t *testing.T) {
	path := testLocalListenerPath(t)
	ln, cleanup, err := ListenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			c = nil
		}
		accepted <- c
	}()
	narrowPipeOwners(t, func(*windows.SID, string) bool { return false })

	conn, err := DialLocal(context.Background(), path, 10*time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("DialLocal returned a connection to a listener whose owner is not accepted")
	}
	if !errors.Is(err, ErrForeignListener) {
		t.Fatalf("want ErrForeignListener in the chain, got %v", err)
	}
	sid := mustOwnSID(t)
	msg := err.Error()
	for _, want := range []string{path, "owned by " + sid, "not " + sid} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the error must name %q; got %q", want, msg)
		}
	}

	// The connection was CLOSED, and closed before anything was written: the
	// server end sees end-of-stream and no bytes.
	srv := <-accepted
	if srv == nil {
		t.Fatal("the listener accepted nothing, so this proves nothing about the close")
	}
	defer srv.Close()
	_ = srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, rerr := srv.Read(make([]byte, 64))
	if n != 0 || !errors.Is(rerr, io.EOF) {
		t.Fatalf("the server end must see EOF with no bytes, got n=%d err=%v", n, rerr)
	}
}

// The accepted set itself: us, SYSTEM and BUILTIN\Administrators, and nobody
// else. This is the gate a genuinely foreign owner meets, which the suite
// cannot produce as a real pipe.
func TestAcceptedPipeOwner_TheDefaultSet(t *testing.T) {
	self := mustOwnSID(t)
	for _, c := range []struct {
		sid  string
		want bool
	}{
		{self, true},
		{"S-1-5-18", true},      // SYSTEM
		{"S-1-5-32-544", true},  // BUILTIN\Administrators
		{"S-1-5-19", false},     // LOCAL SERVICE: another account
		{"S-1-1-0", false},      // Everyone
		{"S-1-5-32-545", false}, // BUILTIN\Users
	} {
		sid, err := windows.StringToSid(c.sid)
		if err != nil {
			t.Fatal(err)
		}
		if got := acceptedPipeOwner(sid, self); got != c.want {
			t.Errorf("acceptedPipeOwner(%s) = %v, want %v", c.sid, got, c.want)
		}
	}
}
