package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/repos"
	"knomit/internal/web"
)

// syncBuffer is a goroutine-safe log sink: the TLS server logs from its own
// connection goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Logger
	log.Logger = zerolog.New(buf)
	t.Cleanup(func() { log.Logger = prev })
	return buf
}

// serveBoth starts what `knomit serve` starts for these two listeners: the
// plaintext http.Server over handler and, through openTLSServer, the TLS one
// beside it. It returns both addresses.
func serveBoth(t *testing.T, handler http.Handler, keyPath string, tcfg config.TLSConfig) (plain, tlsAddr string) {
	t.Helper()
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext:       auth.ConnContext,
	}
	go srv.Serve(pl)
	t.Cleanup(func() { srv.Close() })

	tlsSrv, tl, err := openTLSServer(t.Context(), tcfg, keyPath, srv)
	if err != nil {
		t.Fatal(err)
	}
	if tlsSrv == nil {
		t.Fatal("openTLSServer did not open the listener with a certificate installed")
	}
	go tlsSrv.Serve(tl)
	t.Cleanup(func() { tlsSrv.Close() })
	return pl.Addr().String(), tl.Addr().String()
}

// serverStack is the production server minus the embedder: the repo manager
// over a real control.db, and web.Server with the SQLGrants app.New wires.
// app.New itself is not used because it DOWNLOADS the embedding model into
// <Home>/models, which a cmd test on every CI platform must not do; its own
// wiring of Auth and Grants is pinned by internal/app's tests.
func serverStack(t *testing.T, cfg config.Config, keyPath string) http.Handler {
	t.Helper()
	mgr := repos.New(context.Background(), repos.Deps{Cfg: cfg, KeyPath: keyPath, AgentBranch: "agent/test-00000000"})
	if err := mgr.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })
	s := &web.Server{
		Manager: mgr,
		APIOnly: true,
		Auth:    cfg.Auth,
		Grants:  auth.NewSQLGrants(mgr.ControlDB()),
	}
	return s.Handler()
}

func status(t *testing.T, c *http.Client, method, url string) (int, string, error) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// The phase 2 end-to-end, over a real App and both real listeners:
// an enrolled peer reads; its mutation is refused naming its full-fingerprint
// principal (default deny); a write row lets it through; revoking its serial
// refuses its next connection at the handshake with the reason logged; a
// client with no certificate never gets in; and the plaintext port answers
// exactly as before throughout.
func TestServeTLS_EndToEnd(t *testing.T) {
	logs := captureLog(t)
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	cfg.TLS = config.TLSConfig{Addr: "127.0.0.1:0", Dir: cfg.Home + "/pki"}
	keyPath, _ := pkitest.NewKey(t)
	handler := serverStack(t, cfg, keyPath)

	f := pkitest.New(t)
	self := f.Enroll(t, "server", pki.RoleInstance, keyPath) // the server's OWN key
	f.Install(t, self, cfg.TLS.Dir)
	plain, tlsAddr := serveBoth(t, handler, keyPath, cfg.TLS)

	peer := f.Enroll(t, "peer", pki.RoleInstance)
	principal := auth.InstancePrincipal(peer.Fingerprint()).String()
	pc := f.Client(t, peer)

	// 1. Read: implicit for an enrolled instance.
	code, body, err := status(t, pc, "GET", "https://"+tlsAddr+"/api/v1/repos")
	if err != nil || code != 200 {
		t.Fatalf("peer GET: code=%d err=%v body=%s", code, err, body)
	}

	// 2. Mutation without a row: 403 naming the principal. This is the proof
	//    the principal STRING arrived through the real handshake.
	code, body, err = status(t, pc, "POST", "https://"+tlsAddr+"/api/v1/repos")
	if err != nil || code != 403 || !strings.Contains(body, "Permission denied") || !strings.Contains(body, principal) {
		t.Fatalf("peer POST without write: code=%d err=%v body=%s", code, err, body)
	}

	// 3. A write row (what `knomit grants add` writes) lets the same request
	//    past the gate; the handler then judges the empty body on its own.
	if err := withGrantsAt(cfg, func(g *auth.SQLGrants) error { // what `knomit grants add` does
		return g.Grant(context.Background(), auth.InstancePrincipal(peer.Fingerprint()), auth.Write, "test")
	}); err != nil {
		t.Fatal(err)
	}
	code, body, err = status(t, pc, "POST", "https://"+tlsAddr+"/api/v1/repos")
	if err != nil || code == 403 || strings.Contains(body, "Permission denied") || strings.Contains(body, "Authentication required") {
		t.Fatalf("peer POST with write still gated: code=%d err=%v body=%s", code, err, body)
	}

	// 4. No client certificate: refused at the handshake, never anonymous,
	//    even though it comes from loopback.
	noCert := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: f.Client(t, peer).Transport.(*http.Transport).TLSClientConfig.Clone(),
	}}
	noCert.Transport.(*http.Transport).TLSClientConfig.Certificates = nil
	if _, _, err := status(t, noCert, "GET", "https://"+tlsAddr+"/api/v1/repos"); err == nil {
		t.Fatal("a client with no certificate got a response from the TLS listener")
	}

	// 5. Revoke the peer and publish the CRL into [tls].dir: the running
	//    server refuses its next connection, and the log names why.
	f.Revoke(t, peer, cfg.TLS.Dir)
	if _, _, err := status(t, pc, "GET", "https://"+tlsAddr+"/api/v1/repos"); err == nil {
		t.Fatal("a revoked peer still gets in")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), pki.ErrRevoked.Error()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), pki.ErrRevoked.Error()) {
		t.Fatalf("revocation refusal not logged with its reason:\n%s", logs)
	}

	// 6. Positive control for "nothing changes on the plain port": loopback
	//    http:// answers as it did before phase 2, as the anonymous principal.
	resp, err := http.Get("http://" + plain + "/api/v1/repos")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("plaintext GET: %v %v", resp, err)
	}
	resp.Body.Close()
}

