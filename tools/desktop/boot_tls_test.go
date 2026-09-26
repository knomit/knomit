//go:build desktop

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	knomitapp "knomit/internal/app"
	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/repos"
	"knomit/internal/web"
)

// The desktop as a fleet peer (knomit#256): bootServer opens the mTLS
// listener from app.OpenTLSServer, and an enrolled peer — a second
// pkitest instance, in this process — reaches the desktop's REAL handler
// stack through it. Named TestBootServer_* so Windows CI runs them too
// (.github/workflows/tests.yml runs -run 'TestWritable|TestBootServer_').
//
// Sabotage checks from the approved proposal, and which test each turns red:
//
//  1. TLS server ConnContext = auth.ConnContext        -> TLSPeerIsTheInstancePrincipal
//  2. drop ConnState: ps.ConnState                     -> TLSRevokedPeerIsCut
//  3. drop go ps.Run(ctx)                              -> TLSRevokedPeerIsCut
//  4. open TLS after lockfile.Write                    -> TLSFailureWritesNoLockfile
//  5. wrap the TLS listener's conns                    -> TLSPeerIsTheInstancePrincipal
//  6. bind the plaintext listener on 0.0.0.0           -> PlaintextStaysLoopback
//  7. swap Dir/Addr or drop KeyPath in tlsListenerFrom -> TestTLSListenerFrom_CarriesAddrDirKey
//  8. (PR 2, enrolment UI; not in this PR)
//  9. weaken the TLS layer's no-certificate refusal    -> TLSRefusesNoClientCert
//     (measured at 31f4072d: that refusal is TWO checks, and weakening ONE
//     leaves this green because the other still refuses at the TLS layer —
//     ClientAuth RequireAnyClientCert in pki's configFor, and snapshot.check's
//     "peer presented no certificate". Weakening BOTH — RequestClientCert
//     and check returning nil on no certificate — turns this red: the
//     request then reaches the middleware, whose 403 does not count.)
// 10. drop closeTLS on the lockfile.Write error path   -> TLSAddrFreeAfterLockfileFailure
// 11. open TLS before RequireLocalListener, no close   -> TLSAddrFreeAfterRequireRefusal
// 12. drop ReadTimeout / copy it from like             -> internal/app TestOpenTLSServer_OwnsItsTimeouts

// syncLog captures the global zerolog logger for the length of a test.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncLog) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// hasWarn reports whether one WARN line's message contains msg and its field
// key equals want EXACTLY. The line is parsed, not substring-matched: zerolog
// writes JSON, which escapes a Windows path's backslashes, so a raw
// strings.Contains on the path misses a line that is there (windows-2025 CI).
func (s *syncLog) hasWarn(msg, key, want string) bool {
	for _, line := range strings.Split(s.String(), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		m, _ := ev["message"].(string)
		if ev["level"] == "warn" && strings.Contains(m, msg) && ev[key] == want {
			return true
		}
	}
	return false
}

func captureLog(t *testing.T) *syncLog {
	t.Helper()
	buf := &syncLog{}
	prev := log.Logger
	log.Logger = zerolog.New(buf)
	t.Cleanup(func() { log.Logger = prev })
	return buf
}

// tlsNode is a desktop's worth of fleet state: its key, its enrolled
// certificate installed in its [tls].dir, and its real handler stack
// (web.Server with AuthMiddleware and the write gate), wrapped in a counter
// of every request that reached the HTTP layer at all.
type tlsNode struct {
	f       *pkitest.Fleet
	cfg     config.Config
	keyPath string
	handler http.Handler
	hits    *atomic.Int64
}

func newTLSNode(t *testing.T, addr string) tlsNode {
	t.Helper()
	cfg := testCfg(localListener{})
	cfg.Home = t.TempDir()
	cfg.TLS = config.TLSConfig{Addr: addr, Dir: filepath.Join(cfg.Home, "pki")}
	keyPath, _ := pkitest.NewKey(t)
	f := pkitest.New(t)
	f.Install(t, f.Enroll(t, "desktop", pki.RoleInstance, keyPath), cfg.TLS.Dir)

	mgr := repos.New(context.Background(), repos.Deps{Cfg: cfg, KeyPath: keyPath, AgentBranch: "agent/test-00000000"})
	if err := mgr.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })
	s := &web.Server{Manager: mgr, APIOnly: true, Auth: cfg.Auth, Grants: auth.NewSQLGrants(mgr.ControlDB())}
	inner := s.Handler()
	hits := &atomic.Int64{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		inner.ServeHTTP(w, r)
	})
	return tlsNode{f: f, cfg: cfg, keyPath: keyPath, handler: h, hits: hits}
}

