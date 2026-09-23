package knomitapi

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// These tests used to skip on Windows (53a0c7b9, naming knomit/knomit#245):
// the unix socket was the only credential phase 1 had, and Windows had no
// equivalent. It has one now — a named pipe — so the suite is written against
// the per-platform fixtures in dial_fixtures_{unix,windows}_test.go and RUNS
// on both. Nothing here knows which transport it is on.
//
// Every fixture that listens or dials asserts WHICH outcome it produced; the
// phase 1 review found two socket fixtures that passed while proving nothing.

// isolateHome points KNOMIT_HOME at an empty temp dir so nothing in this
// package's suite can reach a listener that happens to exist on the
// developer's machine. Without it a green run means "no server at ~/.knomit
// right now", not "this commit is good".
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KNOMIT_HOME", dir)
	t.Setenv("KNOMIT_REPO", "")
	t.Setenv("KNOMIT_BASE_URL", "")
	return dir
}

// serveLocalAt starts an HTTP server on the local listener at path and
// answers every request with body.
//
// It installs the SAME ConnContext hook the real server does, and holds every
// request to the standard the transport exists for: a request that arrives
// here must carry a verified peer whose pid is this test process's.
//
// Without that, this suite would prove only TRANSPORT SELECTION. It did: under
// a forced failure that downgraded auth.DialLocal to the anonymous
// impersonation level, internal/auth and internal/config went red and this
// package stayed green -- the bridge still reached the listener, and was
// simply nobody when it got there, which is the exact silent failure
// knomit#245 is about.
func serveLocalAt(t *testing.T, path, body string) {
	t.Helper()
	l := listenLocal(t, path)
	var mu sync.Mutex
	var served, identified int
	srv := &http.Server{
		ConnContext: auth.ConnContext,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			served++
			if peer, ok := auth.PeerFromContext(r.Context()); ok && peer.ID != "" && peer.PID == os.Getpid() {
				identified++
			}
			mu.Unlock()
			_, _ = io.WriteString(w, body)
		}),
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() {
		srv.Close()
		l.Close()
		mu.Lock()
		defer mu.Unlock()
		// served == 0 is not a failure: several tests here exist precisely to
		// show the bridge did NOT come this way.
		if served > 0 && identified != served {
			t.Errorf("the bridge reached this listener %d time(s) but only %d carried a verified peer with our pid: "+
				"the transport worked and the IDENTITY did not, which is the only reason to prefer it", served, identified)
		}
	})
}

// serveLocal starts a server on a listener path of this test's own.
func serveLocal(t *testing.T, body string) string {
	t.Helper()
	path := localListenerPath(t)
	serveLocalAt(t, path, body)
	return path
}

// serveAtResolvedHome points KNOMIT_HOME at a data root this platform can
// open a listener under, and serves on the listener config.SocketPath()
// resolves for it. That is what the hooks-client tests need: they exercise
// the path the BRIDGE would find on its own, not one handed to them.
func serveAtResolvedHome(t *testing.T, body string) string {
	t.Helper()
	home := homeForLocalListener(t)
	t.Setenv("KNOMIT_HOME", home)
	t.Setenv("KNOMIT_REPO", "")
	t.Setenv("KNOMIT_BASE_URL", "")
	path, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	// Positive control: the bridge's own accessor has to agree, or these
	// tests would serve one place and the client would look in another.
	if got := SocketPath(); got != path {
		t.Fatalf("the bridge resolves %q but config resolves %q", got, path)
	}
	serveLocalAt(t, path, body)
	return path
}

func tcpServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-tcp")
	}))
	t.Cleanup(s.Close)
	return s
}

// captureLog redirects the bridge's logger at DEBUG level, so a line emitted
// at Warn by mistake and one emitted at Debug are both captured and can be
// told apart. A logger that dropped Debug could not distinguish them.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	restore := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = restore })
	return &buf
}

