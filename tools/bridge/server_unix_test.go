//go:build !windows

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/serveraddr"
)

func localArg(path string) string  { return "unix://" + path }
func localPath(path string) string { return path }

// serveFakeOnSocket serves f on a unix socket of this test's own, with the
// real server's ConnContext, and records whether each request carried a
// verified peer (the property the local listener exists for).
func serveFakeOnSocket(t *testing.T, f *fakeKnomit) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kbs") // short: sun_path is ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "knomit.sock")
	l, closeL, err := auth.ListenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeL)
	inner := f.handler()
	srv := &http.Server{
		ConnContext: auth.ConnContext,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, ok := auth.PeerFromContext(r.Context()); !ok || p.PID != os.Getpid() {
				t.Errorf("%s %s reached the socket without a verified peer", r.Method, r.URL.Path)
			}
			inner.ServeHTTP(w, r)
		}),
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	return path
}

// A named unix:// socket carries EVERY call — discovery included, which was
// plain TCP to the lockfile's port before — and the lockfile's server sees
// nothing.
func TestBridge_NamedSocketCarriesEveryCall(t *testing.T) {
	lock := isolateServerEnv(t)
	a, aSrv := newFakeTCP(t, "a")
	writeLockfile(t, lock, aSrv.URL)
	b := &fakeKnomit{name: "b"}
	t.Setenv(serveraddr.EnvVar, serveraddr.ForLocal(serveFakeOnSocket(t, b)))

	out := runBridge(t, nil)

	if !strings.Contains(out, `"name":"b"`) {
		t.Fatalf("the agent did not get the socket server's answer: %q", out)
	}
	wantAllThree(t, b)
	if got := a.requests(); len(got) != 0 {
		t.Fatalf("the lockfile's server received %v although KNOMIT_SERVER named a socket", got)
	}
}

// A named socket that is not there fails the bridge at discovery. It does NOT
// fall back to TCP or to the lockfile: that fallback is the original bug in a
// new shape — a session meant for one instance served by another.
func TestBridge_NamedMissingSocketFailsWithoutFallback(t *testing.T) {
	lock := isolateServerEnv(t)
	a, aSrv := newFakeTCP(t, "a")
	writeLockfile(t, lock, aSrv.URL)
	// Short, under /tmp: t.TempDir() on macOS exceeds sun_path, and a path
	// that long is refused at parse time (a different test's subject).
	dir, err := os.MkdirTemp("/tmp", "kbm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	missing := filepath.Join(dir, "gone.sock")
	t.Setenv(serveraddr.EnvVar, serveraddr.ForLocal(missing))

	s, err := resolveServer(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = connect(s, modeRepo, "core", "")
	if err == nil {
		t.Fatal("connect succeeded against a socket that does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the error %q does not name the socket that was asked for", err)
	}
	if got := a.requests(); len(got) != 0 {
		t.Fatalf("a missing named socket fell back to the lockfile's server: %v", got)
	}
}