func (n tlsNode) boot(t *testing.T) *server {
	t.Helper()
	srv, _, err := bootServer(context.Background(), n.handler, filepath.Join(t.TempDir(), "server.json"), "v", n.cfg, n.keyPath)
	if err != nil {
		t.Fatalf("bootServer: %v", err)
	}
	t.Cleanup(srv.shutdown)
	return srv
}

// peer enrolls a second instance in n's fleet with its own [tls].dir, which
// is what `pki.Dial` from a `knomit serve` peer needs.
func (n tlsNode) peer(t *testing.T) (pkitest.Member, string) {
	t.Helper()
	keyPath, _ := pkitest.NewKey(t)
	m := n.f.Enroll(t, "peer", pki.RoleInstance, keyPath)
	dir := filepath.Join(t.TempDir(), "pki")
	n.f.Install(t, m, dir)
	return m, dir
}

// freeAddr returns a loopback address nothing listens on right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	l.Close()
	return a
}

// addrFree reports whether addr can be bound again, closing it at once.
func addrFree(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return l.Close()
}

func TestBootServer_TLSOffWithoutAddr(t *testing.T) {
	n := newTLSNode(t, "")
	srv := n.boot(t)
	if srv.tls != nil || srv.tlsAddr != "" {
		t.Fatalf("TLS listener opened with no [tls].addr: %v %q", srv.tls, srv.tlsAddr)
	}
	if srv.tlsState != (tlsState{}) {
		t.Fatalf("TLS state %+v, want off", srv.tlsState)
	}
}

func TestBootServer_TLSConfiguredWithoutCertServesPlainOnlyAndWarns(t *testing.T) {
	logs := captureLog(t)
	n := newTLSNode(t, "127.0.0.1:0")
	n.cfg.TLS.Dir = filepath.Join(t.TempDir(), "empty-pki") // nothing installed here
	srv := n.boot(t)
	if srv.tls != nil {
		t.Fatal("TLS listener opened with no certificate installed")
	}
	if !logs.hasWarn("no instance certificate installed", "dir", n.cfg.TLS.Dir) {
		t.Fatalf("no WARN naming [tls].dir:\n%s", logs)
	}
	// What Settings shows: configured, not listening, and why.
	if want := (tlsState{Configured: "127.0.0.1:0", Reason: tlsNoCertificate}); srv.tlsState != want {
		t.Fatalf("TLS state %+v, want %+v", srv.tlsState, want)
	}
}

// The headline: a `knomit serve`-style peer in the same fleet reaches the
// desktop over mTLS and is seen as instance:<its fp>@cert — asserted by the
// 403 that names that EXACT principal, which only the real handshake could
// have produced — while pki.Dial, from the peer's side, sees the desktop's
// own identity.
func TestBootServer_TLSPeerIsTheInstancePrincipal(t *testing.T) {
	n := newTLSNode(t, "127.0.0.1:0")
	srv := n.boot(t)
	if want := (tlsState{Configured: "127.0.0.1:0", Listening: srv.tlsAddr}); srv.tlsAddr == "" || srv.tlsState != want {
		t.Fatalf("TLS state %+v, want %+v", srv.tlsState, want)
	}
	if srv.tlsAddr == "" {
		t.Fatal("no TLS listener with [tls].addr set and a certificate installed")
	}
	peer, peerDir := n.peer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := pki.Dial(ctx, srv.tlsAddr, peerDir, peer.KeyPath)
	if err != nil {
		t.Fatalf("pki.Dial from the peer: %v", err)
	}
	selfSigner, selfPub, err := pki.LoadSigner(n.keyPath)
	if err != nil || selfSigner == nil {
		t.Fatal(err)
	}
	if id.Fingerprint != pki.Fingerprint(selfPub) {
		t.Fatalf("the peer saw %s, want the desktop's own %s", id.Fingerprint, pki.Fingerprint(selfPub))
	}

	pc := n.f.Client(t, peer)
	resp, err := pc.Get("https://" + srv.tlsAddr + "/api/v1/repos")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("peer GET (implicit read for an instance): %v %v", resp, err)
	}
	resp.Body.Close()
	resp, err = pc.Post("https://"+srv.tlsAddr+"/api/v1/repos", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	want := auth.InstancePrincipal(peer.Fingerprint()).String()
	if resp.StatusCode != 403 || !strings.Contains(string(body), want) {
		t.Fatalf("peer POST: %d %s; want 403 naming %s", resp.StatusCode, body, want)
	}
}

