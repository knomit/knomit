package knomitapi

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// requireUnixSockets skips on Windows. The unix socket is the ONLY credential
// this phase has, and Windows has no equivalent yet -- knomit/knomit#245 tracks
// the named-pipe transport that will give it one. These tests are not
// platform-agnostic tests that happen to fail there; they exercise a mechanism
// that does not exist on that platform in this phase.
//
// They also need a SHORT socket path (os.MkdirTemp("/tmp", ...) rather than
// t.TempDir()), because macOS caps sun_path at 104 bytes and t.TempDir()
// overruns it -- and /tmp does not exist on Windows, which is how the missing
// guard here first showed up, as a CI failure rather than a skip.
func requireUnixSockets(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no unix domain sockets on windows in this phase; named pipes are knomit/knomit#245")
	}
}

// isolateHome points KNOMIT_HOME at an empty temp dir so nothing in this
// package's suite can reach a socket that happens to exist on the developer's
// machine. Without it a green run means "no socket at ~/.knomit right now",
// not "this commit is good".
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KNOMIT_HOME", dir)
	t.Setenv("KNOMIT_REPO", "")
	t.Setenv("KNOMIT_BASE_URL", "")
	return dir
}

// serveUnix starts an HTTP server on a unix socket that answers with body.
// The path is short on purpose: macOS caps sun_path at 104 bytes and
// t.TempDir() can exceed it.
func serveUnix(t *testing.T, body string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "knomit.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close(); l.Close() })
	return sock
}

// staleSocket creates a socket INODE with nothing accepting on it — what an
// ungraceful server exit leaves behind, because cmd/serve.go only removes the
// socket on a graceful one.
func staleSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ks")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "knomit.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Go unlinks a unix socket on Close by default, which is precisely what a
	// CRASHING server does not do. Turning that off reproduces the real
	// leftover: the inode survives, so os.Stat still reports a socket and a
	// dial gets ECONNREFUSED.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if st, serr := os.Stat(sock); serr != nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected a stale SOCKET to remain at %s (err=%v)", sock, serr)
	}
	return sock
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
	requireUnixSockets(t)
	isolateHome(t)
	sock := serveUnix(t, "via-socket")
	c := NewHTTPClient(sock, false, 2*time.Second)
	// Port 1: nothing listens there, so ONLY the socket can answer this.
	if got := get(t, c, "http://localhost:1/anything"); got != "via-socket" {
		t.Fatalf("body=%q", got)
	}
}

// REGRESSION, blocking finding 1 of the 2026-09-22 review: a socket file that
// exists but is not serving must cost the verified identity, not all
// connectivity. Deciding the transport once from os.Stat installed a
// socket-only transport and every request failed with "connection refused".
func TestNewHTTPClient_StaleSocketFallsBackToLiveTCP(t *testing.T) {
	requireUnixSockets(t)
	isolateHome(t)
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-tcp")
	}))
	defer tcp.Close()

	sock := staleSocket(t)

	// Capture the bridge log: falling back SILENTLY is the failure mode worth
	// guarding, because a dead socket beside a live server then looks exactly
	// like a healthy bridge.
	var logbuf bytes.Buffer
	restore := log.Logger
	log.Logger = zerolog.New(&logbuf)
	t.Cleanup(func() { log.Logger = restore })

	c := NewHTTPClient(sock, false, 2*time.Second)
	// Asserts WHICH server answered: a call that merely succeeded would pass
	// even if the socket had been used.
	if got := get(t, c, tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("a stale socket must fall back to the TCP target; body=%q", got)
	}
	out := logbuf.String()
	if !strings.Contains(out, "falling back to TCP") || !strings.Contains(out, sock) {
		t.Fatalf("the fallback must be logged with the socket path, got: %s", out)
	}

	// Once, not per dial. A second request must not repeat the warning.
	logbuf.Reset()
	c.CloseIdleConnections() // force a fresh dial
	_ = get(t, c, tcp.URL+"/again")
	if strings.Contains(logbuf.String(), "falling back to TCP") {
		t.Fatalf("the fallback warning must be logged ONCE, not per dial: %s", logbuf.String())
	}
}

// A path too long for sun_path fails with EINVAL, not ENOENT. That is a
// misconfiguration and not the ordinary "no server running" case, so it must
// still WARN — this pins that the quiet path is keyed on ENOENT specifically
// and not on "any failure to reach the socket".
func TestNewHTTPClient_OverlongSocketPathStillWarns(t *testing.T) {
	requireUnixSockets(t)
	if runtime.GOOS != "darwin" {
		t.Skip("the sun_path cap that produces EINVAL here is a darwin limit")
	}
	isolateHome(t)
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-tcp")
	}))
	defer tcp.Close()

	var logbuf bytes.Buffer
	restore := log.Logger
	log.Logger = zerolog.New(&logbuf).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = restore })

	overlong := filepath.Join(t.TempDir(), strings.Repeat("x", 120)+".sock")
	c := NewHTTPClient(overlong, false, 2*time.Second)
	if got := get(t, c, tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("it must still fall back; body=%q", got)
	}
	if !strings.Contains(logbuf.String(), "unreachable") {
		t.Fatalf("an unusable socket path is an anomaly and must warn, got: %s", logbuf.String())
	}
}

