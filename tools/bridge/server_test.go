package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"knomit/internal/config"
	"knomit/internal/serveraddr"
)

// isolateServerEnv empties every input ResolveServer reads — KNOMIT_SERVER and
// the desktop lockfile's directory — and points KNOMIT_HOME (the default
// local listener's home) at an empty temp dir, so no test here can reach a
// server that happens to run on the developer's machine. Returns the lockfile
// path a test may write.
func isolateServerEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(serveraddr.EnvVar, "")
	t.Setenv("KNOMIT_HOME", filepath.Join(dir, "home"))
	t.Setenv("KNOMIT_REPO", "")
	t.Setenv("KNOMIT_SOCKET", "")
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("LOCALAPPDATA", filepath.Join(dir, "local"))
	p, err := config.LockfilePath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, dir) {
		t.Fatalf("lockfile %q is not under the isolated dir %q", p, dir)
	}
	return p
}

// writeLockfile advertises srvURL's port the way the desktop does.
func writeLockfile(t *testing.T, path, srvURL string) {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"port":`+u.Port()+`,"pid":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeKnomit answers the three kinds of request a repo-mode bridge makes —
// discovery (GET /api/v1/repos/{repo}), the proxied MCP POST and the closing
// DELETE — and records every request it receives, so a test can assert both
// that the chosen server got them all and that another got NONE.
type fakeKnomit struct {
	name string
	mu   sync.Mutex
	seen []string
}

func (f *fakeKnomit) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/repos/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"agent_branch": "agent/" + f.name})
		case r.Method == http.MethodPost:
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "sess-"+f.name)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"`+f.name+`"}}}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
}

func (f *fakeKnomit) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func newFakeTCP(t *testing.T, name string) (*fakeKnomit, *httptest.Server) {
	t.Helper()
	f := &fakeKnomit{name: name}
	s := httptest.NewServer(f.handler())
	t.Cleanup(s.Close)
	return f, s
}

// runBridge drives the real startup path a repo-mode `kb` takes —
// resolveServer, connect (which discovers the agent branch), runProxy on one
// initialize line, terminateSession — and returns what the agent saw.
func runBridge(t *testing.T, args []string) string {
	t.Helper()
	s, err := resolveServer(args)
	if err != nil {
		t.Fatalf("resolveServer: %v", err)
	}
	conn, err := connect(s, modeRepo, "core", "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var out strings.Builder
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	hdr := clientHeaders(buildIdentity(conn.branch, time.Now()))
	sid, err := runProxy(in, &out, conn.client, conn.serverURL, hdr)
	if err != nil {
		t.Fatalf("runProxy: %v", err)
	}
	terminateSession(conn.client, conn.serverURL, sid, hdr)
	return out.String()
}

// wantAllThree asserts f received discovery, the proxied POST and the closing
// DELETE — every call a repo-mode bridge makes.
func wantAllThree(t *testing.T, f *fakeKnomit) {
	t.Helper()
	got := strings.Join(f.requests(), "\n")
	for _, want := range []string{
		"GET /api/v1/repos/core",
		"POST /api/v1/repos/core/branches/agent:" + f.name + "/mcp",
		"DELETE /api/v1/repos/core/branches/agent:" + f.name + "/mcp",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("server %s did not receive %q; it saw:\n%s", f.name, want, got)
		}
	}
}

// THE BUG (first-mission rehearsal, 2026-10-02): with several instances on one
// machine, `kb --repo X` read the server only from its argument or the
// desktop lockfile, so a session meant for one instance reached another. Here
// the lockfile names A and KNOMIT_SERVER names B: every request — discovery
// first — must reach B, and A must see NONE of them.
func TestBridge_KnomitServerBeatsTheLockfileForEveryCall(t *testing.T) {
	lock := isolateServerEnv(t)
	a, aSrv := newFakeTCP(t, "a")
	b, bSrv := newFakeTCP(t, "b")
	writeLockfile(t, lock, aSrv.URL)
	t.Setenv(serveraddr.EnvVar, bSrv.URL)

	out := runBridge(t, nil)

	if !strings.Contains(out, `"name":"b"`) {
		t.Fatalf("the agent did not get B's answer: %q", out)
	}
	wantAllThree(t, b)
	if got := a.requests(); len(got) != 0 {
		t.Fatalf("the lockfile's server received %d request(s) although KNOMIT_SERVER named another: %v", len(got), got)
	}
}

// The argument beats KNOMIT_SERVER, for every call.
func TestBridge_ArgumentBeatsKnomitServer(t *testing.T) {
	isolateServerEnv(t)
	a, aSrv := newFakeTCP(t, "a")
	b, bSrv := newFakeTCP(t, "b")
	t.Setenv(serveraddr.EnvVar, aSrv.URL)

	runBridge(t, []string{bSrv.URL})

	wantAllThree(t, b)
	if got := a.requests(); len(got) != 0 {
		t.Fatalf("KNOMIT_SERVER's server received %d request(s) although the argument named another: %v", len(got), got)
	}
}

// With nothing named, the lockfile's server gets every call (the level below
// KNOMIT_SERVER), so the two tests above are not passing because the lockfile
// is never read.
func TestBridge_LockfileWhenNothingIsNamed(t *testing.T) {
	lock := isolateServerEnv(t)
	a, aSrv := newFakeTCP(t, "a")
	writeLockfile(t, lock, aSrv.URL)

	runBridge(t, nil)

	wantAllThree(t, a)
}

// KNOMIT_HOME is the instance's folder, not its address: pointing it somewhere
// else does not move the bridge off KNOMIT_SERVER.
func TestBridge_KnomitHomeDoesNotChooseTheServer(t *testing.T) {
	lock := isolateServerEnv(t)
	a, aSrv := newFakeTCP(t, "a")
	b, bSrv := newFakeTCP(t, "b")
	writeLockfile(t, lock, aSrv.URL)
	t.Setenv("KNOMIT_HOME", t.TempDir())
	t.Setenv(serveraddr.EnvVar, bSrv.URL)

	runBridge(t, nil)

	wantAllThree(t, b)
	if got := a.requests(); len(got) != 0 {
		t.Fatalf("A received %v", got)
	}
}

// A malformed KNOMIT_SERVER stops the bridge with an error naming the
// variable; it never falls through to the lockfile.
func TestResolveServer_MalformedKnomitServerIsAnError(t *testing.T) {
	lock := isolateServerEnv(t)
	_, aSrv := newFakeTCP(t, "a")
	writeLockfile(t, lock, aSrv.URL)
	for _, bad := range []string{"localhost:19278", "ftp://h", "http://", "unix://relative.sock", "http://h:1/api"} {
		t.Setenv(serveraddr.EnvVar, bad)
		s, err := resolveServer(nil)
		if err == nil {
			t.Errorf("KNOMIT_SERVER=%q resolved to %+v, want an error", bad, s)
			continue
		}
		if !strings.Contains(err.Error(), serveraddr.EnvVar) {
			t.Errorf("KNOMIT_SERVER=%q: the error %q does not name the variable", bad, err)
		}
	}
}
