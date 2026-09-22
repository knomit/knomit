//go:build windows

package auth

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// The WINDOWS half of the ListenLocal tests. It asks the same QUESTIONS as
// listen_unix_test.go — can a second instance steal a live listener, does
// cleanup release it, is a nonsense path refused — of the thing this platform
// actually has. It does not translate that file's fixtures, because they are
// about a socket FILE: a 0600 mode, a stale inode left by a crash, a flock to
// decide liveness. A pipe has none of those. Its name exists only while an
// instance is open, so there is nothing to leave behind and nothing to lock.

// A live pipe must not be stolen, and the refusal must be ErrSocketInUse
// specifically — both cmd/serve.go and tools/desktop/boot.go match on it with
// errors.Is and serve TCP only, where any other error is fatal. Getting the
// class wrong would turn "the desktop already owns this" into a dead server.
func TestListenLocal_LivePipeIsNotStolen(t *testing.T) {
	path := testLocalListenerPath(t)
	first, cleanup1, err := ListenLocal(path)
	if err != nil || first == nil {
		t.Fatalf("first ListenLocal: %v %v", first, err)
	}
	defer cleanup1()

	second, cleanup2, err := ListenLocal(path)
	if second != nil {
		defer second.Close()
	}
	if !errors.Is(err, ErrSocketInUse) || second != nil {
		t.Fatalf("a second ListenLocal on a LIVE pipe must refuse with ErrSocketInUse: %v %v", second, err)
	}
	// The errno is RECORDED, not asserted blind: isPipeNameTaken maps it, and
	// a future Windows build that changed it would otherwise turn this refusal
	// into a fatal error at every desktop-plus-serve boot.
	t.Logf("second-listener refusal, recorded verbatim: %v", err)
	cleanup2() // the noop: must not disturb the first listener

	// POSITIVE CONTROL: the first listener still owns the name and still
	// yields a verified peer. "The second one failed" alone would also be
	// satisfied by both of them being broken.
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := first.Accept(); accepted <- c }()
	client, err := DialLocal(context.Background(), path, 10*time.Second)
	if err != nil {
		t.Fatalf("the live owner was disturbed: %v", err)
	}
	defer client.Close()
	srv := <-accepted
	if srv == nil {
		t.Fatal("the live owner accepted nothing")
	}
	defer srv.Close()
	peer, ok := PeerCred(srv)
	if !ok || peer.ID != localID(mustOwnSID(t)) {
		t.Fatalf("PeerCred over the surviving listener: %+v ok=%v", peer, ok)
	}
	if peer.PID != os.Getpid() {
		t.Fatalf("peer pid = %d, want %d", peer.PID, os.Getpid())
	}
}

// The counterpart, and what makes the test above a statement about LIVENESS
// rather than about the name being permanently burnt: once the owner releases
// it, the name is free again. On unix this needs a stale-socket fixture; here
// the OS does it, which is the whole reason there is no lock file.
func TestListenLocal_PipeNameIsFreeAfterCleanup(t *testing.T) {
	path := testLocalListenerPath(t)
	first, cleanup1, err := ListenLocal(path)
	if err != nil || first == nil {
		t.Fatalf("first ListenLocal: %v %v", first, err)
	}
	// Precondition: it really is taken while the first one holds it.
	if _, c, err := ListenLocal(path); !errors.Is(err, ErrSocketInUse) {
		c()
		t.Fatalf("fixture: the name was not taken to begin with (%v), so its release proves nothing", err)
	}
	cleanup1()
	cleanup1() // idempotent

	second, cleanup2, err := ListenLocal(path)
	if err != nil || second == nil {
		t.Fatalf("after cleanup the pipe name must be free again: %v %v", second, err)
	}
	defer cleanup2()

	// And the successor is the one that answers now.
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := second.Accept(); accepted <- c }()
	client, err := DialLocal(context.Background(), path, 10*time.Second)
	if err != nil {
		t.Fatalf("the successor is not reachable: %v", err)
	}
	defer client.Close()
	srv := <-accepted
	if srv == nil {
		t.Fatal("the successor's Accept did not receive the dial")
	}
	defer srv.Close()
	if _, ok := PeerCred(srv); !ok {
		t.Fatal("the successor's connection carried no verified peer")
	}
}

// A path outside the pipe namespace must be an ERROR, and not the (nil, noop,
// nil) that an empty path gets. CreateFile would open an ordinary path as a
// FILE, so a server handed one would report a listener and accept nothing.
func TestListenLocal_RefusesAPathOutsideThePipeNamespace(t *testing.T) {
	notAPipe := t.TempDir() + `\knomit.sock`
	ln, cleanup, err := ListenLocal(notAPipe)
	if cleanup != nil {
		cleanup()
	}
	if err == nil || ln != nil {
		t.Fatal("a non-pipe path must be refused, not opened as a file")
	}
	if errors.Is(err, ErrSocketInUse) {
		t.Fatalf("a malformed path is a misconfiguration, not a live owner; the caller would serve TCP and say nothing: %v", err)
	}
	if !strings.Contains(err.Error(), PipePrefix) {
		t.Fatalf("the error must name the namespace it wanted; got: %v", err)
	}
	if _, derr := DialLocal(context.Background(), notAPipe, time.Second); derr == nil {
		t.Fatal("DialLocal must refuse a non-pipe path too")
	}

	// POSITIVE CONTROL: the same call succeeds on a real pipe path, so the
	// refusal above is the guard firing and not ListenLocal being broken.
	good, gcleanup, gerr := ListenLocal(testLocalListenerPath(t))
	if gerr != nil || good == nil {
		t.Fatalf("ListenLocal on a real pipe path: %v %v", good, gerr)
	}
	gcleanup()
}

func mustOwnSID(t *testing.T) string {
	t.Helper()
	sid, err := ownSID()
	if err != nil {
		t.Fatal(err)
	}
	return sid
}