// Precedence is unchanged by the fallback: a socket that ANSWERS still wins
// over a live TCP server.
func TestNewHTTPClient_LiveSocketWinsOverLiveTCP(t *testing.T) {
	requireUnixSockets(t)
	isolateHome(t)
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-tcp")
	}))
	defer tcp.Close()

	c := NewHTTPClient(serveUnix(t, "via-socket"), false, 2*time.Second)
	if got := get(t, c, tcp.URL+"/anything"); got != "via-socket" {
		t.Fatalf("a live socket must win over a live TCP server; body=%q", got)
	}
}

func TestNewHTTPClient_ExplicitURLIgnoresSocket(t *testing.T) {
	requireUnixSockets(t)
	isolateHome(t)
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-tcp")
	}))
	defer tcp.Close()

	// The socket is LIVE and would win if this were not explicit.
	c := NewHTTPClient(serveUnix(t, "via-socket"), true, 2*time.Second)
	if got := get(t, c, tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("an explicit URL must not be rerouted onto the socket; body=%q", got)
	}
}

// A MISSING socket is the ordinary case — no server running, or one older
// than the socket. It must fall back silently: warning about it would dilute
// the signal the WARN exists for, which is the stale-socket anomaly.
func TestNewHTTPClient_MissingSocketFallsBackToTCPWithoutWarning(t *testing.T) {
	requireUnixSockets(t)
	isolateHome(t)
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-tcp")
	}))
	defer tcp.Close()

	var logbuf bytes.Buffer
	restore := log.Logger
	// Debug level, so a Debug line WOULD be captured if one were emitted at
	// WARN by mistake -- the assertion below is about the level, and a
	// logger that dropped Debug could not tell the two apart.
	log.Logger = zerolog.New(&logbuf).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = restore })

	// A SHORT path: macOS caps sun_path at 104 bytes, and a path over that
	// fails with EINVAL rather than ENOENT — which is a different thing and
	// SHOULD still warn, so using t.TempDir() here would test the wrong case.
	dir, err := os.MkdirTemp("/tmp", "ka")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	c := NewHTTPClient(filepath.Join(dir, "absent.sock"), false, 2*time.Second)
	if got := get(t, c, tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("a missing socket must fall back to TCP; body=%q", got)
	}
	out := logbuf.String()
	if strings.Contains(out, "unreachable") || strings.Contains(out, `"level":"warn"`) {
		t.Fatalf("an absent socket is ordinary and must not WARN, got: %s", out)
	}
	// Positive control: it did take the fallback path, and said so quietly.
	if !strings.Contains(out, `"level":"debug"`) || !strings.Contains(out, "no unix socket") {
		t.Fatalf("the absent-socket fallback should still be visible at Debug, got: %s", out)
	}
}

// A path that exists but is NOT a socket (a stale regular file) must also
// fall back rather than fail every request.
func TestNewHTTPClient_NonSocketPathFallsBackToTCP(t *testing.T) {
	requireUnixSockets(t)
	isolateHome(t)
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-tcp")
	}))
	defer tcp.Close()

	notASocket := filepath.Join(t.TempDir(), "knomit.sock")
	if err := os.WriteFile(notASocket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewHTTPClient(notASocket, false, 2*time.Second)
	if got := get(t, c, tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("a regular file must not be dialled as a socket; body=%q", got)
	}
}

// REGRESSION, blocking finding 2: the hooks client must read KNOMIT_BASE_URL
// at DIAL time. A package-level var froze the transport at package init,
// before any test body ran, so t.Setenv could not redirect it — and with a
// live socket those tests reached the real local server and could pass for
// the wrong reason.
func TestClient_HonoursBaseURLSetAfterInit(t *testing.T) {
	requireUnixSockets(t)
	isolateHome(t)
	// A LIVE socket at the resolved home: without per-dial resolution this is
	// exactly the machine state that hijacks the call.
	sock := serveUnix(t, "via-socket")
	t.Setenv("KNOMIT_HOME", filepath.Dir(sock))

	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-tcp")
	}))
	defer tcp.Close()
	t.Setenv("KNOMIT_BASE_URL", tcp.URL)

	if got := get(t, Client(), tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("KNOMIT_BASE_URL set after init must be honoured; body=%q", got)
	}
}

// The counterpart: with no explicit URL, the same lazily-built client DOES
// take the socket — so the test above is not passing merely because the
// socket was never reachable.
func TestClient_UsesTheSocketWhenNoBaseURLIsNamed(t *testing.T) {
	requireUnixSockets(t)
	isolateHome(t)
	sock := serveUnix(t, "via-socket")
	t.Setenv("KNOMIT_HOME", filepath.Dir(sock))

	if got := get(t, Client(), "http://127.0.0.1:1/anything"); got != "via-socket" {
		t.Fatalf("the hooks client must prefer the socket; body=%q", got)
	}
}

func TestTransportPreference_NeverClaimsUnixWithoutASocket(t *testing.T) {
	if got := TransportPreference("/tmp/x.sock", true); got != "tcp" {
		t.Fatalf("explicit = %q", got)
	}
	if got := TransportPreference("", false); got != "tcp" {
		t.Fatalf("no socket path = %q", got)
	}
	got := TransportPreference("/tmp/x.sock", false)
	if got != "unix-preferred" {
		t.Fatalf("socket path = %q", got)
	}
	// The word matters: the startup line must not read as a fact about a
	// connection that has not been made.
	if got == "unix" || !strings.Contains(got, "preferred") {
		t.Fatalf("the log value must say it is a preference, got %q", got)
	}
}
