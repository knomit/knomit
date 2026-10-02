package knomitapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"knomit/internal/serveraddr"
)

// countingTCP is a TCP server that answers body and counts what it served, so
// a test can assert it was NOT reached.
func countingTCP(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s, &n
}

// One test per precedence level: argument > KNOMIT_SERVER > lockfile >
// default. Each level is set up WITH every lower level present, so a resolver
// that consulted them in the wrong order picks the wrong one.
func TestResolveServer_Precedence(t *testing.T) {
	const arg, env = "http://arg.test:1", "http://env.test:2"
	cases := []struct {
		name              string
		arg, env          string
		lockfile          bool
		wantBase, wantSrc string
		wantNamed         bool
	}{
		{"argument beats everything", arg, env, true, arg, SourceArgument, true},
		{"KNOMIT_SERVER beats the lockfile", "", env, true, env, SourceEnv, true},
		{"lockfile beats the default", "", "", true, "http://127.0.0.1:4242", SourceLockfile, false},
		{"default last", "", "", false, DefaultServer, SourceDefault, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolateHome(t)
			lock := isolateLockfile(t)
			if c.lockfile {
				writeLockfile(t, lock, "http://127.0.0.1:4242")
			}
			t.Setenv(serveraddr.EnvVar, c.env)
			s, err := ResolveServer(c.arg)
			if err != nil {
				t.Fatal(err)
			}
			if s.Base != c.wantBase || s.Source != c.wantSrc || s.Named != c.wantNamed || s.IsLocal() {
				t.Fatalf("got base %q source %q named %v local %q; want %q %q %v",
					s.Base, s.Source, s.Named, s.Local, c.wantBase, c.wantSrc, c.wantNamed)
			}
		})
	}
}

// A malformed KNOMIT_SERVER or argument is an error naming WHICH, never a
// quiet fall to the next level.
func TestResolveServer_MalformedNamesItsSource(t *testing.T) {
	isolateHome(t)
	writeLockfile(t, isolateLockfile(t), "http://127.0.0.1:4242")
	t.Setenv(serveraddr.EnvVar, "not a url")
	if _, err := ResolveServer(""); err == nil || !strings.Contains(err.Error(), serveraddr.EnvVar) {
		t.Fatalf("malformed KNOMIT_SERVER: err = %v, want one naming %s", err, serveraddr.EnvVar)
	}
	t.Setenv(serveraddr.EnvVar, "")
	if _, err := ResolveServer("ftp://x"); err == nil || !strings.Contains(err.Error(), "server argument") {
		t.Fatalf("malformed argument: err = %v, want one naming the server argument", err)
	}
}

// A named http address is used exactly as given: a LIVE local listener at the
// default path is not taken instead.
func TestNewServerClient_NamedHTTPIsNeverReroutedToTheSocket(t *testing.T) {
	isolateHome(t)
	serveAtResolvedHome(t, "via-socket")
	tcp, _ := countingTCP(t, "via-tcp")
	t.Setenv(serveraddr.EnvVar, tcp.URL)
	s, err := ResolveServer("")
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, NewServerClient(s, 5*time.Second), s.Base+"/x"); got != "via-tcp" {
		t.Fatalf("a named http address went elsewhere; body=%q", got)
	}
}

// The default path is unchanged: with nothing named and the lockfile naming a
// live TCP server, a live local listener still wins.
func TestNewServerClient_DefaultStillPrefersTheSocket(t *testing.T) {
	isolateHome(t)
	serveAtResolvedHome(t, "via-socket")
	tcp, hits := countingTCP(t, "via-tcp")
	writeLockfile(t, isolateLockfile(t), tcp.URL)
	s, err := ResolveServer("")
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != SourceLockfile {
		t.Fatalf("source = %q, want the lockfile", s.Source)
	}
	if got := get(t, NewServerClient(s, 5*time.Second), s.Base+"/x"); got != "via-socket" {
		t.Fatalf("the default path must prefer the %s; body=%q", localListenerName, got)
	}
	if hits.Load() != 0 {
		t.Fatal("the lockfile's TCP server was reached although the local listener was live")
	}
}