func get(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestNewHTTPClient_DialsSocketWhenPresentAndNoExplicitURL(t *testing.T) {
	isolateHome(t)
	sock := serveLocal(t, "via-socket")
	c := NewHTTPClient(sock, false, 5*time.Second)
	// Port 1: nothing listens there, so ONLY the local listener can answer.
	if got := get(t, c, "http://localhost:1/anything"); got != "via-socket" {
		t.Fatalf("the %s must answer; body=%q", localListenerName, got)
	}
}

// REGRESSION, blocking finding 1 of the 2026-09-22 review: a listener that
// exists but cannot be used must cost the verified identity, not all
// connectivity. Deciding the transport once from os.Stat installed a
// socket-only transport and every request failed with "connection refused".
func TestNewHTTPClient_StaleSocketFallsBackToLiveTCP(t *testing.T) {
	isolateHome(t)
	tcp := tcpServer(t)
	sock := unreachableLocalListener(t)

	// Falling back SILENTLY is the failure mode worth guarding, because a
	// dead listener beside a live server then looks exactly like a healthy
	// bridge.
	logbuf := captureLog(t)

	c := NewHTTPClient(sock, false, 5*time.Second)
	// Asserts WHICH server answered: a call that merely succeeded would pass
	// even if the local listener had been used.
	if got := get(t, c, tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("an unreachable %s must fall back to the TCP target; body=%q", localListenerName, got)
	}
	out := logbuf.String()
	if !strings.Contains(out, "falling back to TCP") || !strings.Contains(out, `"level":"warn"`) {
		t.Fatalf("the fallback must be logged at WARN, got: %s", out)
	}
	if !strings.Contains(out, jsonEscaped(sock)) {
		t.Fatalf("the warning must name the listener path %q, got: %s", sock, out)
	}

	// Once, not per dial. A second request must not repeat the warning.
	logbuf.Reset()
	c.CloseIdleConnections() // force a fresh dial
	_ = get(t, c, tcp.URL+"/again")
	if strings.Contains(logbuf.String(), "falling back to TCP") {
		t.Fatalf("the fallback warning must be logged ONCE, not per dial: %s", logbuf.String())
	}
}

// Precedence is unchanged by the fallback: a listener that ANSWERS still wins
// over a live TCP server.
func TestNewHTTPClient_LiveSocketWinsOverLiveTCP(t *testing.T) {
	isolateHome(t)
	tcp := tcpServer(t)
	c := NewHTTPClient(serveLocal(t, "via-socket"), false, 5*time.Second)
	if got := get(t, c, tcp.URL+"/anything"); got != "via-socket" {
		t.Fatalf("a live %s must win over a live TCP server; body=%q", localListenerName, got)
	}
}

func TestNewHTTPClient_ExplicitURLIgnoresSocket(t *testing.T) {
	isolateHome(t)
	tcp := tcpServer(t)
	// The local listener is LIVE and would win if this were not explicit.
	c := NewHTTPClient(serveLocal(t, "via-socket"), true, 5*time.Second)
	if got := get(t, c, tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("an explicit URL must not be rerouted onto the %s; body=%q", localListenerName, got)
	}
}

// A MISSING listener is the ordinary case — no server running, or one older
// than the socket. It must fall back silently: warning about it would dilute
// the signal the WARN exists for, which is the unreachable-listener anomaly.
func TestNewHTTPClient_MissingSocketFallsBackToTCPWithoutWarning(t *testing.T) {
	isolateHome(t)
	tcp := tcpServer(t)
	logbuf := captureLog(t)

	c := NewHTTPClient(absentLocalListenerPath(t), false, 5*time.Second)
	if got := get(t, c, tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("a missing %s must fall back to TCP; body=%q", localListenerName, got)
	}
	out := logbuf.String()
	if strings.Contains(out, "unreachable") || strings.Contains(out, `"level":"warn"`) {
		t.Fatalf("an absent %s is ordinary and must not WARN, got: %s", localListenerName, out)
	}
	// Positive control: it did take the fallback path, and said so quietly.
	if !strings.Contains(out, `"level":"debug"`) || !strings.Contains(out, "no local listener") {
		t.Fatalf("the absent-listener fallback should still be visible at Debug, got: %s", out)
	}
}

// A path that exists but is NOT a local listener must also fall back rather
// than fail every request. On unix that is a stale regular file where a
// socket should be; on Windows it is any path outside the pipe namespace,
// which must be refused rather than opened as a file.
func TestNewHTTPClient_NonSocketPathFallsBackToTCP(t *testing.T) {
	isolateHome(t)
	tcp := tcpServer(t)
	c := NewHTTPClient(notAListenerPath(t), false, 5*time.Second)
	if got := get(t, c, tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("a non-listener path must not be dialled as one; body=%q", got)
	}
}

// REGRESSION, blocking finding 2: the hooks client must read KNOMIT_BASE_URL
// at DIAL time. A package-level var froze the transport at package init,
// before any test body ran, so t.Setenv could not redirect it — and with a
// live listener those tests reached the real local server and could pass for
// the wrong reason.
func TestClient_HonoursBaseURLSetAfterInit(t *testing.T) {
	isolateHome(t)
	// A LIVE listener at the resolved home: without per-dial resolution this
	// is exactly the machine state that hijacks the call.
	serveAtResolvedHome(t, "via-socket")

	tcp := tcpServer(t)
	t.Setenv("KNOMIT_BASE_URL", tcp.URL)

	if got := get(t, Client(), tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("KNOMIT_BASE_URL set after init must be honoured; body=%q", got)
	}
}

// The counterpart: with no explicit URL, the same lazily-built client DOES
// take the local listener — so the test above is not passing merely because
// the listener was never reachable.
func TestClient_UsesTheSocketWhenNoBaseURLIsNamed(t *testing.T) {
	isolateHome(t)
	serveAtResolvedHome(t, "via-socket")

	if got := get(t, Client(), "http://127.0.0.1:1/anything"); got != "via-socket" {
		t.Fatalf("the hooks client must prefer the %s; body=%q", localListenerName, got)
	}
}

func TestTransportPreference_NeverClaimsUnixWithoutASocket(t *testing.T) {
	if got := TransportPreference(localListenerPath(t), true); got != "tcp" {
		t.Fatalf("explicit = %q", got)
	}
	if got := TransportPreference("", false); got != "tcp" {
		t.Fatalf("no listener path = %q", got)
	}
	got := TransportPreference(localListenerPath(t), false)
	// The word matters: the startup line must not read as a fact about a
	// connection that has not been made.
	if !strings.HasSuffix(got, "-preferred") {
		t.Fatalf("the log value must say it is a preference, got %q", got)
	}
	// And it must name the MECHANISM, which is what tells a reader which
	// credential a session on it will carry.
	if got != "socket-preferred" && got != "pipe-preferred" {
		t.Fatalf("listener path = %q, want socket-preferred or pipe-preferred", got)
	}
}

// jsonEscaped is how a path appears inside a zerolog JSON line: Windows
// separators are escaped there, so a raw strings.Contains on the path would
// miss it and the "names the path" assertion would silently never fire.
func jsonEscaped(s string) string {
	return strings.ReplaceAll(s, `\`, `\\`)
}

// REGRESSION. The real bridge builds its client with Timeout 0 — no limit,
// because it holds SSE long-polls open and a deadline would cut them
// (tools/bridge/main.go). Zero must not become a zero-length budget for the
// local dial: a context deadline of "now" fails every dial instantly, so the
// bridge would never once use the pipe while looking perfectly healthy on TCP.
//
// This is not hypothetical. It is the state this branch was in until the
// budget below was added, and NOTHING else in the suite caught it, because
// every other test passes a real timeout.
func TestNewHTTPClient_UnlimitedTimeoutStillPrefersTheLocalListener(t *testing.T) {
	isolateHome(t)
	sock := serveLocal(t, "via-socket")
	c := NewHTTPClient(sock, false, 0) // exactly what tools/bridge/main.go does
	if got := get(t, c, "http://localhost:1/anything"); got != "via-socket" {
		t.Fatalf("with no overall timeout the %s must still answer; body=%q", localListenerName, got)
	}
}

// The budget is a SLICE of the request's time, not all of it: a local dial
// that spends the whole budget leaves the TCP fallback none, which is not a
// fallback. On Windows this is what stops a BUSY pipe — which winio retries
// every 10ms until the context expires rather than failing fast — from
// consuming the entire request.
func TestLocalDialBudget_LeavesRoomForTheFallback(t *testing.T) {
	if got := localDialBudget(0); got != localDialCap {
		t.Fatalf("no limit must become a bounded budget, got %v", got)
	}
	if got := localDialBudget(time.Hour); got != localDialCap {
		t.Fatalf("a huge timeout must still be capped, got %v", got)
	}
	if got := localDialBudget(4 * time.Second); got != time.Second {
		t.Fatalf("budget for 4s = %v, want 1s (a quarter, leaving 3s for TCP)", got)
	}
	// Never zero and never longer than the request itself: both would be
	// worse than not trying the local listener at all.
	for _, d := range []time.Duration{time.Nanosecond, time.Millisecond, time.Second, time.Minute} {
		got := localDialBudget(d)
		if got <= 0 || got > d {
			t.Fatalf("budget for %v = %v, want a positive slice no longer than it", d, got)
		}
	}
}
