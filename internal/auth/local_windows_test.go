//go:build windows

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unsafe"

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

// rawServerPipe creates a single-instance, overlapped, byte-mode server end at
// path with ListenLocal's own SDDL, so DialLocal can open it and reads off it
// exactly the owner it would read off a ListenLocal pipe. The handle is closed
// at cleanup; any I/O on it must be cancelled before then (overlappedIO does).
func rawServerPipe(t *testing.T, path string) windows.Handle {
	t.Helper()
	sddl, err := ownerOnlySDDL()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateNamedPipe(name,
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, 4096, 4096, 0, sa)
	if err != nil {
		t.Fatalf("CreateNamedPipe %s: %v", path, err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
	return h
}

// overlappedIO is ONE overlapped operation on h, signalled through its own
// event. The kernel writes into ov (and into buf, for a read) until the
// operation completes, so neither may go away while it is in flight: the
// cleanup, registered after the handle's and so run before it, cancels a
// still-pending operation and WAITS for the cancellation to land before it
// closes the event. That is what makes a t.Fatal on a timed-out wait safe.
type overlappedIO struct {
	h       windows.Handle
	ov      windows.Overlapped
	buf     []byte
	pending bool
}

func newOverlappedIO(t *testing.T, h windows.Handle, bufSize int) *overlappedIO {
	t.Helper()
	ev, err := windows.CreateEvent(nil, 1, 0, nil) // manual reset, unsignalled
	if err != nil {
		t.Fatal(err)
	}
	op := &overlappedIO{h: h, buf: make([]byte, bufSize)}
	op.ov.HEvent = ev
	t.Cleanup(func() {
		if op.pending {
			_ = windows.CancelIoEx(op.h, &op.ov)
			var n uint32
			_ = windows.GetOverlappedResult(op.h, &op.ov, &n, true)
		}
		windows.CloseHandle(ev)
	})
	return op
}

// wait takes issued, what the call that started the operation returned, and
// waits at most bound for the operation to finish. It returns the bytes moved
// and the operation's own outcome. Exceeding bound is a test failure naming
// what; the bound only turns a hang into that failure.
func (op *overlappedIO) wait(t *testing.T, issued error, bound time.Duration, what string) (uint32, error) {
	t.Helper()
	if issued != nil && !errors.Is(issued, windows.ERROR_IO_PENDING) {
		return 0, issued // failed synchronously; nothing is in flight
	}
	op.pending = true
	ev, err := windows.WaitForSingleObject(op.ov.HEvent, uint32(bound/time.Millisecond))
	if err != nil {
		t.Fatalf("waiting for %s: %v", what, err)
	}
	if ev == uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("%s did not finish within %v", what, bound)
	}
	var n uint32
	err = windows.GetOverlappedResult(op.h, &op.ov, &n, false)
	op.pending = false
	return n, err
}

// The squatting case of knomit#265. A pipe owned by ANOTHER account cannot be
// created without privileges this suite does not have, so the accepted set is
// narrowed to nothing instead: a pipe of our own then stands in for a foreign
// one, and everything downstream of the owner read is DialLocal's real path.
//
// WHY A RAW PIPE AND NOT ListenLocal + Accept (knomit#367). DialLocal's
// refusal connects, reads the owner and hangs up at once. go-winio's Accept
// drops a client that hangs up before the listener has issued
// ConnectNamedPipe on the instance it connected to (the connect then fails
// with ERROR_NO_DATA and the listener quietly waits for the next client), so
// a test waiting on Accept for the server end could hang forever; it did,
// twice, on Windows CI. See
// kb/gotchas/windows/named-pipes/accept-drops-early-hangup/51d80936.md.
//
// So the ConnectNamedPipe below is issued BEFORE the dial, on purpose: a
// connect that is already pending completes when the client connects, even if
// the client has hung up by the time anyone looks. The race cannot occur, and
// the server end is then read directly to see what DialLocal left behind.
//
// The bounds on the two waits are failure bounds, NOT the fix: they exist so
// that a DialLocal that never connects, or a read that never ends, fails with
// a message instead of hanging the package until the test binary times out.
func TestDialLocal_RejectsAForeignOwner(t *testing.T) {
	path := testLocalListenerPath(t)
	srv := rawServerPipe(t, path)
	connect := newOverlappedIO(t, srv, 0)
	issued := windows.ConnectNamedPipe(srv, &connect.ov)
	// ERROR_PIPE_CONNECTED means a client got in before the call, which is
	// "connected", not a failure; FILE_FLAG_FIRST_PIPE_INSTANCE and the
	// per-test name keep that client from being anyone but DialLocal.
	alreadyConnected := errors.Is(issued, windows.ERROR_PIPE_CONNECTED)
	if !alreadyConnected && issued != nil && !errors.Is(issued, windows.ERROR_IO_PENDING) {
		t.Fatalf("ConnectNamedPipe: %v", issued)
	}
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

	// DialLocal has returned, so if it connected at all the pending connect
	// has completed already; the bound only covers a DialLocal that refused
	// WITHOUT connecting, which would make the read below prove nothing.
	if !alreadyConnected {
		if _, err := connect.wait(t, issued, 10*time.Second, "the pending ConnectNamedPipe (DialLocal never connected)"); err != nil {
			t.Fatalf("ConnectNamedPipe completed with %v", err)
		}
	}

	// The connection was CLOSED, and closed before anything was written: the
	// server end reads no bytes and a broken pipe, which is how a pipe reports
	// that the client end hung up (there is no separate end-of-stream; measured,
	// ReadFile fails at once with ERROR_BROKEN_PIPE). A client that wrote first
	// would be read here as n > 0, so this also proves nothing was sent.
	read := newOverlappedIO(t, srv, 64)
	var done uint32
	issued = windows.ReadFile(srv, read.buf, &done, &read.ov)
	n, rerr := read.wait(t, issued, 5*time.Second, "ReadFile on the server end")
	if n != 0 || !errors.Is(rerr, windows.ERROR_BROKEN_PIPE) {
		t.Fatalf("the server end must read no bytes and ERROR_BROKEN_PIPE, got n=%d err=%v", n, rerr)
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
