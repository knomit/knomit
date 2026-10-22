package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"

	"knomit/internal/corpus/repos"

	"knomit/internal/corpus/repos/control"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/storetest"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki"
	"knomit/internal/foundation/pki/pkitest"
	"knomit/internal/testsupport/testsigner"
)

// requireCertOrigin: ri's origin is stored in control.db as the plain https
// URL with method cert (knomit's decision), and only the derived .git/config
// holds the fleet transport's internal form.
func requireCertOrigin(t *testing.T, mgr *repos.Manager, ri *repos.RepoInstance, url string) {
	t.Helper()
	o, err := mgr.Origins().Get(ri.UID())
	if err != nil || o == nil {
		t.Fatalf("control.db origin of %s: %v %+v", ri.Name(), err, o)
	}
	if o.URL != url || o.AuthMethod != "cert" {
		t.Fatalf("control.db origin of %s = %q method %q, want %q method cert", ri.Name(), o.URL, o.AuthMethod, url)
	}
	want, err := pki.GitURL(url, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := ri.WithRead(func(s *store.Service) { got = storetest.RemoteURLs(s, "origin") }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf(".git/config origin of %s = %v, want [%s]", ri.Name(), got, want)
	}
}

// countStockHTTPS wraps go-git's stock https transport for one test and
// counts the sessions it opens.
func countStockHTTPS(t *testing.T) *atomic.Int32 {
	t.Helper()
	prev := transportclient.Protocols["https"]
	n := &atomic.Int32{}
	transportclient.InstallProtocol("https", countingTransport{prev, n})
	t.Cleanup(func() { transportclient.InstallProtocol("https", prev) })
	return n
}

type countingTransport struct {
	transport.Transport
	n *atomic.Int32
}

func (c countingTransport) NewUploadPackSession(ep *transport.Endpoint, a transport.AuthMethod) (transport.UploadPackSession, error) {
	c.n.Add(1)
	return c.Transport.NewUploadPackSession(ep, a)
}

func (c countingTransport) NewReceivePackSession(ep *transport.Endpoint, a transport.AuthMethod) (transport.ReceivePackSession, error) {
	c.n.Add(1)
	return c.Transport.NewReceivePackSession(ep, a)
}

// fetcher is A: enrolled in f, its fleet transport installed, a manager whose
// [tls].dir holds the fleet trust and whose GLOBAL [remote] is a forge token.
type fetcher struct {
	dir, key string
	member   pkitest.Member
	mgr      *repos.Manager
}

func newFetcher(t *testing.T, f *pkitest.Fleet) *fetcher {
	t.Helper()
	dir, key, m, _ := newFleetFetcherHome(t, f, "alpha")
	if err := pki.InstallGitTransport(dir, key); err != nil {
		t.Fatalf("install: %v", err)
	}
	cfg := config.Config{Home: t.TempDir(), OntologyRoot: "kb", TLS: config.TLSConfig{Dir: dir},
		Remote: config.RemoteAuthConfig{AuthMethod: "token", Token: "ghp_global_forge_token"}}
	mgr := repos.New(context.Background(), repos.Deps{Cfg: cfg, KeyPath: key, AgentID: testsigner.FixtureAgentID("agent/alpha-00000000"), AgentBranch: "agent/alpha-00000000", Machine: repos.Options{Synchronous: true}})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Close() })
	return &fetcher{dir: dir, key: key, member: m, mgr: mgr}
}

// remount archives and restores ri: its next open configures the git remote
// from control.db, and the first sync round runs from the STORED origin,
// with no classification.
func remount(t *testing.T, mgr *repos.Manager, ri *repos.RepoInstance) *repos.RepoInstance {
	t.Helper()
	archived, err := mgr.Archive(ri.Name())
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	ri, err = mgr.Restore(archived.ID, "")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	return ri
}

