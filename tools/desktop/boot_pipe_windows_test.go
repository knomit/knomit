//go:build desktop && windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"knomit/internal/auth"
)

// The WINDOWS half of the desktop's local-listener tests; the unix half is
// boot_socket_unix_test.go. Same three questions — does bootServer open the
// listener AND attach a credential, does it survive another instance owning
// it, does a failure to open it suppress the lockfile — asked of a named pipe
// (knomit#245). They are not that file's fixtures translated: those are about
// a socket FILE (0600 mode, os.SameFile, a leftover inode), none of which a
// pipe has.
//
// This matters beyond symmetry: since #248 the desktop opens cfg.Socket
// itself, and on Windows cfg.Socket is now a pipe. The desktop IS the normal
// install, so this is the path most users' bridges will actually take.

// testPipePath is a pipe name for one test's exclusive use. The namespace is
// flat and machine-wide, so uniqueness has to be manufactured: t.TempDir() is
// unique per test and per run.
func testPipePath(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte(t.TempDir()))
	return auth.PipePrefix + "knomit-desktop-test-" + hex.EncodeToString(sum[:8])
}

// Issue #248 on Windows: the desktop's own http.Server must open the pipe AND
// attach the OS's peer credential, or bridges stay anonymous on the normal
// install.
func TestBootServer_OpensLocalPipeWithPeerCreds(t *testing.T) {
	pipe := testPipePath(t)
	lockPath := filepath.Join(t.TempDir(), "server.json")
	srv, port, err := bootServer(context.Background(), peerEcho, lockPath, "v", "", pipe)
	if err != nil {
		t.Fatal(err)
	}

	// Port 1: nothing listens there, so ONLY the pipe can answer this.
	want := wantOwnPeer(t)
	if got := getBody(t, localClient(pipe), "http://localhost:1/x"); got != want {
		srv.shutdown()
		t.Fatalf("over the pipe: got %q, want %q", got, want)
	}
	// Positive control for the TCP side: same handler, no peer credential.
	// Without it, a handler that always printed ok=false would pass above.
	if got := getBody(t, http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/x", port)); got != wantNoPeer {
		srv.shutdown()
		t.Fatalf("over TCP: got %q, want no peer", got)
	}

	srv.shutdown()

	// What closeSocket has to achieve on Windows is that the NAME is free
	// again: a relaunch (NativeService.RestartApp) that found it still taken
	// would serve TCP only, anonymous, until the next restart. Take the name
	// the way that successor would.
	ln, release, err := auth.ListenLocal(pipe)
	if err != nil || ln == nil {
		t.Fatalf("successor could not take the pipe after shutdown: %v %v", ln, err)
	}
	release()
}

// A live instance (normally `knomit serve`) already owns the pipe: the desktop
// must still boot on TCP and leave that listener alone.
func TestBootServer_PipeInUseServesTCPOnly(t *testing.T) {
	pipe := testPipePath(t)
	owner, release, err := auth.ListenLocal(pipe)
	if err != nil || owner == nil {
		t.Fatalf("owner ListenLocal: %v %v", owner, err)
	}
	defer release()

	lockPath := filepath.Join(t.TempDir(), "server.json")
	srv, port, err := bootServer(context.Background(), peerEcho, lockPath, "v", "", pipe)
	if err != nil {
		t.Fatalf("a pipe in use must not fail the boot: %v", err)
	}
	defer srv.shutdown()
	if got := getBody(t, http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/x", port)); got != wantNoPeer {
		t.Fatalf("TCP must still serve: got %q", got)
	}

	// The owner still owns it. os.SameFile has no meaning here, so this is
	// checked the only way a pipe allows: the owner still accepts, and the
	// connection still carries a credential.
	accepted := make(chan bool, 1)
	go func() {
		c, aerr := owner.Accept()
		if aerr != nil {
			accepted <- false
			return
		}
		defer c.Close()
		_, ok := auth.PeerCred(c)
		accepted <- ok
	}()
	client, err := auth.DialLocal(context.Background(), pipe, 5*time.Second)
	if err != nil {
		t.Fatalf("the owner's pipe was disturbed: %v", err)
	}
	defer client.Close()
	if !<-accepted {
		t.Fatal("the owner's pipe no longer yields a verified peer")
	}
}

// No lockfile until every listener that can fail has been opened: a listener
// failure must never leave a lockfile advertising a port nothing serves. The
// unix fixture uses a missing parent directory; a pipe has no parent, so the
// equivalent unopenable path is one outside the pipe namespace.
func TestBootServer_PipeFailureWritesNoLockfile(t *testing.T) {
	notAPipe := filepath.Join(t.TempDir(), "knomit.sock")
	lockPath := filepath.Join(t.TempDir(), "server.json")
	srv, _, err := bootServer(context.Background(), peerEcho, lockPath, "v", "", notAPipe)
	if err == nil {
		srv.shutdown()
		t.Fatal("bootServer must fail when the local listener cannot be opened")
	}
	if errors.Is(err, auth.ErrSocketInUse) {
		t.Fatalf("fixture reached the in-use branch, not a real failure: %v", err)
	}
	if _, serr := os.Stat(lockPath); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("lockfile written despite the listener failure: %v", serr)
	}
}
