//go:build desktop

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"knomit/internal/auth"
)

// shortSocketPath returns a socket path under a fresh /tmp directory: macOS
// caps sun_path at 104 bytes and t.TempDir() can exceed it.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no unix domain sockets on windows in this phase; named pipes are knomit/knomit#245")
	}
	dir, err := os.MkdirTemp("/tmp", "bs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "knomit.sock")
}

// peerEcho answers with what ConnContext attached to the request, so a test
// can tell a socket that merely serves from one that carries peer credentials.
var peerEcho = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	uid, _, ok := auth.PeerFromContext(r.Context())
	fmt.Fprintf(w, "peer uid=%d ok=%v", uid, ok)
})

func unixClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
	}
}

func getBody(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// Issue #248: the desktop's own http.Server must open the socket AND attach
// the kernel's peer credential, or bridges stay anonymous on the normal install.
func TestBootServer_OpensLocalSocketWithPeerCreds(t *testing.T) {
	sock := shortSocketPath(t)
	lockPath := filepath.Join(t.TempDir(), "server.json")
	srv, port, err := bootServer(context.Background(), peerEcho, lockPath, "v", "", sock)
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
	want := fmt.Sprintf("peer uid=%d ok=true", os.Getuid())
	if got := getBody(t, unixClient(sock), "http://localhost:1/x"); got != want {
		t.Errorf("over the socket: got %q, want %q", got, want)
	}
	// Positive control for the TCP side: same handler, no peer credential.
	if got := getBody(t, http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/x", port)); got != "peer uid=0 ok=false" {
		t.Errorf("over TCP: got %q, want no peer", got)
	}
	srv.shutdown()
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket must be gone after shutdown: %v", err)
	}
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
	srv, port, err := bootServer(context.Background(), peerEcho, lockPath, "v", "", sock)
	if err != nil {
		t.Fatalf("socket in use must not fail the boot: %v", err)
	}
	if got := getBody(t, http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/x", port)); got != "peer uid=0 ok=false" {
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
	srv, _, err := bootServer(context.Background(), peerEcho, lockPath, "v", "", sock)
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