// An attach of a peer's plain https URL with no auth is classified once:
// control.db holds the https URL with method cert, .git/config the internal
// form. The same URL with a token is refused by name, nothing is stored, and
// no request reaches the peer with an Authorization header.
// SABOTAGE: Manager.Send not classifying evAttachOrigin -> the method stays
// "" -> red; ResolveOrigin dropping the credential instead of refusing it ->
// the token attach succeeds -> red.
func TestFleetOrigin_AttachDecidesThePeerOnce(t *testing.T) {
	ctx := context.Background()
	f := pkitest.New(t)
	b := newFleetServer(t, f)
	a := newFetcher(t, f)
	url := "https://" + b.tlsAddr + "/git/kb"

	tokenRepo, err := a.mgr.Create(ctx, repos.CreateSpec{Name: "local", Mode: "preset"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.mgr.Send(ctx, tokenRepo, repos.AttachOrigin(repos.OriginSpec{URL: url, AuthMethod: "token", AuthToken: "ghp_x"}))
	if !errors.Is(err, repos.ErrPeerCredential) {
		t.Fatalf("attach with a token: %v, want ErrPeerCredential", err)
	}
	if o, err := a.mgr.Origins().Get(tokenRepo.UID()); err != nil || o != nil {
		t.Fatalf("a refused attach stored %+v (%v)", o, err)
	}
	if n := b.hdrs.count("tls"); n != 0 {
		t.Fatalf("%d requests carried an Authorization header to the peer: %v", n, b.hdrs.hits)
	}

	if _, err := a.mgr.Send(ctx, tokenRepo, repos.AttachOrigin(repos.OriginSpec{URL: url})); err != nil {
		t.Fatalf("attach: %v", err)
	}
	requireCertOrigin(t, a.mgr, tokenRepo, url)
	if n := b.hdrs.count("tls"); n != 0 {
		t.Fatalf("%d requests carried an Authorization header to the peer: %v", n, b.hdrs.hits)
	}
}

// The Connect wizard's commit (a store swap carrying an origin) classifies
// its origin like an attach: a peer's https URL with a token is refused by
// name and nothing is stored; with no auth it is stored with method cert and
// the derived git config holds the internal form.
// SABOTAGE: Manager.Send not classifying evSwapStore -> the token swap
// succeeds and the method stays "" -> red.
func TestFleetOrigin_SwapDecidesThePeerOnce(t *testing.T) {
	ctx := context.Background()
	f := pkitest.New(t)
	b := newFleetServer(t, f)
	a := newFetcher(t, f)
	url := "https://" + b.tlsAddr + "/git/kb"

	ri, err := a.mgr.Create(ctx, repos.CreateSpec{Name: "local", Mode: "preset"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.mgr.Send(ctx, ri, repos.SwapStore(repos.SwapSpec{TempDB: copyOwnDB(t, a.mgr, ri),
		Origin: repos.OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "ghp_x"}}))
	if !errors.Is(err, repos.ErrPeerCredential) {
		t.Fatalf("swap with a token: %v, want ErrPeerCredential", err)
	}
	if o, err := a.mgr.Origins().Get(ri.UID()); err != nil || o != nil {
		t.Fatalf("a refused swap stored %+v (%v)", o, err)
	}
	if n := b.hdrs.count("tls"); n != 0 {
		t.Fatalf("%d requests carried an Authorization header to the peer: %v", n, b.hdrs.hits)
	}

	if _, err := a.mgr.Send(ctx, ri, repos.SwapStore(repos.SwapSpec{TempDB: copyOwnDB(t, a.mgr, ri),
		Origin: repos.OriginSpec{URL: url, Branch: "main"}})); err != nil {
		t.Fatalf("swap: %v", err)
	}
	requireCertOrigin(t, a.mgr, ri, url)
	if n := b.hdrs.count("tls"); n != 0 {
		t.Fatalf("%d requests carried an Authorization header to the peer: %v", n, b.hdrs.hits)
	}
}

// copyOwnDB checkpoints ri and copies its database: a valid swap source with
// the same root commit.
func copyOwnDB(t *testing.T, mgr *repos.Manager, ri *repos.RepoInstance) string {
	t.Helper()
	var cerr error
	if err := ri.WithRead(func(s *store.Service) { cerr = s.Checkpoint() }); err != nil || cerr != nil {
		t.Fatalf("checkpoint: %v %v", err, cerr)
	}
	raw, err := os.ReadFile(mgr.RepoPath(ri.UID()))
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "swap.db")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return tmp
}

// A stored cert origin is never re-classified: once its peer is revoked (or
// this instance's trust is uninstalled) the sync round fails with the named
// error, the method stays cert, and go-git's stock https transport is never
// asked for a session. A URL stored with the internal scheme fails every
// open and sync with "use https://" before anything is dialled.
// SABOTAGE: resolveAuthWithOrigin not calling pki.GitURL -> the stored
// internal URL syncs -> red.
func TestFleetOrigin_StoredCertOriginFailsClosed(t *testing.T) {
	ctx := context.Background()
	f := pkitest.New(t)
	b := newFleetServer(t, f)
	a := newFetcher(t, f)
	url := "https://" + b.tlsAddr + "/git/kb"
	stock := countStockHTTPS(t)

	ri, err := a.mgr.Create(ctx, repos.CreateSpec{Name: "peer", Mode: "subscribe", Origin: &repos.OriginSpec{URL: url}}, nil)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	requireCertOrigin(t, a.mgr, ri, url)

	// Revoked: A's CRL lists B.
	f.Revoke(t, b.member, a.dir)
	ri = remount(t, a.mgr, ri)
	if last := lastSyncError(t, ri); !strings.Contains(last, pki.ErrRevoked.Error()) {
		t.Fatalf("sync after B's revocation: LastError %q, want %q", last, pki.ErrRevoked)
	}
	if o, _ := a.mgr.Origins().Get(ri.UID()); o == nil || o.AuthMethod != "cert" {
		t.Fatalf("the stored method changed: %+v", o)
	}
	if n := stock.Load(); n != 0 {
		t.Fatalf("stock https opened %d sessions for a cert origin", n)
	}

	// Uninstalled: no certificate, so ErrNotEnrolled before any dial.
	if err := pki.Uninstall(a.dir); err != nil {
		t.Fatal(err)
	}
	before := b.hdrs.tlsRequests()
	ri = remount(t, a.mgr, ri)
	if last := lastSyncError(t, ri); !strings.Contains(last, pki.ErrNotEnrolled.Error()) {
		t.Fatalf("sync after the uninstall: LastError %q, want %q", last, pki.ErrNotEnrolled)
	}
	if n := b.hdrs.tlsRequests() - before; n != 0 {
		t.Fatalf("%d requests reached the peer after the uninstall", n)
	}

	// The internal scheme stored by hand: refused at open and at sync.
	internal, err := pki.GitURL(url, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.mgr.Origins().Set(ri.UID(), control.Origin{URL: internal, Branch: "main", AuthMethod: "cert", Mode: control.OriginModeSubscribe}); err != nil {
		t.Fatal(err)
	}
	before = b.hdrs.tlsRequests()
	ri = remount(t, a.mgr, ri)
	if last := lastSyncError(t, ri); !strings.Contains(last, "use https://") {
		t.Fatalf("sync of a stored internal URL: LastError %q, want the use-https refusal", last)
	}
	if n := b.hdrs.tlsRequests() - before; n != 0 || stock.Load() != 0 {
		t.Fatalf("a stored internal URL was dialled: %d peer requests, %d stock sessions", n, stock.Load())
	}
}

// Fleet create with another instance's https URL as its origin: refuseInstalledTrust
// guarantees this instance holds no fleet trust, so the URL is not a peer,
// stock https fails its handshake, and nothing of the fleet is left: no
// repository, no create marker, no <home>/fleet. (Replaces the removed
// knomit_origin refusal.)
func TestFleetCreate_APeerOriginCannotSucceed(t *testing.T) {
	ctx := context.Background()
	f := pkitest.New(t)
	b := newFleetServer(t, f)
	n := newFleetNode(t, t.TempDir(), "founder")
	_, err := n.mgr.CreateFleet(ctx, repos.FleetCreateSpec{Origin: &repos.OriginSpec{URL: "https://" + b.tlsAddr + "/git/kb"}})
	if err == nil {
		t.Fatal("a fleet create with a knomit instance as its origin succeeded")
	}
	if names := n.mgr.Names(); len(names) != 0 {
		t.Fatalf("repositories left: %v", names)
	}
	var markers int
	if err := n.mgr.ControlDB().QueryRow(`SELECT COUNT(*) FROM instance_fleet_create`).Scan(&markers); err != nil || markers != 0 {
		t.Fatalf("create markers left: %d (%v)", markers, err)
	}
	if _, err := os.Stat(filepath.Join(n.cfg.Home, pki.FleetRootDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("<home>/fleet left: %v", err)
	}
	if n := b.hdrs.tlsRequests(); n != 0 {
		t.Fatalf("%d requests reached the peer", n)
	}
}