// M3: refused at the TLS layer, not by the middleware. The client's own
// Handshake() is not the signal — in TLS 1.3 it completes before the server
// judges the client certificate — so the proof is that no HTTP response ever
// arrives AND not one request reached the HTTP layer. If the TLS layer let a
// certificate-less client through, the middleware would answer 403 and the
// counter would move: that is what this must catch (sabotage 9 above).
func TestBootServer_TLSRefusesNoClientCert(t *testing.T) {
	n := newTLSNode(t, "127.0.0.1:0")
	srv := n.boot(t)
	peer, _ := n.peer(t)
	cfg := n.f.Client(t, peer).Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg.Certificates = nil

	c, err := tls.Dial("tcp", srv.tlsAddr, cfg)
	if err == nil {
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(c, "GET /api/v1/repos HTTP/1.1\r\nHost: knomit\r\n\r\n")
		if resp, rerr := http.ReadResponse(bufio.NewReader(c), nil); rerr == nil {
			resp.Body.Close()
			t.Fatalf("a client with no certificate got an HTTP response: %d", resp.StatusCode)
		}
	}
	if h := n.hits.Load(); h != 0 {
		t.Fatalf("%d request(s) with no client certificate reached the HTTP layer", h)
	}
}

// knomit#258 on the desktop: bootServer must carry OpenTLSServer's ConnState
// and Run through, or an established connection from a revoked peer is never
// cut. One raw connection so http.Transport cannot retry on a fresh one.
func TestBootServer_TLSRevokedPeerIsCut(t *testing.T) {
	t.Cleanup(knomitapp.SetTLSRecheckIntervalForTest(50 * time.Millisecond))
	n := newTLSNode(t, "127.0.0.1:0")
	srv := n.boot(t)
	peer, _ := n.peer(t)

	c, err := tls.Dial("tcp", srv.tlsAddr, n.f.Client(t, peer).Transport.(*http.Transport).TLSClientConfig)
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

	n.f.Revoke(t, peer, n.cfg.TLS.Dir)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = br.ReadByte()
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("the revoked peer's established connection was not cut (read err %v)", err)
	}
}

// A TRUST failure — a certificate installed but its CRL gone — fails the
// boot, writes no lockfile, and leaves no plaintext port behind.
func TestBootServer_TLSFailureWritesNoLockfile(t *testing.T) {
	n := newTLSNode(t, "127.0.0.1:0")
	if err := os.Remove(filepath.Join(n.cfg.TLS.Dir, pki.CRLFile)); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(t.TempDir(), "server.json")
	n.cfg.Port = freeAddrPort(t)
	srv, _, err := bootServer(context.Background(), n.handler, lockPath, "v", n.cfg, n.keyPath)
	if err == nil {
		srv.shutdown()
		t.Fatal("bootServer came up with an installed certificate and no CRL")
	}
	if errors.Is(err, knomitapp.ErrTLSAddrInUse) || !errors.Is(err, pki.ErrCRLMissing) {
		t.Fatalf("fixture did not reach the trust failure: %v", err)
	}
	if _, serr := os.Stat(lockPath); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("lockfile written despite the TLS failure: %v", serr)
	}
	if err := addrFree("127.0.0.1:" + n.cfg.Port); err != nil {
		t.Fatalf("the plaintext port is still bound after the failed boot: %v", err)
	}
}

// freeAddrPort is freeAddr's port alone, for cfg.Port.
func freeAddrPort(t *testing.T) string {
	_, p, _ := net.SplitHostPort(freeAddr(t))
	return p
}

