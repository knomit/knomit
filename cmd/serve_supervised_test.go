//go:build !windows

package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/app"
	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/pki/pkitest"
	"knomit/internal/platform/crashdump"
	"knomit/internal/serveraddr"
	"knomit/test/testenv"
)

// `knomit serve --supervised` as the desktop will run it: stdin is a pipe the
// parent holds, the exit codes are documented, and a held TCP or TLS address
// is a warning. These run serve's whole RunE (the embedder swapped through
// serveAppOptions), because the contract is the returned error and the
// process' lifecycle, not a helper beside it.

// serveHarness is one RunE in a goroutine, on its own data root.
type serveHarness struct {
	t      *testing.T
	home   string
	stdin  *io.PipeWriter
	done   chan struct{} // closed when RunE has returned; err is then set
	err    error
	cancel context.CancelFunc
	lock   string
}

func isolateServe(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	t.Setenv("KNOMIT_HOST", "127.0.0.1")
	t.Setenv("KNOMIT_PORT", freePort(t))
	prevGlobal := crashdump.Global
	if crashdump.Global == nil {
		crashdump.Global = crashdump.NewRingWriter(200)
	}
	prevOpts, prevLog, prevLevel := serveAppOptions, log.Logger, zerolog.GlobalLevel()
	serveAppOptions = func(o app.Options) app.Options {
		o.Embedder = &testenv.DeterministicEmbedder{}
		return o
	}
	t.Cleanup(func() {
		serveAppOptions, log.Logger, crashdump.Global = prevOpts, prevLog, prevGlobal
		zerolog.SetGlobalLevel(prevLevel)
	})
	return home
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func startServe(t *testing.T, home string, args ...string) *serveHarness {
	t.Helper()
	c := serveCmd()
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	c.SetIn(pr)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	c.SetContext(ctx)
	h := &serveHarness{t: t, home: home, stdin: pw, done: make(chan struct{}), cancel: cancel,
		lock: config.LockfilePathFor(home)}
	go func() { h.err = c.RunE(c, nil); close(h.done) }()
	t.Cleanup(func() {
		cancel()
		pw.Close()
		select {
		case <-h.done:
		case <-time.After(20 * time.Second):
		}
	})
	return h
}

// waitServing blocks until server.json exists (written last, after every
// listener) and returns what it says.
func (h *serveHarness) waitServing() serveraddr.LockInfo {
	h.t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-h.done:
			h.t.Fatalf("serve returned before it was serving: %v", h.err)
		default:
		}
		if info, err := serveraddr.ReadLock(h.lock); err == nil {
			return info
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatal("serve never wrote server.json")
	return serveraddr.LockInfo{}
}

func (h *serveHarness) wait(d time.Duration) error {
	h.t.Helper()
	select {
	case <-h.done:
		return h.err
	case <-time.After(d):
		h.t.Fatalf("serve did not return within %v", d)
		return nil
	}
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var coded *ExitCodeError
	if !errors.As(err, &coded) {
		t.Fatalf("err = %v, want an *ExitCodeError", err)
	}
	return coded.Code
}

// The parent closing our stdin is a graceful stop: RunE returns nil (exit 0)
// inside the shutdown timeout, server.json is gone and the socket is free for
// a successor. Sabotage: removing the stdin watcher leaves RunE running until
// the 60 s context expires and wait() fails.
func TestServeSupervised_StdinEOFStopsCleanly(t *testing.T) {
	home := isolateServe(t)
	h := startServe(t, home, "--supervised")
	info := h.waitServing()
	if info.PID != os.Getpid() || info.Schema != serveraddr.LockSchemaV2 {
		t.Fatalf("server.json = %+v", info)
	}
	start := time.Now()
	h.stdin.Close()
	if err := h.wait(15 * time.Second); err != nil {
		t.Fatalf("stdin EOF is a clean stop, got %v", err)
	}
	t.Logf("stopped %v after stdin closed", time.Since(start))
	if _, err := os.Stat(h.lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("server.json survived the stop: %v", err)
	}
	sock, _ := config.SocketPath()
	ln, cleanup, err := auth.ListenLocal(sock)
	if err != nil || ln == nil {
		t.Fatalf("the socket is not free after the stop: %v", err)
	}
	ln.Close()
	cleanup()
}

// Unsupervised, a closed stdin means nothing: a service started with
// </dev/null must keep serving.
func TestServe_StdinEOFIsIgnoredWithoutSupervised(t *testing.T) {
	home := isolateServe(t)
	h := startServe(t, home)
	h.waitServing()
	h.stdin.Close()
	select {
	case <-h.done:
		t.Fatalf("unsupervised serve stopped when stdin closed: %v", h.err)
	case <-time.After(1500 * time.Millisecond):
	}
	h.cancel()
	if err := h.wait(15 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// http = false: serve answers on the local socket alone. The TCP port is not
// bound (it stays free while serve runs), server.json says port 0 and an empty
// http, and a request over the socket is served by the real handler.
func TestServe_HTTPFalseServesOverTheSocketAlone(t *testing.T) {
	home := isolateServe(t)
	port := os.Getenv("KNOMIT_PORT")
	t.Setenv("KNOMIT_HTTP", "false")
	h := startServe(t, home)
	info := h.waitServing()
	if info.Port != 0 || info.HTTP != "" || info.Socket == "" {
		t.Fatalf("server.json = %+v, want port 0, http \"\" and a socket", info)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("http = false but the port is bound: %v", err)
	}
	ln.Close()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return auth.DialLocal(ctx, info.Socket, 5*time.Second)
		},
	}}
	resp, err := client.Get("http://knomit.local/api/v1/version")
	if err != nil {
		t.Fatalf("GET over the socket: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/version over the socket = %d, want 200", resp.StatusCode)
	}
	h.cancel()
	if err := h.wait(15 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// Exit 3: the socket is held by another server. The supervisor attaches
// instead of restarting.
func TestServeSupervised_SocketHeldExits3(t *testing.T) {
	home := isolateServe(t)
	sock, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	owner, release, err := auth.ListenLocal(sock)
	if err != nil || owner == nil {
		t.Fatalf("owner: %v", err)
	}
	defer release()
	h := startServe(t, home, "--supervised")
	err = h.wait(40 * time.Second)
	if got := exitCode(t, err); got != ExitSupervisedHeld || !errors.Is(err, auth.ErrSocketInUse) {
		t.Fatalf("exit %d (%v), want %d wrapping ErrSocketInUse", got, err, ExitSupervisedHeld)
	}
}

// The same held socket WITHOUT --supervised keeps today's behaviour: serve
// TCP only, exit through the normal path.
func TestServe_SocketHeldServesTCPOnlyWithoutSupervised(t *testing.T) {
	home := isolateServe(t)
	sock, _ := config.SocketPath()
	owner, release, err := auth.ListenLocal(sock)
	if err != nil || owner == nil {
		t.Fatalf("owner: %v", err)
	}
	defer release()
	h := startServe(t, home)
	// It serves, but does not write the home's server.json: the owner of the
	// socket is the home's server and owns the file.
	deadline := time.Now().Add(40 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+os.Getenv("KNOMIT_PORT"), time.Second)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never listened on TCP: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(h.lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a TCP-only sibling wrote the home's server.json: %v", err)
	}
	h.cancel()
	if err := h.wait(15 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// Exit 2: a configuration the supervisor cannot fix by retrying.
func TestServeSupervised_ConfigFailureExits2(t *testing.T) {
	home := isolateServe(t)
	t.Setenv("KNOMIT_HTTP", "banana")
	h := startServe(t, home, "--supervised")
	err := h.wait(20 * time.Second)
	if got := exitCode(t, err); got != ExitSupervisedConfig {
		t.Fatalf("exit %d (%v), want %d", got, err, ExitSupervisedConfig)
	}
	// And unsupervised the same failure is the ordinary error.
	c := serveCmd()
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SetContext(context.Background())
	err = c.RunE(c, nil)
	var coded *ExitCodeError
	if err == nil || errors.As(err, &coded) {
		t.Fatalf("unsupervised config failure = %v, want a plain error", err)
	}
}

// tlsHome installs a certificate into home and returns the TLS address. With
// wrapOwnKey the certificate wraps serve's own key (a working listener);
// without it, another key's (a trust failure).
func tlsHome(t *testing.T, home string, wrapOwnKey bool) string {
	t.Helper()
	keyPath, _ := pkitest.NewKey(t)
	for _, suffix := range []string{"", ".pub"} {
		b, err := os.ReadFile(keyPath + suffix)
		if err != nil {
			if suffix == ".pub" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "id_ed25519"+suffix), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	certKey := keyPath
	if !wrapOwnKey {
		certKey, _ = pkitest.NewKey(t)
	}
	f := pkitest.New(t)
	f.Install(t, f.Enroll(t, "serve@agents.knomit.io", certKey), filepath.Join(home, "pki"))
	addr := "127.0.0.1:" + freePort(t)
	t.Setenv("KNOMIT_TLS_ADDR", addr)
	return addr
}

// Exit 2: a certificate that is installed but cannot be loaded is a trust
// failure; --supervised does not soften it into a warning.
func TestServeSupervised_TrustFailureExits2(t *testing.T) {
	home := isolateServe(t)
	tlsHome(t, home, false)
	h := startServe(t, home, "--supervised")
	err := h.wait(40 * time.Second)
	if got := exitCode(t, err); got != ExitSupervisedConfig {
		t.Fatalf("exit %d (%v), want %d", got, err, ExitSupervisedConfig)
	}
	if errors.Is(err, app.ErrTLSAddrInUse) {
		t.Fatalf("fixture failed on the bind, not on trust: %v", err)
	}
}

// A held [tls].addr is a warning under --supervised: serve carries on and
// server.json shows no TLS listener. (Unsupervised it fails the boot:
// TestServeTLS_BindConflictFailsTheBoot.)
func TestServeSupervised_TLSAddrHeldWarnsAndServes(t *testing.T) {
	home := isolateServe(t)
	addr := tlsHome(t, home, true)
	held, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	h := startServe(t, home, "--supervised")
	info := h.waitServing()
	if info.TLS != "" || info.Socket == "" || info.Port == 0 {
		t.Fatalf("server.json = %+v, want no TLS but the socket and the port", info)
	}
	h.stdin.Close()
	if err := h.wait(15 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// A held plaintext port is a warning under --supervised: serve carries on over
// the socket alone and server.json says port 0.
func TestServeSupervised_PortHeldWarnsAndServes(t *testing.T) {
	home := isolateServe(t)
	held, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("KNOMIT_PORT"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	h := startServe(t, home, "--supervised")
	info := h.waitServing()
	if info.Port != 0 || info.HTTP != "" || info.Socket == "" {
		t.Fatalf("server.json = %+v, want port 0 and the socket", info)
	}
	h.stdin.Close()
	if err := h.wait(15 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// server.json is written LAST: after OAuth and the diagnostics listener are
// bound, so it never names a server that then fails to come up. The ordering
// of the call site is invisible to behaviour tests (Advertise moved earlier
// leaves every one green), so this pins source order in serve's RunE:
// openOAuthServer and openDiagServer come before ls.Advertise.
func TestServe_AdvertiseIsAfterEveryOtherListener(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "serve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]token.Pos{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "openOAuthServer" || fn.Name == "openDiagServer" {
				pos[fn.Name] = call.Pos()
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "Advertise" || fn.Sel.Name == "OpenListeners" {
				pos[fn.Sel.Name] = call.Pos()
			}
		}
		return true
	})
	for _, name := range []string{"OpenListeners", "openOAuthServer", "openDiagServer", "Advertise"} {
		if !pos[name].IsValid() {
			t.Fatalf("serve.go has no call to %s (positive control: the pin must find all four)", name)
		}
	}
	if !(pos["OpenListeners"] < pos["openOAuthServer"] && pos["openOAuthServer"] < pos["Advertise"] && pos["openDiagServer"] < pos["Advertise"]) {
		t.Fatalf("order in serve.go is wrong: Open=%v oauth=%v diag=%v Advertise=%v; Advertise must be last",
			pos["OpenListeners"], pos["openOAuthServer"], pos["openDiagServer"], pos["Advertise"])
	}
}

// An unwritable server.json (a directory sits where the file goes) does not
// stop a plain `knomit serve`: WARN and keep serving. Supervised it is fatal,
// exit 2.
func TestServe_AdvertiseFailure(t *testing.T) {
	t.Run("plain serve warns and keeps serving", func(t *testing.T) {
		home := isolateServe(t)
		if err := os.Mkdir(filepath.Join(home, "server.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		h := startServe(t, home)
		waitSocket(t, h) // serving despite the unwritable file
		if fi, err := os.Stat(filepath.Join(home, "server.json")); err != nil || !fi.IsDir() {
			t.Fatalf("server.json was replaced or removed: %v %v", fi, err)
		}
		h.cancel()
		if err := h.wait(15 * time.Second); err != nil {
			t.Fatalf("stop: %v", err)
		}
	})
	t.Run("supervised exits 2", func(t *testing.T) {
		home := isolateServe(t)
		if err := os.Mkdir(filepath.Join(home, "server.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		h := startServe(t, home, "--supervised")
		err := h.wait(40 * time.Second)
		if got := exitCode(t, err); got != ExitSupervisedConfig || !strings.Contains(err.Error(), "write lockfile") {
			t.Fatalf("exit %d (%v), want %d naming the lockfile", got, err, ExitSupervisedConfig)
		}
	})
}

// A held OAuth or diagnostics address is an availability problem under
// --supervised, like TCP and TLS: WARN and serve. Unsupervised keeps its
// refusal (the positive control of each row).
func TestServe_HeldOAuthAndDiagAddresses(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, addr string)
	}{
		{"oauth", func(t *testing.T, addr string) {
			t.Setenv("KNOMIT_OAUTH_ISSUER", "https://kb.example.com")
			t.Setenv("KNOMIT_OAUTH_ADDR", addr)
		}},
		{"diag", func(t *testing.T, addr string) { t.Setenv("KNOMIT_RUNTIME_ADDR", addr) }},
	} {
		t.Run(tc.name+" supervised warns and serves", func(t *testing.T) {
			home := isolateServe(t)
			held, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			tc.set(t, held.Addr().String())
			h := startServe(t, home, "--supervised")
			info := h.waitServing()
			if info.Socket == "" {
				t.Fatalf("server.json = %+v", info)
			}
			h.stdin.Close()
			if err := h.wait(15 * time.Second); err != nil {
				t.Fatalf("stop: %v", err)
			}
		})
		t.Run(tc.name+" unsupervised still refuses", func(t *testing.T) {
			home := isolateServe(t)
			held, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			tc.set(t, held.Addr().String())
			h := startServe(t, home)
			err = h.wait(40 * time.Second)
			if err == nil || !app.IsAddrInUse(err) {
				t.Fatalf("unsupervised serve with a held %s address = %v, want an address-in-use failure", tc.name, err)
			}
		})
	}
}

// waitSocket waits until the local socket answers, for runs that write no
// server.json.
func waitSocket(t *testing.T, h *serveHarness) string {
	t.Helper()
	sock, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-h.done:
			t.Fatalf("serve returned before it was serving: %v", h.err)
		default:
		}
		if c, err := auth.DialLocal(context.Background(), sock, time.Second); err == nil {
			c.Close()
			return sock
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the socket never answered")
	return ""
}
