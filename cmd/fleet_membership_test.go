package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web"
)

// fleetNode is one instance of a knomit-hosted fleet test: its own home, key
// and Manager, its API (called as a page on loopback would) and, once
// served, its /git over mTLS. receives counts the receive-pack requests its
// /git answered: every push a member makes to it.
type fleetNode struct {
	id       string
	cfg      config.Config
	key      string
	signer   ssh.Signer
	mgr      *repos.Manager
	srv      *web.Server
	tlsAddr  string
	receives atomic.Int32
}

func newFleetNode(t *testing.T, dir, id string) *fleetNode {
	t.Helper()
	cfg := config.Defaults()
	cfg.Home = filepath.Join(dir, id)
	cfg.OntologyRoot = "kb"
	cfg.TLS = config.TLSConfig{Addr: "127.0.0.1:0", Dir: filepath.Join(cfg.Home, "pki")}
	cfg.ExternalAddresses = []string{"https://" + id + ".fleet.example"}
	key, _ := pkitest.NewKey(t)
	n := &fleetNode{id: id, cfg: cfg, key: key, signer: keySigner(t, key)}
	n.mgr = repos.New(context.Background(), repos.Deps{Cfg: cfg, KeyPath: key, Signer: n.signer, AgentID: id,
		AgentBranch: "agent/" + id, Machine: repos.Options{Synchronous: true}})
	if err := n.mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.mgr.Close() })
	n.srv = &web.Server{Manager: n.mgr, GitHandler: web.GitRemoteHandler(n.mgr), APIOnly: true,
		Auth: cfg.Auth, Grants: auth.NewSQLGrants(n.mgr.ControlDB())}
	return n
}

// serve opens n's /git over mTLS with the certificate n holds installed.
func (n *fleetNode) serve(t *testing.T) {
	t.Helper()
	h := n.srv.Handler()
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			n.receives.Add(1)
		}
		h.ServeHTTP(w, r)
	})
	_, n.tlsAddr = serveFleetNode(t, counted, n.key, n.cfg.TLS)
}

func (n *fleetNode) fleetURL() string { return pki.GitScheme + "://" + n.tlsAddr + "/git/fleet" }

// asClient makes n the process's knomit+https client identity: the
// transport is one per process, so a test with several instances switches
// it before each one talks to another.
func (n *fleetNode) asClient(t *testing.T) {
	t.Helper()
	_ = pki.InstallGitTransport(n.cfg.TLS.Dir, n.key)
}

func (n *fleetNode) keyLine() string { return string(ssh.MarshalAuthorizedKey(n.signer.PublicKey())) }

func (n *fleetNode) fleet(t *testing.T) *repos.RepoInstance {
	t.Helper()
	ri := n.mgr.Get("fleet")
	if ri == nil {
		t.Fatalf("%s: no fleet repository mounted", n.id)
	}
	return ri
}

// api calls n's REST API as a page on this machine would (loopback TCP).
func (n *fleetNode) api(method, p, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, p, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr, req.Host = "127.0.0.1:1", "localhost"
	rec := httptest.NewRecorder()
	n.srv.NewAPIRouter().ServeHTTP(rec, req)
	return rec
}

// round runs one origin round of n's fleet copy (Synchronous: a re-attach of
// the same origin runs exactly one).
func (n *fleetNode) round(t *testing.T) {
	t.Helper()
	ri := n.fleet(t)
	var url string
	if err := ri.WithRead(func(s *store.Service) {
		if r, err := s.Remote().GetRemote("origin"); err == nil && r != nil {
			url = r.URL
		}
	}); err != nil || url == "" {
		t.Fatalf("%s: the fleet copy has no origin (%v)", n.id, err)
	}
	n.asClient(t)
	if _, err := n.mgr.Send(context.Background(), ri, repos.AttachOrigin(repos.OriginSpec{URL: url})); err != nil {
		t.Fatalf("%s: round: %v", n.id, err)
	}
}

// join enrolls n with master m's API-level enroll and joins m's fleet.
func (n *fleetNode) join(t *testing.T, m *fleetNode) {
	t.Helper()
	enr, err := m.mgr.EnrollFleetMember(context.Background(), n.id, n.keyLine())
	if err != nil || !enr.Published {
		t.Fatalf("enroll %s: %v (%+v)", n.id, err, enr)
	}
	n.asClient(t)
	st, err := n.mgr.JoinFleet(context.Background(), repos.FleetJoinSpec{URL: m.fleetURL(), Bundle: []byte(enr.Bundle)})
	if err != nil || st.State != repos.FleetRegistered {
		t.Fatalf("join %s: %v (state %q)", n.id, err, st.State)
	}
}