// User decision 2 (knomit#256): a held [tls].addr is an AVAILABILITY
// failure. The desktop boots, serves plaintext, writes its lockfile, and
// says why there is no TLS listener — rather than losing its UI and MCP
// server to a `knomit serve` that is already serving the fleet.
func TestBootServer_TLSAddrInUseWarnsAndServes(t *testing.T) {
	logs := captureLog(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	n := newTLSNode(t, held.Addr().String())
	lockPath := filepath.Join(t.TempDir(), "server.json")
	srv, port, err := bootServer(context.Background(), n.handler, lockPath, "v", n.cfg, n.keyPath)
	if err != nil {
		t.Fatalf("a held [tls].addr failed the boot: %v", err)
	}
	defer srv.shutdown()
	if srv.tls != nil {
		t.Fatal("a TLS server was kept although its address was held")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("no lockfile although plaintext IS serving: %v", err)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/v1/repos", port))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("plaintext does not serve: %v %v", resp, err)
	}
	resp.Body.Close()
	if !logs.hasWarn("serving without the mTLS listener", "addr", held.Addr().String()) {
		t.Fatalf("no WARN naming the held address:\n%s", logs)
	}
	if want := (tlsState{Configured: held.Addr().String(), Reason: tlsAddrInUse}); srv.tlsState != want {
		t.Fatalf("TLS state %+v, want %+v", srv.tlsState, want)
	}
}

// M4: RequireLocalListener refuses AFTER the plaintext and local listeners
// are open. Nothing TLS may be left bound behind the error.
func TestBootServer_TLSAddrFreeAfterRequireRefusal(t *testing.T) {
	path := localTestPath(t)
	owner, release, err := auth.ListenLocal(path)
	if err != nil || owner == nil {
		t.Fatalf("owner ListenLocal: %v %v", owner, err)
	}
	defer release()
	addr := freeAddr(t)
	n := newTLSNode(t, addr)
	n.cfg.Socket = path
	n.cfg.Auth.Require = true
	srv, _, err := bootServer(context.Background(), n.handler, filepath.Join(t.TempDir(), "server.json"), "v", n.cfg, n.keyPath)
	if err == nil {
		srv.shutdown()
		t.Fatal("require=true with the local listener held must refuse the boot")
	}
	// The refusal names its setting on both platforms; the error it wraps is
	// ErrSocketInUse for a held unix socket, and whatever the pipe namespace
	// reports on Windows.
	if !strings.Contains(err.Error(), "[auth].require = true") {
		t.Fatalf("fixture did not reach the RequireLocalListener refusal: %v", err)
	}
	if err := addrFree(addr); err != nil {
		t.Fatalf("[tls].addr still bound after the refused boot: %v", err)
	}
}

// M4: the lockfile is the last step; when it fails, the TLS listener opened
// just before it must be closed and its re-judging loop stopped.
func TestBootServer_TLSAddrFreeAfterLockfileFailure(t *testing.T) {
	addr := freeAddr(t)
	n := newTLSNode(t, addr)
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	srv, _, err := bootServer(context.Background(), n.handler, filepath.Join(notADir, "server.json"), "v", n.cfg, n.keyPath)
	if err == nil {
		srv.shutdown()
		t.Fatal("bootServer succeeded with an unwritable lockfile path")
	}
	if !strings.Contains(err.Error(), "write lockfile") {
		t.Fatalf("fixture did not reach the lockfile failure: %v", err)
	}
	if err := addrFree(addr); err != nil {
		t.Fatalf("[tls].addr still bound after the lockfile failure: %v", err)
	}
}

// Positive control for "only the TLS listener is network-facing": with TLS
// on, the plaintext port answers on loopback and NOT on this machine's
// non-loopback address.
func TestBootServer_PlaintextStaysLoopback(t *testing.T) {
	ip := nonLoopbackIPv4(t)
	n := newTLSNode(t, "127.0.0.1:0")
	n.cfg.Port = freeAddrPort(t)
	srv := n.boot(t)
	if srv.tls == nil {
		t.Fatal("fixture: the TLS listener is not on")
	}
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+n.cfg.Port, 2*time.Second)
	if err != nil {
		t.Fatalf("positive control: loopback does not answer: %v", err)
	}
	c.Close()
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, n.cfg.Port), 2*time.Second); err == nil {
		c.Close()
		t.Fatalf("the plaintext port answers on %s: it is not loopback-only", ip)
	}
}

// nonLoopbackIPv4 is an address of this machine that is not loopback. A
// runner with none cannot tell loopback-only from all-interfaces, so the
// test says so rather than passing.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address on this machine; loopback-only cannot be distinguished here")
	return ""
}

// A Settings restart relaunches straight after shutdown: the successor must
// find [tls].addr free and the old listener gone.
func TestBootServer_ShutdownClosesTLS(t *testing.T) {
	addr := freeAddr(t)
	n := newTLSNode(t, addr)
	srv, _, err := bootServer(context.Background(), n.handler, filepath.Join(t.TempDir(), "server.json"), "v", n.cfg, n.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if srv.tlsAddr != addr {
		t.Fatalf("TLS listener at %q, want %q", srv.tlsAddr, addr)
	}
	srv.shutdown()
	if err := addrFree(addr); err != nil {
		t.Fatalf("[tls].addr still bound after shutdown: %v", err)
	}
}
