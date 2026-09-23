//go:build desktop && !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/auth"
)

// shortSocketPath returns a socket path under a fresh /tmp directory: macOS
// caps sun_path at 104 bytes and t.TempDir() can exceed it.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "knomit.sock")
}

// Issue #248: the desktop's own http.Server must open the socket AND attach
// the kernel's peer credential, or bridges stay anonymous on the normal install.
func TestBootServer_OpensLocalSocketWithPeerCreds(t *testing.T) {
	sock := shortSocketPath(t)
	lockPath := filepath.Join(t.TempDir(), "server.json")
	srv, port, err := bootServer(context.Background(), peerEcho, lockPath, "v", "", localListener{Path: sock})
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(sock)
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		srv.shutdown()
		t.Fatalf("no socket at %s after bootServer: %v %v", sock, st, err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %o, want 0600", st.Mode().Perm())
	}
	// Port 1: nothing listens there, so ONLY the socket can answer this.
	want := wantOwnPeer(t)
	if got := getBody(t, localClient(sock), "http://localhost:1/x"); got != want {
		t.Errorf("over the socket: got %q, want %q", got, want)
	}
	// Positive control for the TCP side: same handler, no peer credential.
	if got := getBody(t, http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/x", port)); got != wantNoPeer {
		t.Errorf("over TCP: got %q, want no peer", got)
	}
	srv.shutdown()
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket must be gone after shutdown: %v", err)
	}
	// http.Server.Shutdown alone already unlinks the socket file, so the check
	// above cannot see a missing closeSocket. What only closeSocket does is
	// RELEASE THE LOCK — and a relaunch (NativeService.RestartApp) that finds
	// it still held serves TCP only, anonymous, until the next restart. Take
	// the path again the way that successor would.
	ln, release, err := auth.ListenLocal(sock)
	if err != nil || ln == nil {
		t.Fatalf("successor could not take the socket after shutdown: %v", err)
	}
	release()
}

// A live instance (normally `knomit serve`) already owns the socket: the
// desktop must still boot on TCP and leave that socket exactly where it is.
func TestBootServer_SocketInUseServesTCPOnly(t *testing.T) {
	sock := shortSocketPath(t)
	owner, release, err := auth.ListenLocal(sock)
	if err != nil || owner == nil {
		t.Fatalf("owner ListenLocal: %v %v", owner, err)
	}
	defer release()
	before, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(t.TempDir(), "server.json")
	srv, port, err := bootServer(context.Background(), peerEcho, lockPath, "v", "", localListener{Path: sock})
	if err != nil {
		t.Fatalf("socket in use must not fail the boot: %v", err)
	}
	if got := getBody(t, http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/x", port)); got != wantNoPeer {
		t.Errorf("TCP must still serve: got %q", got)
	}
	srv.shutdown() // its socket cleanup is the noop: must not touch the owner's file
	after, err := os.Stat(sock)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the owner's socket was removed or replaced: %v", err)
	}
}

// No lockfile until every listener that can fail has been opened: a socket
// failure must never leave a lockfile advertising a port nothing serves.
func TestBootServer_SocketFailureWritesNoLockfile(t *testing.T) {
	sock := filepath.Join(shortSocketPath(t)+".missing-dir", "knomit.sock") // parent does not exist
	lockPath := filepath.Join(t.TempDir(), "server.json")
	srv, _, err := bootServer(context.Background(), peerEcho, lockPath, "v", "", localListener{Path: sock})
	if err == nil {
		srv.shutdown()
		t.Fatal("bootServer must fail when the socket cannot be opened")
	}
	if errors.Is(err, auth.ErrSocketInUse) {
		t.Fatalf("fixture reached the in-use branch, not a real failure: %v", err)
	}
	if _, serr := os.Stat(lockPath); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("lockfile written despite socket failure: %v", serr)
	}
}

// knomit#253: a local listener path too long for sun_path must not fail the
// desktop's boot. It serves TCP only, and the lockfile is written because
// TCP IS serving. With [auth].require it refuses, naming the cause. The
// overlong path is explicit; a long data root alone gets config's short
// fallback and never reaches this branch.
func TestBootServer_PathTooLongServesTCPOnly(t *testing.T) {
	long := "/tmp/" + strings.Repeat("d", 120) + "/knomit.sock"
	lockPath := filepath.Join(t.TempDir(), "server.json")
	srv, port, err := bootServer(context.Background(), peerEcho, lockPath, "v", "", localListener{Path: long})
	if err != nil {
		t.Fatalf("an overlong socket path must not fail the boot: %v", err)
	}
	defer srv.shutdown()
	if got := getBody(t, http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/x", port)); got != wantNoPeer {
		t.Errorf("TCP must still serve: got %q", got)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("the lockfile must advertise the TCP port that IS serving: %v", err)
	}

	lockPath2 := filepath.Join(t.TempDir(), "server.json")
	srv2, _, err := bootServer(context.Background(), peerEcho, lockPath2, "v", "", localListener{Path: long, Require: true})
	if err == nil {
		srv2.shutdown()
		t.Fatal("require=true with no bindable local listener must refuse the boot")
	}
	if !errors.Is(err, auth.ErrPathTooLong) {
		t.Fatalf("the refusal does not name ErrPathTooLong: %v", err)
	}
	if _, serr := os.Stat(lockPath2); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("lockfile written despite the refused boot: %v", serr)
	}
}
