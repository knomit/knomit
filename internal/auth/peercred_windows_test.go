//go:build windows

package auth

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// acceptOne starts a listener at path and hands back the accepted server-side
// conn for whatever the dial function produces. Both conns are closed by the
// test's cleanup.
func acceptOne(t *testing.T, path string, dial func(context.Context, string) (net.Conn, error)) net.Conn {
	t.Helper()
	l, err := ListenLocal(path)
	if err != nil {
		t.Fatalf("ListenLocal(%q): %v", path, err)
	}
	t.Cleanup(func() { l.Close() })
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()
	client, err := dial(context.Background(), path)
	if err != nil {
		t.Fatalf("dial %q: %v", path, err)
	}
	t.Cleanup(func() { client.Close() })
	server := <-accepted
	if server == nil {
		t.Fatal("accept produced no connection; this fixture would prove nothing")
	}
	t.Cleanup(func() { server.Close() })
	return server
}

// The Windows counterpart of TestPeerCred_UnixSocketReportsOwnUID: the pipe
// reports the SID of the account on the other end, and the pid it reports is
// the real one the OS knows — not a number the client sent about itself.
func TestPeerCred_PipeReportsOwnSID(t *testing.T) {
	server := acceptOne(t, testLocalListenerPath(t), func(ctx context.Context, p string) (net.Conn, error) {
		return DialLocal(ctx, p, 10*time.Second)
	})

	peer, ok := PeerCred(server)
	if !ok {
		t.Fatal("PeerCred must succeed on a named pipe connection")
	}
	sid, err := ownSID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sid, "S-1-") {
		t.Fatalf("ownSID returned %q, which is not a SID", sid)
	}
	if peer.ID != "sid:"+sid {
		t.Fatalf("id = %q, want %q", peer.ID, "sid:"+sid)
	}
	if peer.Via != ViaPipe {
		t.Fatalf("via = %q, want %q", peer.Via, ViaPipe)
	}
	if peer.PID != os.Getpid() {
		t.Fatalf("pid = %d, want this process's %d (GetNamedPipeClientProcessId reports it)", peer.PID, os.Getpid())
	}
}

// NEGATIVE CONTROL for the impersonation level, and the reason DialLocal does
// not just call winio.DialPipeContext.
//
// DialPipeContext dials at PipeImpLevelAnonymous. At SECURITY_ANONYMOUS the
// server's OpenThreadToken fails with ERROR_CANT_OPEN_ANONYMOUS, so there is
// no SID to read and the caller silently becomes anonymous — a bridge that
// looks connected and holds none of its grants. This pins the failure so that
// a future change to DialLocal's level cannot pass unnoticed.
func TestPeerCred_AnonymousImpersonationLevelYieldsNoPeer(t *testing.T) {
	server := acceptOne(t, testLocalListenerPath(t), func(ctx context.Context, p string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return winio.DialPipeContext(ctx, p) // the winio default: Anonymous
	})

	if peer, ok := PeerCred(server); ok {
		t.Fatalf("a client dialling at PipeImpLevelAnonymous must yield NO peer, got %+v", peer)
	}

	// Positive control on the same fixture: the failure above is the
	// impersonation LEVEL and not something broken about the listener, which
	// an assertion of "not ok" alone could never tell apart.
	server2 := acceptOne(t, testLocalListenerPath(t), func(ctx context.Context, p string) (net.Conn, error) {
		return DialLocal(ctx, p, 10*time.Second)
	})
	if _, ok := PeerCred(server2); !ok {
		t.Fatal("the same listener must yield a peer for a client dialling at Identification level")
	}
}

// The failure the test above pins, named exactly: it is
// ERROR_CANT_OPEN_ANONYMOUS and not some generic denial, so a future reader
// diagnosing "no peer" has the errno to search for.
func TestReadClientSID_AnonymousFailsWithCantOpenAnonymous(t *testing.T) {
	server := acceptOne(t, testLocalListenerPath(t), func(ctx context.Context, p string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return winio.DialPipeContext(ctx, p)
	})
	pc, ok := server.(interface{ Fd() uintptr })
	if !ok {
		t.Fatalf("accepted conn %T exposes no Fd(); PeerCred could not read the handle either", server)
	}
	_, err := pipeClientSID(windows.Handle(pc.Fd()))
	if err == nil {
		t.Fatal("reading a SID at anonymous level must fail")
	}
	if !strings.Contains(err.Error(), "OpenThreadToken") {
		t.Fatalf("the failure must come from OpenThreadToken; got: %v", err)
	}
	t.Logf("anonymous-level failure, recorded verbatim: %v", err)
}