// knomit#258 through the production wiring: openTLSServer must hand the
// http.Server the pki.Server's ConnState and start its ticker, or an
// established connection from a revoked peer is never cut. One raw
// connection, HTTP/1.1 written by hand, so http.Transport cannot retry on a
// fresh connection and let a handshake do the refusing.
//
// Sabotage (run against the commit that added it): dropping ConnState from
// the http.Server, or not starting Run, leaves the connection open and this
// fails.
func TestServeTLS_RevocationCutsAnEstablishedConnection(t *testing.T) {
	prev := tlsRecheckInterval
	tlsRecheckInterval = 50 * time.Millisecond
	t.Cleanup(func() { tlsRecheckInterval = prev })
	logs := captureLog(t)
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	cfg.TLS = config.TLSConfig{Addr: "127.0.0.1:0", Dir: cfg.Home + "/pki"}
	keyPath, _ := pkitest.NewKey(t)
	handler := serverStack(t, cfg, keyPath)
	f := pkitest.New(t)
	f.Install(t, f.Enroll(t, "server", pki.RoleInstance, keyPath), cfg.TLS.Dir)
	_, tlsAddr := serveBoth(t, handler, keyPath, cfg.TLS)

	peer := f.Enroll(t, "peer", pki.RoleInstance)
	c, err := tls.Dial("tcp", tlsAddr, f.Client(t, peer).Transport.(*http.Transport).TLSClientConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "GET /api/v1/repos HTTP/1.1\r\nHost: knomit\r\n\r\n")
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("peer GET before revocation: %v %v", resp, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	f.Revoke(t, peer, cfg.TLS.Dir)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = br.ReadByte()
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("the revoked peer's established connection was not cut (read err %v)\n%s", err, logs)
	}
	if !strings.Contains(logs.String(), "tls_conn_closed") || !strings.Contains(logs.String(), pki.ErrRevoked.Error()) {
		t.Fatalf("the cut was not logged with its reason:\n%s", logs)
	}
}

// [tls].addr set but no certificate installed: WARN, plaintext only, no error.
func TestOpenTLSServer_ConfiguredWithoutCertificateIsOffAndWarns(t *testing.T) {
	logs := captureLog(t)
	srv, ln, err := openTLSServer(t.Context(), config.TLSConfig{Addr: "127.0.0.1:0", Dir: t.TempDir()}, "/nonexistent", &http.Server{})
	if err != nil || srv != nil || ln != nil {
		t.Fatalf("srv=%v ln=%v err=%v; want all nil", srv, ln, err)
	}
	if !strings.Contains(logs.String(), "no instance certificate installed") {
		t.Fatalf("no WARN logged:\n%s", logs)
	}
	if srv, ln, err := openTLSServer(t.Context(), config.TLSConfig{Dir: t.TempDir()}, "", &http.Server{}); srv != nil || ln != nil || err != nil {
		t.Fatal("empty addr must mean off, silently")
	}
}

// A certificate is installed but the CRL is gone: fail closed, an error.
func TestOpenTLSServer_InstalledButCRLMissingIsAnError(t *testing.T) {
	f := pkitest.New(t)
	self := f.Enroll(t, "server", pki.RoleInstance)
	dir := t.TempDir()
	f.Install(t, self, dir)
	if err := removeFile(dir + "/" + pki.CRLFile); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openTLSServer(t.Context(), config.TLSConfig{Addr: "127.0.0.1:0", Dir: dir}, self.KeyPath, &http.Server{}); err == nil {
		t.Fatal("opened a TLS listener with no CRL")
	}
}

func removeFile(p string) error { return os.Remove(p) }
