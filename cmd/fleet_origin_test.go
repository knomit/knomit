package cmd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web"
)

// SABOTAGE (against the commit that added this file): ClassifyPeerError
// returning err unchanged turns step 6 red ("remote error: tls: bad
// certificate", unnamed). The global token is kept from a fleet peer by FOUR
// layers: remoteAuthFromRecord's fleet return and resolveAuthWithOrigin's
// fleet branch (repos), the transport's refusal of a non-nil AuthMethod, and
// the transport handing go-git nil auth. Measured:
//   - both repos layers off: step 3 goes red LOUDLY — the transport refuses
//     the credential by name ("a http-basic-auth credential is refused"). That
//     loud refusal is deliberate; do not turn it into a silent drop.
//   - repos layers AND the transport refusal off: green, because the last
//     layer silently strips the credential. Nothing leaks, but nothing is
//     said either, which is why the refusal above exists.
//   - all four off: step 4 red, naming three requests (info/refs twice,
//     upload-pack) that carried the token.
// The repos and pki unit tests pin the layers one by one.
// IdleTimeout 0 turns step 6 red with
// a nil error — the kept-alive connection from before the revocation is
// reused and the revoked instance still fetches.
//
// F19 phase 2b end to end: knomit's go-git client (the knomit+https
// transport) fetching from knomit's REAL /git handler — web.Server with
// GitHandler = web.GitRemoteHandler, AuthMiddleware and the write gate —
// served on the real mTLS listener from openTLSServer. The pki tests prove
// the transport against a minimal upload-pack server; this proves the pair.
//
// Only A, the fetcher, uses the client transport, and A is the only identity
// this process ever installs: pki.InstallGitTransport registers once per
// process, which is why this is ONE test and no other test in the package
// installs.

// headerLog records every request that carried an Authorization header, and
// whether it came in on the TLS listener.
type headerLog struct {
	mu   sync.Mutex
	hits []string
}

func (h *headerLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			h.mu.Lock()
			via := "plain"
			if r.TLS != nil {
				via = "tls"
			}
			h.hits = append(h.hits, via+" "+r.URL.Path)
			h.mu.Unlock()
		}
		next.ServeHTTP(w, r)
	})
}

func (h *headerLog) count(via string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.hits {
		if strings.HasPrefix(s, via+" ") {
			n++
		}
	}
	return n
}

// serveFleetNode is serveBoth with a short IdleTimeout on the plaintext
// server, which openTLSServer copies to the TLS one. Revocation is checked at
// the handshake, so a peer's kept-alive connection outlives it until the idle
// timeout closes it (60s in `knomit serve`); the short one lets this test
// reach the next handshake without waiting a minute.
func serveFleetNode(t *testing.T, handler http.Handler, keyPath string, tcfg config.TLSConfig) (plain, tlsAddr string) {
	t.Helper()
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       200 * time.Millisecond,
		ConnContext:       auth.ConnContext,
	}
	go srv.Serve(pl)
	t.Cleanup(func() { srv.Close() })
	tlsSrv, tl, err := openTLSServer(tcfg, keyPath, srv)
	if err != nil || tlsSrv == nil {
		t.Fatalf("openTLSServer: %v (server %v)", err, tlsSrv)
	}
	go tlsSrv.Serve(tl)
	t.Cleanup(func() { tlsSrv.Close() })
	return pl.Addr().String(), tl.Addr().String()
}

func factBody(title string) string {
	return "---\ntype: observation\nconfidence: 0.9\nsources: 1\ndomain: [peering]\n" +
		"entities: []\nrefs: []\n---\n# " + title + "\n\n" + title + "\n"
}

