package knomitapi

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

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
func serveLocalAt(t *testing.T, path, body string) {
	t.Helper()
	l := listenLocal(t, path)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close(); l.Close() })
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