// A named local listener is the ONLY place the client goes; the fixture
// asserts each request carried a verified peer.
func TestNewServerClient_NamedLocalListenerIsUsed(t *testing.T) {
	isolateHome(t)
	path := serveLocal(t, "via-named-socket")
	tcp, hits := countingTCP(t, "via-tcp")
	writeLockfile(t, isolateLockfile(t), tcp.URL)
	t.Setenv(serveraddr.EnvVar, serveraddr.ForLocal(path))
	s, err := ResolveServer("")
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsLocal() || s.Local != path || s.Base != serveraddr.LocalBase {
		t.Fatalf("resolved %+v, want the local listener %q", s, path)
	}
	if got := get(t, NewServerClient(s, 5*time.Second), s.Base+"/x"); got != "via-named-socket" {
		t.Fatalf("body=%q", got)
	}
	if hits.Load() != 0 {
		t.Fatal("the lockfile's server was reached")
	}
}

// A named local listener that is missing fails the request naming the
// address — no TCP, no lockfile.
func TestNewServerClient_NamedMissingLocalListenerFails(t *testing.T) {
	isolateHome(t)
	missing := absentLocalListenerPath(t)
	tcp, hits := countingTCP(t, "via-tcp")
	writeLockfile(t, isolateLockfile(t), tcp.URL)
	t.Setenv(serveraddr.EnvVar, serveraddr.ForLocal(missing))
	s, err := ResolveServer("")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := NewServerClient(s, 5*time.Second).Get(s.Base + "/x")
	if err == nil {
		resp.Body.Close()
		t.Fatal("a request to a missing named listener succeeded")
	}
	if !strings.Contains(err.Error(), s.Raw) {
		t.Errorf("the error %q does not name the address that was asked for", err)
	}
	if hits.Load() != 0 {
		t.Fatal("a missing named listener fell back to the lockfile's TCP server")
	}
}

// The hooks follow KNOMIT_SERVER too, per dial: a named local listener, then
// — after first use — a named TCP server.
func TestClient_HooksFollowKnomitServer(t *testing.T) {
	isolateHome(t)
	path := serveLocal(t, "via-named-socket")
	tcp, _ := countingTCP(t, "via-tcp")

	t.Setenv(serveraddr.EnvVar, serveraddr.ForLocal(path))
	if got := get(t, Client(), BaseURL()+"/x"); got != "via-named-socket" {
		t.Fatalf("hooks with KNOMIT_SERVER=%s: body=%q", serveraddr.ForLocal(path), got)
	}
	t.Setenv(serveraddr.EnvVar, tcp.URL)
	if got := get(t, Client(), BaseURL()+"/x"); got != "via-tcp" {
		t.Fatalf("hooks with KNOMIT_SERVER=%s: body=%q", tcp.URL, got)
	}
}

// A malformed KNOMIT_SERVER sends the hooks nowhere — not to the lockfile's
// server — and the error names the variable.
func TestClient_HooksMalformedKnomitServerSendsNothing(t *testing.T) {
	isolateHome(t)
	tcp, hits := countingTCP(t, "via-tcp")
	writeLockfile(t, isolateLockfile(t), tcp.URL)
	t.Setenv(serveraddr.EnvVar, "localhost:19278")
	resp, err := Client().Get(BaseURL() + "/x")
	if err == nil {
		resp.Body.Close()
		t.Fatal("a hook request went out with a malformed KNOMIT_SERVER")
	}
	if !strings.Contains(err.Error(), serveraddr.EnvVar) {
		t.Errorf("the error %q does not name %s", err, serveraddr.EnvVar)
	}
	if hits.Load() != 0 {
		t.Fatal("a malformed KNOMIT_SERVER fell back to the lockfile's server")
	}
}

// The hooks read the lockfile when nothing is named — one rule for every call.
func TestClient_HooksUseTheLockfileWhenNothingIsNamed(t *testing.T) {
	isolateHome(t)
	tcp, _ := countingTCP(t, "via-lockfile")
	writeLockfile(t, isolateLockfile(t), tcp.URL)
	if got := get(t, Client(), BaseURL()+"/x"); got != "via-lockfile" {
		t.Fatalf("body=%q", got)
	}
}