// publish writes a fact on B's agent branch and fast-forwards main to it,
// which is what B's local reconcile does.
func publish(t *testing.T, ri *repos.RepoInstance, agent, name string) {
	t.Helper()
	ctx := context.Background()
	if err := ri.WithRead(func(s *store.Service) {
		if _, err := s.Facts().WriteFact(ctx, agent, "kb/"+name+".md", factBody(name), name, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AdvanceLocalUpstream(ctx, agent, "main"); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func hasFact(t *testing.T, ri *repos.RepoInstance, name string) bool {
	t.Helper()
	found := false
	if err := ri.WithRead(func(s *store.Service) {
		f, err := s.Facts().ReadFact(context.Background(), "main", "kb/"+name+".md", nil)
		found = err == nil && strings.Contains(f.Content, name)
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

func TestFleetOrigin_SubscribeProbeSyncAndRevokeOverKnomitHTTPS(t *testing.T) {
	ctx := context.Background()
	f := pkitest.New(t)

	// B: the serving instance, the production stack on both listeners.
	bCfg := config.Defaults()
	bCfg.Home = t.TempDir()
	bCfg.OntologyRoot = "kb"
	bCfg.TLS = config.TLSConfig{Addr: "127.0.0.1:0", Dir: filepath.Join(bCfg.Home, "pki")}
	bKey, _ := pkitest.NewKey(t)
	const bAgent = "agent/bravo-00000000"
	bMgr := repos.New(ctx, repos.Deps{Cfg: bCfg, KeyPath: bKey, AgentBranch: bAgent, DisableBackgroundSync: true})
	if err := bMgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bMgr.Close() })
	bSrv := &web.Server{
		Manager:    bMgr,
		GitHandler: web.GitRemoteHandler(bMgr),
		APIOnly:    true,
		Auth:       bCfg.Auth,
		Grants:     auth.NewSQLGrants(bMgr.ControlDB()),
	}
	hdrs := &headerLog{}
	f.Install(t, f.Enroll(t, "bravo", pki.RoleInstance, bKey), bCfg.TLS.Dir)
	plain, tlsAddr := serveFleetNode(t, hdrs.wrap(bSrv.Handler()), bKey, bCfg.TLS)

	riB, err := bMgr.Create(ctx, repos.CreateSpec{Name: "kb", Mode: "preset"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, riB, bAgent, "first")
	riB2, err := bMgr.Create(ctx, repos.CreateSpec{Name: "kb2", Mode: "preset"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, riB2, bAgent, "plainfact")

	// A: the fetcher, enrolled in the same fleet, WITH a global forge token
	// configured (KNOMIT_REMOTE_AUTH=token) that must never reach B.
	aHome := t.TempDir()
	aKey, _ := pkitest.NewKey(t)
	aDir := filepath.Join(aHome, "pki")
	aMember := f.Enroll(t, "alpha", pki.RoleInstance, aKey)
	f.Install(t, aMember, aDir)
	if err := pki.InstallGitTransport(aDir, aKey); err != nil {
		t.Fatalf("install: %v", err)
	}
	aCfg := config.Config{Home: aHome, OntologyRoot: "kb",
		Remote: config.RemoteAuthConfig{AuthMethod: "token", Token: "ghp_global_forge_token"}}
	aMgr := repos.New(ctx, repos.Deps{Cfg: aCfg, KeyPath: aKey, AgentBranch: "agent/alpha-00000000", DisableBackgroundSync: true})
	if err := aMgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aMgr.Close() })

	fleetURL := pki.GitScheme + "://" + tlsAddr + "/git/kb"

	// 1. Probe over knomit+https: B's branches, through B's real handler.
	res, err := aMgr.ProbeOrigin(ctx, repos.OriginSpec{URL: fleetURL, AuthMethod: "cert"})
	if err != nil || !res.Reachable || res.UpstreamBranch != "main" {
		t.Fatalf("probe: %+v err=%v", res, err)
	}

	// 2. Subscribe (clone) with NO auth method — the S1 case: the stored
	//    origin has no method, so the sync below resolves from the record
	//    plus the GLOBAL token config. (An origin stored as "cert" overrides
	//    the global method by itself and would not exercise the rule; the
	//    explicit cert method is covered by the probe above.)
	riA, err := aMgr.Create(ctx, repos.CreateSpec{Name: "peer", Mode: "subscribe",
		Origin: &repos.OriginSpec{URL: fleetURL}}, nil)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if !hasFact(t, riA, "first") {
		t.Fatal("A's clone does not hold B's fact")
	}

	// 3. Sync — the path that resolves auth from the stored origin plus the
	//    GLOBAL [remote] config — brings B's next fact.
	publish(t, riB, bAgent, "second")
	if err := riA.ActivateSync(fleetURL); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !hasFact(t, riA, "second") {
		t.Fatal("A's sync did not bring B's second fact")
	}

	// 4. S1: not one request on the TLS listener carried an Authorization
	//    header, although A has a global token configured.
	if n := hdrs.count("tls"); n != 0 {
		t.Fatalf("%d fleet requests carried an Authorization header; the global forge token reached a peer: %v", n, hdrs.hits)
	}

	// 5. Positive control, and "nothing regressed" for plaintext origins: an
	//    http:// origin of the same B, stored with no method, syncs with the
	//    global token (the create's clone resolves from the spec alone and is
	//    anonymous; the sync resolves from the record plus the global config)
	//    — so the header log above would have seen a token had one been sent.
	plainURL := "http://" + plain + "/git/kb2"
	riP, err := aMgr.Create(ctx, repos.CreateSpec{Name: "plainpeer", Mode: "subscribe",
		Origin: &repos.OriginSpec{URL: plainURL}}, nil)
	if err != nil {
		t.Fatalf("plaintext subscribe: %v", err)
	}
	if !hasFact(t, riP, "plainfact") {
		t.Fatal("plaintext origin: fact missing")
	}
	if err := riP.ActivateSync(plainURL); err != nil {
		t.Fatalf("plaintext sync: %v", err)
	}
	if hdrs.count("plain") == 0 {
		t.Fatal("positive control: the global token was never sent to the plaintext origin, so the fleet assertion proves nothing")
	}

	// 6. B revokes A. After the idle timeout closes the kept-alive
	//    connection, A's next sync handshakes and is refused by name.
	f.Revoke(t, aMember, bCfg.TLS.Dir)
	time.Sleep(500 * time.Millisecond)
	publish(t, riB, bAgent, "third")
	err = riA.ActivateSync(fleetURL)
	if !errors.Is(err, pki.ErrRefusedByPeer) && (err == nil || !strings.Contains(err.Error(), pki.ErrRefusedByPeer.Error())) {
		t.Fatalf("sync after revocation: err=%v, want the ErrRefusedByPeer refusal", err)
	}
	if hasFact(t, riA, "third") {
		t.Fatal("a revoked instance still fetched")
	}
	last := lastSyncError(t, riA)
	if !strings.Contains(last, pki.ErrRefusedByPeer.Error()) {
		t.Fatalf("remote status LastError = %q, want the refusal named", last)
	}

	// 7. The probe carries the named refusal in detail (the JSON field the
	//    create wizard renders for an unreachable remote).
	res, err = aMgr.ProbeOrigin(ctx, repos.OriginSpec{URL: fleetURL, AuthMethod: "cert"})
	if err != nil || res.Reachable || !strings.Contains(res.Detail, pki.ErrRefusedByPeer.Error()) {
		t.Fatalf("probe after revocation: %+v err=%v", res, err)
	}
}

func lastSyncError(t *testing.T, ri *repos.RepoInstance) string {
	t.Helper()
	var last string
	if err := ri.WithRead(func(s *store.Service) {
		r, err := s.Remote().GetRemote("origin")
		if err != nil || r == nil {
			t.Fatalf("origin remote: %v", err)
		}
		if r.LastError != nil {
			last = *r.LastError
		}
	}); err != nil {
		t.Fatal(err)
	}
	return last
}
