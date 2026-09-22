package knomitapi

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestNewHTTPClient_DialsSocketWhenPresentAndNoExplicitURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix sockets")
	}
	// A short path: macOS caps sun_path at 104 bytes and t.TempDir() can exceed it.
	dir, err := os.MkdirTemp("/tmp", "bd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "knomit.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-socket")
	})}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	c := NewHTTPClient(sock, false, 2*time.Second)
	// Port 1: nothing listens there, so ONLY the socket can answer this.
	resp, err := c.Get("http://localhost:1/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "via-socket" {
		t.Fatalf("body=%q", b)
	}
}

func TestNewHTTPClient_ExplicitURLIgnoresSocket(t *testing.T) {
	c := NewHTTPClient("/nonexistent/knomit.sock", true, 500*time.Millisecond)
	if _, err := c.Get("http://127.0.0.1:1/"); err == nil {
		t.Fatal("an explicit URL must use TCP and fail on a closed port")
	}
}

func TestNewHTTPClient_MissingSocketFallsBackToTCP(t *testing.T) {
	c := NewHTTPClient("/nonexistent/knomit.sock", false, 500*time.Millisecond)
	if _, err := c.Get("http://127.0.0.1:1/"); err == nil {
		t.Fatal("a missing socket must fall back to TCP, which fails on a closed port")
	}
}

// An EXPLICIT url wins even when the socket is right there: the user is
// pointing at a particular server, possibly a remote one.
func TestNewHTTPClient_ExplicitURLWinsOverALiveSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix sockets")
	}
	dir, err := os.MkdirTemp("/tmp", "bd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "knomit.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-socket")
	})}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	c := NewHTTPClient(sock, true, 500*time.Millisecond)
	if _, err := c.Get("http://127.0.0.1:1/"); err == nil {
		t.Fatal("explicit must not be redirected onto the socket")
	}
}

// A path that exists but is NOT a socket (a stale regular file) must not be
// dialled: every request would fail instead of falling back to TCP.
func TestNewHTTPClient_NonSocketPathFallsBackToTCP(t *testing.T) {
	dir := t.TempDir()
	notASocket := filepath.Join(dir, "knomit.sock")
	if err := os.WriteFile(notASocket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewHTTPClient(notASocket, false, 500*time.Millisecond)
	if _, err := c.Get("http://127.0.0.1:1/"); err == nil {
		t.Fatal("a regular file must not be dialled as a socket")
	}
	if c.Transport != nil {
		t.Fatal("a non-socket path must leave the default TCP transport in place")
	}
}