func recordPath(id string) string {
	sum := sha256.Sum256([]byte(id))
	return path.Join("kb", fact.MembersTopic, id, hex.EncodeToString(sum[:4])+".md")
}

// readAt is path's content at branch of ri ("" when absent).
func readAt(t *testing.T, ri *repos.RepoInstance, branch, p string) string {
	t.Helper()
	var out string
	if err := ri.WithRead(func(s *store.Service) {
		if f, err := s.Facts().ReadFact(context.Background(), branch, p, nil); err == nil {
			out = f.Content
		}
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// hubPublishes waits for the hub's merger to bring want into its agent
// branch, then moves main there, as the hub's local reconcile does.
func hubPublishes(t *testing.T, hub *fleetNode, p, want string) {
	t.Helper()
	ri := hub.fleet(t)
	own := "agent/" + hub.id
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(readAt(t, ri, own, p), want) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: the merger never brought %q at %s into %s", hub.id, want, p, own)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := ri.WithRead(func(s *store.Service) {
		if _, err := s.AdvanceLocalUpstream(context.Background(), own, s.UpstreamBranch()); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// editOwnRecord rewrites n's own record on its agent branch with field
// replaced, a member's one kind of write to the fleet repository.
func editOwnRecord(t *testing.T, n *fleetNode, from, to string) {
	t.Helper()
	ri := n.fleet(t)
	if err := ri.WithRead(func(s *store.Service) {
		cur, err := s.Facts().ReadFact(context.Background(), ri.AgentBranch(), recordPath(n.id), nil)
		if err != nil || !strings.Contains(cur.Content, from) {
			t.Fatalf("%s: own record (%v): %q has no %q", n.id, err, cur.Content, from)
		}
		if _, err := s.Facts().WriteFact(context.Background(), ri.AgentBranch(), recordPath(n.id),
			strings.Replace(cur.Content, from, to, 1), "fleet: "+n.id+" record updated", ""); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func fleetCodeOf(t *testing.T, err error) string {
	t.Helper()
	var fe *repos.FleetError
	if !errors.As(err, &fe) {
		t.Fatalf("want a FleetError, got %v", err)
	}
	return fe.Code
}

func memberStates(t *testing.T, n *fleetNode) map[string]string {
	t.Helper()
	ms, err := n.mgr.FleetMembers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range ms {
		out[m.Agent] = m.State
	}
	return out
}

// A knomit-hosted fleet end to end, two Managers in one process: alpha
// creates (it is the hub and the master), bravo's first join with a leaf that
// is not on main yet is refused by the hub's receive-pack and leaves nothing
// on bravo; the master enrolls bravo and the retry is admitted and merged;
// bravo leaves (one push, then nothing of the fleet is left on bravo, its lens
// kept); alpha revokes and decommissions.
// SABOTAGE: JoinFleet keeps going when its push fails (state registered) ->
// the first join is not refused -> red.
func TestFleetMembership_KnomitHostedJoinRaceLeaveRevokeDecommission(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	alpha := newFleetNode(t, dir, "alpha")
	st, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{})
	if err != nil || st.RecordState != fact.MemberActive || !st.Master || st.Hosting != "none" {
		t.Fatalf("create: %v %+v", err, st)
	}
	alpha.serve(t)
	bravo := newFleetNode(t, dir, "bravo")

	// The join race: a leaf the master issued but main does not hold yet.
	rootDir := filepath.Join(alpha.cfg.Home, pki.FleetRootDirName)
	root, err := pki.LoadRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	_, bpub, _ := pki.LoadSigner(bravo.key)
	leaf, _, err := pki.IssueInstance(rootDir, root, bpub, "bravo@"+pki.AgentEmailDomain, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	early := leaf
	for _, f := range []string{pki.RootCertFile, pki.CRLFile} {
		b, err := os.ReadFile(filepath.Join(rootDir, f))
		if err != nil {
			t.Fatal(err)
		}
		early = append(early, b...)
	}
	bravo.asClient(t)
	_, err = bravo.mgr.JoinFleet(ctx, repos.FleetJoinSpec{URL: alpha.fleetURL(), Bundle: early})
	if code := fleetCodeOf(t, err); code != "join_refused" {
		t.Fatalf("a join whose leaf is not on main: %s (%v), want join_refused", code, err)
	}
	if !strings.Contains(err.Error(), "no artifacts/fleet/certs/bravo.pem") || strings.Contains(err.Error(), "undoing the join failed") {
		t.Errorf("the hub's reason is passed on: %v", err)
	}
	if names := bravo.mgr.Names(); len(names) != 0 {
		t.Fatalf("a refused join left repositories: %v", names)
	}
	if _, err := os.Stat(filepath.Join(bravo.cfg.TLS.Dir, pki.InstanceCertFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused join left its certificate installed: %v", err)
	}
	if bst, _ := bravo.mgr.FleetStatus(ctx); bst.State != repos.FleetStandalone {
		t.Fatalf("a refused join left state %q", bst.State)
	}
	if _, ok := memberStates(t, alpha)["bravo"]; ok {
		t.Fatal("a refused join left a member record on main")
	}

	before := alpha.receives.Load()
	bravo.join(t, alpha)
	if got := alpha.receives.Load() - before; got != 2 {
		t.Errorf("the join reached the hub with %d pushes, want 2 (the mount's round, the record)", got)
	}
	hubPublishes(t, alpha, recordPath("bravo"), "state: active")
	if s := memberStates(t, alpha)["bravo"]; s != fact.MemberActive {
		t.Fatalf("bravo on the hub's main: %q", s)
	}
	bravo.round(t)
	if bst, _ := bravo.mgr.FleetStatus(ctx); bst.RecordState != fact.MemberActive || bst.Master || bst.Hosting != alpha.fleetURL() {
		t.Fatalf("bravo after the round: %+v", bst)
	}

	kb, err := bravo.mgr.Create(ctx, repos.CreateSpec{Name: "kb", Mode: "preset"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fleetUID := bravo.fleet(t).UID()
	if _, err := bravo.mgr.CreateLens(ctx, repos.Lens{Name: "with-fleet", WriteUID: kb.UID(), Reads: []repos.LensRead{{RepoUID: fleetUID}}}); err != nil {
		t.Fatal(err)
	}
	before = alpha.receives.Load()
	bravo.asClient(t)
	left, err := bravo.mgr.LeaveFleet(ctx)
	if err != nil || !left.Pushed {
		t.Fatalf("leave: %v %+v", err, left)
	}
	if got := alpha.receives.Load() - before; got != 1 {
		t.Errorf("the leave pushed %d times, want 1", got)
	}
	if bravo.mgr.Get("fleet") != nil {
		t.Fatal("the fleet repository is still mounted after the leave")
	}
	l, _, err := bravo.mgr.LensRegistry().Get("with-fleet")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range l.Reads {
		if r.RepoUID == fleetUID {
			t.Fatal("the lens still reads the fleet repository")
		}
	}
	hubPublishes(t, alpha, recordPath("bravo"), "state: left")

	rv, err := alpha.mgr.RevokeFleetMember(ctx, "bravo", "left")
	if err != nil || rv.InstallError != "" || rv.CRLNumber != "2" {
		t.Fatalf("revoke: %v %+v", err, rv)
	}
	if s := memberStates(t, alpha)["bravo"]; s != fact.MemberRevoked {
		t.Fatalf("bravo after the revoke: %q", s)
	}

	// A member still enrolled reaches the hub's running mTLS listener until
	// the decommission, and not after it: no restart (review B2).
	charlie := newFleetNode(t, dir, "charlie")
	enr, err := alpha.mgr.EnrollFleetMember(ctx, "charlie", charlie.keyLine())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pki.InstallBundle(charlie.cfg.TLS.Dir, charlie.key, []byte(enr.Bundle), pki.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	peer, err := pki.HTTPClient(charlie.cfg.TLS.Dir, charlie.key)
	if err != nil {
		t.Fatal(err)
	}
	hubGet := func() error {
		peer.CloseIdleConnections()
		resp, err := peer.Get("https://" + alpha.tlsAddr + "/api/v1/fleet")
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	if err := hubGet(); err != nil {
		t.Fatalf("an enrolled member is refused by the hub before the decommission: %v", err)
	}

	if _, err := alpha.mgr.DecommissionFleet(ctx); err != nil {
		t.Fatal(err)
	}
	if err := hubGet(); err == nil {
		t.Fatal("the decommissioned hub's running listener still admits a member")
	}
	if alpha.mgr.Get("fleet") != nil {
		t.Fatal("the fleet repository survived the decommission")
	}
	if _, err := os.Stat(rootDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the root survived the decommission: %v", err)
	}
}

// The master switch on a knomit-hosted fleet (R3.2), over the real routes:
// alpha is the hub and master, bravo and charlie members. The root files go
// from alpha (GET /fleet/root) to bravo (PUT); alpha's stand-down is refused
// while it is the hub; bravo detaches its origin (it hosts the fleet now);
// alpha re-points at bravo and stands down; charlie re-points; charlie's and
// alpha's next pushes reach bravo, which judges and merges them. A member's
// detach, a member's upstream PATCH and the old hub's manual merge of a dead
// pushed branch are refused.
// SABOTAGE: drop the hub check in StandDown -> alpha stands down while it is
// the hub -> red; drop the master check of GuardFleetDetach -> charlie's
// detach is 204 -> red.
func TestFleetMasterSwitch_KnomitHosted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	alpha := newFleetNode(t, dir, "alpha")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{}); err != nil {
		t.Fatal(err)
	}
	alpha.serve(t)
	bravo := newFleetNode(t, dir, "bravo")
	charlie := newFleetNode(t, dir, "charlie")
	bravo.join(t, alpha)
	hubPublishes(t, alpha, recordPath("bravo"), "state: active")
	charlie.join(t, alpha)
	hubPublishes(t, alpha, recordPath("charlie"), "state: active")
	bravo.round(t)
	charlie.round(t)

	// A member keeps its origin and its consensus branch.
	if rec := charlie.api(http.MethodDelete, "/repos/fleet/origin", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "member_keeps_origin") {
		t.Fatalf("a member's detach: %d %s", rec.Code, rec.Body.String())
	}
	if rec := charlie.api(http.MethodPatch, "/repos/fleet/origin/upstream", `{"branch":"trunk"}`); rec.Code != http.StatusConflict {
		t.Fatalf("a member's upstream PATCH: %d %s", rec.Code, rec.Body.String())
	}

	files, err := alpha.mgr.FleetRootFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bravo.mgr.InstallFleetRoot(files.Files); err != nil {
		t.Fatal(err)
	}
	if code := fleetCodeOf(t, alpha.mgr.StandDown(ctx)); code != "hub" {
		t.Fatalf("the hub's stand-down: %s, want hub", code)
	}

	bravo.serve(t)
	if rec := bravo.api(http.MethodDelete, "/repos/fleet/origin", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("the new master hosts the fleet: %d %s", rec.Code, rec.Body.String())
	}
	if bst, _ := bravo.mgr.FleetStatus(ctx); !bst.Master || bst.Hosting != "none" {
		t.Fatalf("bravo after the detach: %+v", bst)
	}

	alpha.asClient(t)
	body := `{"url":"` + bravo.fleetURL() + `","auth_method":"cert"}`
	if rec := alpha.api(http.MethodPut, "/repos/fleet/origin", body); rec.Code/100 != 2 {
		t.Fatalf("the old master re-points at the new one: %d %s", rec.Code, rec.Body.String())
	}
	if err := alpha.mgr.StandDown(ctx); err != nil {
		t.Fatalf("the old master stands down once it has an origin: %v", err)
	}
	if ast, _ := alpha.mgr.FleetStatus(ctx); ast.Master || ast.Hosting != bravo.fleetURL() {
		t.Fatalf("alpha after the stand-down: %+v", ast)
	}
	// The old hub still holds the pushed branches it received; it may not
	// merge them by hand: it is not the master.
	var tip string
	_ = alpha.fleet(t).WithRead(func(s *store.Service) { tip, _ = s.Branches().HeadCommit(ctx, "agent/charlie") })
	if tip == "" {
		t.Fatal("fixture: the old hub holds charlie's pushed branch")
	}
	if rec := alpha.api(http.MethodPost, "/repos/fleet/branches/agent:charlie/merge", `{"expected_tip":"`+tip+`"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_master") {
		t.Fatalf("the old hub's manual merge: %d %s", rec.Code, rec.Body.String())
	}

	charlie.asClient(t)
	if rec := charlie.api(http.MethodPut, "/repos/fleet/origin", body); rec.Code/100 != 2 {
		t.Fatalf("charlie re-points: %d %s", rec.Code, rec.Body.String())
	}
	editOwnRecord(t, charlie, "https://charlie.fleet.example", "https://charlie.moved.example")
	before := bravo.receives.Load()
	charlie.round(t)
	if bravo.receives.Load() == before {
		t.Fatal("charlie's round pushed nothing to the new hub")
	}
	hubPublishes(t, bravo, recordPath("charlie"), "https://charlie.moved.example")

	editOwnRecord(t, alpha, "https://alpha.fleet.example", "https://alpha.moved.example")
	alpha.round(t)
	hubPublishes(t, bravo, recordPath("alpha"), "https://alpha.moved.example")
	if s := memberStates(t, bravo); s["alpha"] != fact.MemberActive || s["charlie"] != fact.MemberActive {
		t.Fatalf("the new hub's main: %v", s)
	}
}
