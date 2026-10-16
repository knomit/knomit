package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
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

// fleetFreePort is a TCP port free on 127.0.0.1 a moment ago (every OS:
// serve_supervised_test.go's freePort is not built on Windows).
func fleetFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func newFleetNode(t *testing.T, dir, id string, mods ...func(*config.Config)) *fleetNode {
	t.Helper()
	cfg := config.Defaults()
	cfg.Home = filepath.Join(dir, id)
	cfg.OntologyRoot = "kb"
	// The mTLS listener's port is fixed up front so external_addresses can
	// name it: an enroll's bundle carries https://<that address>/git/fleet,
	// the URL a joiner clones.
	addr := "127.0.0.1:" + fleetFreePort(t)
	cfg.TLS = config.TLSConfig{Addr: addr, Dir: filepath.Join(cfg.Home, "pki")}
	cfg.ExternalAddresses = []string{"https://" + addr}
	for _, mod := range mods {
		mod(&cfg)
	}
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

// fleetURL is the plain https URL of n's fleet repository on its mTLS
// listener: what n's enroll puts in a bundle, and what a member stores.
func (n *fleetNode) fleetURL() string { return "https://" + n.tlsAddr + "/git/fleet" }

// asClient makes n the process's fleet transport client identity: the
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

// localAPI is api over the local authenticated listener, as the desktop app
// reaches it: the /fleet operations are served there only.
func (n *fleetNode) localAPI(method, p, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, p, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	peer := auth.Peer{ID: "uid:" + strconv.Itoa(os.Getuid()), Via: auth.LocalVia, PID: 7}
	// The operator's grant on its own socket, as `knomit grant` gives it.
	g := n.srv.Grants.(*auth.SQLGrants)
	if err := g.Grant(req.Context(), auth.Principal{Kind: auth.KindBridge, ID: peer.ID, Via: auth.LocalVia}, auth.Write, "test"); err != nil {
		panic(err)
	}
	req = req.WithContext(auth.WithPeer(req.Context(), peer))
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
	enr, err := m.mgr.EnrollFleetMember(context.Background(), n.id, n.keyLine(), "")
	if err != nil || !enr.Published {
		t.Fatalf("enroll %s: %v (%+v)", n.id, err, enr)
	}
	n.asClient(t)
	if enr.FleetURL != m.fleetURL() {
		t.Fatalf("enroll %s: the bundle names %q, want the hub's %q", n.id, enr.FleetURL, m.fleetURL())
	}
	st, err := n.mgr.JoinFleet(context.Background(), repos.FleetJoinSpec{Bundle: []byte(enr.Bundle)})
	if err != nil || st.State != repos.FleetRegistered {
		t.Fatalf("join %s: %v (state %q)", n.id, err, st.State)
	}
	// The join classified the bundle's https URL against the trust it had
	// just installed: the hub is a peer.
	requireCertOrigin(t, n.mgr, n.fleet(t), m.fleetURL())
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
	_, err = bravo.mgr.JoinFleet(ctx, repos.FleetJoinSpec{Bundle: pkitest.WithURL(t, early, alpha.fleetURL())})
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
	// The hub's receive-pack is the master's judge (PR #527 SF2): its refusal
	// is the master's, with the refused tip's author (SF3).
	refused := alpha.mgr.FleetRefusals()["agent/bravo"]
	if refused.Agent != "bravo" || refused.Tip == "" || !strings.Contains(refused.Reason, "no artifacts/fleet/certs/bravo.pem") {
		t.Fatalf("the hub's push refusal is not the master's: %+v", refused)
	}

	before := alpha.receives.Load()
	bravo.join(t, alpha)
	if _, ok := alpha.mgr.FleetRefusals()["agent/bravo"]; ok {
		t.Error("the admitted push did not clear bravo's refusal")
	}
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
	enr, err := alpha.mgr.EnrollFleetMember(ctx, "charlie", charlie.keyLine(), "")
	if err != nil {
		t.Fatal(err)
	}
	requireFleetActivity(t, alpha, enr.Serial)
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

// requireFleetActivity: GET /fleet/activity on the hub after the scenario of
// TestFleetMembership_KnomitHostedJoinRaceLeaveRevokeDecommission (create,
// bravo's refused join, enroll, join, leave, revoke, charlie's enroll with
// serial charlieSerial) shows each from its source, newest first: the
// creating commit, the host record, the issuance log (the race's local
// issuance included: it IS one), the merges into main and the records they
// brought, and the CRL with the revocation's reason.
func requireFleetActivity(t *testing.T, hub *fleetNode, charlieSerial string) {
	t.Helper()
	rec := hub.api(http.MethodGet, "/fleet/activity", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /fleet/activity: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Events []repos.FleetEvent `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for i, e := range out.Events {
		got[e.Kind+" "+e.Agent]++
		if i > 0 && e.At.After(out.Events[i-1].At) {
			t.Fatalf("event %d (%s) is newer than the one before it: not newest first", i, e.Kind)
		}
		switch {
		case e.Kind == repos.FleetEventAdmitted && (e.Branch != "agent/bravo" || e.Tip == ""):
			t.Errorf("admitted: %+v, want bravo's branch and tip", e)
		case e.Kind == repos.FleetEventRevoked && !strings.HasSuffix(e.Detail, ": left"):
			t.Errorf("revoked: %+v, want the statement's reason", e)
		case e.Kind == repos.FleetEventHostChanged && e.Detail != "hosted at "+hub.fleetURL():
			t.Errorf("host-changed: %+v", e)
		}
	}
	want := map[string]int{
		"created alpha": 1, "host-changed alpha": 1, "joined alpha": 1,
		"enrolled alpha": 1, "enrolled bravo": 2, "enrolled charlie": 1,
		"joined bravo": 1, "admitted bravo": 2, "left bravo": 1, "revoked bravo": 1,
		"refused bravo": 1, // the first join, refused by the hub's receive-pack
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("activity has %d %q, want %d (all: %v)", got[k], k, n, got)
		}
	}
	if len(out.Events) != 13 {
		t.Errorf("%d events, want 13: %v", len(out.Events), got)
	}
	found := false
	for _, e := range out.Events {
		found = found || e.Kind == repos.FleetEventEnrolled && e.Serial == charlieSerial
	}
	if !found {
		t.Errorf("no enrolled event with charlie's serial %s", charlieSerial)
	}
	if rec := hub.api(http.MethodGet, "/fleet/activity?limit=3", ""); !strings.Contains(rec.Body.String(), `"events"`) ||
		strings.Count(rec.Body.String(), `"kind"`) != 3 {
		t.Errorf("limit=3: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hub.api(http.MethodGet, "/fleet/activity?limit=0", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("limit=0: %d, want 400", rec.Code)
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
// the hub -> red; drop the master check of guardFleetDetach -> charlie's
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

	// A member keeps its origin (POST /fleet/host), and no generic route
	// changes the fleet repository (user ruling 2026-10-16).
	if rec := charlie.localAPI(http.MethodPost, "/fleet/host", "{}"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "member_keeps_origin") {
		t.Fatalf("a member's host-here: %d %s", rec.Code, rec.Body.String())
	}
	for _, rt := range [][3]string{
		{http.MethodDelete, "/repos/fleet/origin", ""},
		{http.MethodPatch, "/repos/fleet/origin/upstream", `{"branch":"trunk"}`},
		{http.MethodPut, "/repos/fleet/origin", `{"url":"` + alpha.fleetURL() + `"}`},
	} {
		if rec := charlie.localAPI(rt[0], rt[1], rt[2]); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "fleet_repository") {
			t.Fatalf("%s %s on the fleet repository: %d %s", rt[0], rt[1], rec.Code, rec.Body.String())
		}
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
	if rec := bravo.localAPI(http.MethodPost, "/fleet/host", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("the new master hosts the fleet: %d %s", rec.Code, rec.Body.String())
	}
	if bst, _ := bravo.mgr.FleetStatus(ctx); !bst.Master || bst.Hosting != "none" {
		t.Fatalf("bravo after the detach: %+v", bst)
	}
	// The new host records where the fleet is hosted on its main; the old
	// hub's main still says the old host.
	if got, want := readAt(t, bravo.fleet(t), "main", fact.FleetHostPath), "url: "+bravo.fleetURL()+"\n"; !strings.HasSuffix(got, want) {
		t.Fatalf("bravo's host record after the detach: %q, want it to end %q", got, want)
	}
	if got, want := readAt(t, alpha.fleet(t), "main", fact.FleetHostPath), "url: "+alpha.fleetURL()+"\n"; !strings.HasSuffix(got, want) {
		t.Fatalf("alpha's host record (written at create): %q, want it to end %q", got, want)
	}

	alpha.asClient(t)
	body := `{"url":"` + bravo.fleetURL() + `"}`
	if rec := alpha.localAPI(http.MethodPost, "/fleet/follow", body); rec.Code != http.StatusOK {
		t.Fatalf("the old master re-points at the new one: %d %s", rec.Code, rec.Body.String())
	}
	if err := alpha.mgr.StandDown(ctx); err != nil {
		t.Fatalf("the old master stands down once it has an origin: %v", err)
	}
	if ast, _ := alpha.mgr.FleetStatus(ctx); ast.Master || ast.Hosting != bravo.fleetURL() {
		t.Fatalf("alpha after the stand-down: %+v", ast)
	}

	// Charlie still follows the old host. Once the old host's main holds the
	// new master's host record (alpha's round fetched it from bravo),
	// charlie's next fetch from alpha brings it: GET /fleet then shows the
	// record (fleet_urls, host_record) apart from where charlie syncs
	// (hosting), which is how the page tells "this copy follows the old
	// host". Charlie's push there is refused (alpha has an origin now).
	alpha.round(t)
	charlie.asClient(t)
	_, _ = charlie.mgr.Send(ctx, charlie.fleet(t), repos.AttachOrigin(repos.OriginSpec{URL: alpha.fleetURL()}))
	cst, err := charlie.mgr.FleetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cst.Hosting != alpha.fleetURL() || len(cst.HostRecord) != 1 || cst.HostRecord[0] != bravo.fleetURL() ||
		len(cst.FleetURLs) != 1 || cst.FleetURLs[0] != bravo.fleetURL() {
		t.Fatalf("charlie on the old host: hosting %q host_record %v fleet_urls %v; want %q, [%s], [%s]",
			cst.Hosting, cst.HostRecord, cst.FleetURLs, alpha.fleetURL(), bravo.fleetURL(), bravo.fleetURL())
	}
	if cst.FollowsHost == nil || *cst.FollowsHost {
		t.Fatalf("charlie on the old host: follows_host %v, want false (the server's comparison)", cst.FollowsHost)
	}

	if rec := charlie.localAPI(http.MethodPost, "/fleet/follow", body); rec.Code != http.StatusOK {
		t.Fatalf("charlie re-points: %d %s", rec.Code, rec.Body.String())
	}
	if cst, err := charlie.mgr.FleetStatus(ctx); err != nil || cst.FollowsHost == nil || !*cst.FollowsHost {
		t.Fatalf("charlie re-pointed at the host: follows_host %v (%v), want true", cst.FollowsHost, err)
	}
	editOwnRecord(t, charlie, charlie.cfg.ExternalAddresses[0], "https://charlie.moved.example")
	before := bravo.receives.Load()
	charlie.round(t)
	if bravo.receives.Load() == before {
		t.Fatal("charlie's round pushed nothing to the new hub")
	}
	hubPublishes(t, bravo, recordPath("charlie"), "https://charlie.moved.example")

	editOwnRecord(t, alpha, alpha.cfg.ExternalAddresses[0], "https://alpha.moved.example")
	alpha.round(t)
	hubPublishes(t, bravo, recordPath("alpha"), "https://alpha.moved.example")
	if s := memberStates(t, bravo); s["alpha"] != fact.MemberActive || s["charlie"] != fact.MemberActive {
		t.Fatalf("the new hub's main: %v", s)
	}
	// Charlie's and alpha's PUT origin wrote no host record: their pushes
	// were admitted, and the hub refuses a member branch that changes it.
	// Both copies follow the new host's record through main.
	for _, n := range []*fleetNode{alpha, charlie} {
		if got, want := readAt(t, n.fleet(t), "main", fact.FleetHostPath), "url: "+bravo.fleetURL()+"\n"; !strings.HasSuffix(got, want) {
			t.Fatalf("%s's main after its round: host record %q, want it to end %q", n.id, got, want)
		}
	}
}

// N1 (PR #528 follow-up): the self-hosted master switch in the order a user
// meets it. The new master (bravo) hosts while its main LAGS the old hub's:
// alpha merged bravo's pushed branch, and wrote a record change of its own,
// after bravo's last fetch. Before that fetch alpha revoked charlie and
// re-enrolled the same key, so certs/charlie.pem on main is a REPLACED leaf.
// alpha then points at bravo and stands down. Its round finds bravo's main
// not a descendant of its own and replays its agent branch onto it. Before
// the fix the replay walked back to the watermark alpha's create seeded (a
// hub never advances it) and re-submitted alpha's whole hub history: the
// create commit's host record and charlie's first leaf, the local side
// winning, so bravo's judge refused the push (host-record; artifact-replace
// leaf-renewal) on every retry. Now only what bravo's main lacks is replayed:
// alpha's own record change arrives, and alpha's branch holds bravo's host
// record and charlie's newer leaf.
// SABOTAGE: unpushedCommits ignores the upstream's history -> the host record
// and the replaced leaf regress on alpha's branch and bravo refuses it -> red.
func TestFleetMasterSwitch_NewHostBehindTheOldHub(t *testing.T) {
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

	// A replaced leaf on main: revoke charlie, re-enroll the same key. The
	// re-issued leaf must start later than the first (leaf-renewal).
	charliePEM := path.Join("artifacts", "fleet", "certs", "charlie.pem")
	firstLeaf := readAt(t, alpha.fleet(t), "main", charliePEM)
	if _, err := alpha.mgr.RevokeFleetMember(ctx, "charlie", "re-enrolled"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := alpha.mgr.EnrollFleetMember(ctx, "charlie", charlie.keyLine(), ""); err != nil {
		t.Fatal(err)
	}
	newLeaf := readAt(t, alpha.fleet(t), "main", charliePEM)
	if firstLeaf == "" || newLeaf == firstLeaf {
		t.Fatalf("certs/charlie.pem was not replaced on alpha's main (%d bytes before)", len(firstLeaf))
	}

	// bravo's last fetch: its main is alpha's main here.
	bravo.round(t)
	if got := readAt(t, bravo.fleet(t), "main", charliePEM); got != newLeaf {
		t.Fatal("bravo's fetch did not bring the re-enrolled leaf")
	}

	// After it, alpha's main moves twice: alpha merges bravo's pushed branch
	// (bravo's round pushes; the merge lands after its fetch), and alpha
	// writes a change of its own record, a member's write that must reach the
	// new host.
	editOwnRecord(t, bravo, bravo.cfg.ExternalAddresses[0], "https://bravo.moved.example")
	bravo.round(t)
	hubPublishes(t, alpha, recordPath("bravo"), "https://bravo.moved.example")
	editOwnRecord(t, alpha, alpha.cfg.ExternalAddresses[0], "https://alpha.moved.example")
	hubPublishes(t, alpha, recordPath("alpha"), "https://alpha.moved.example")
	if got := readAt(t, bravo.fleet(t), "main", recordPath("alpha")); strings.Contains(got, "alpha.moved.example") {
		t.Fatal("bravo's main is not behind alpha's: the scenario needs it to lag")
	}

	// The switch, as the Settings page drives it.
	files, err := alpha.mgr.FleetRootFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bravo.mgr.InstallFleetRoot(files.Files); err != nil {
		t.Fatal(err)
	}
	bravo.serve(t)
	if rec := bravo.localAPI(http.MethodPost, "/fleet/host", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("the new master hosts the fleet: %d %s", rec.Code, rec.Body.String())
	}
	alpha.asClient(t)
	if rec := alpha.localAPI(http.MethodPost, "/fleet/follow", `{"url":"`+bravo.fleetURL()+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("the old master points at the new host: %d %s", rec.Code, rec.Body.String())
	}
	if err := alpha.mgr.StandDown(ctx); err != nil {
		t.Fatalf("the old master stands down: %v", err)
	}
	alpha.round(t)

	// Errorf, not Fatalf: each of the three names its own regression.
	if ast, err := alpha.mgr.FleetStatus(ctx); err != nil || ast.LastError != "" {
		t.Errorf("the old master's sync after the switch: %v %q", err, ast.LastError)
	}
	own := alpha.fleet(t).AgentBranch()
	if got, want := readAt(t, alpha.fleet(t), own, fact.FleetHostPath), "url: "+bravo.fleetURL()+"\n"; !strings.HasSuffix(got, want) {
		t.Errorf("alpha's branch after the replay: host record %q, want it to end %q (the new host's)", got, want)
	}
	if got := readAt(t, alpha.fleet(t), own, charliePEM); got != newLeaf {
		t.Error("alpha's branch after the replay holds charlie's FIRST leaf, not the re-enrolled one")
	}
	if t.Failed() {
		t.FailNow()
	}
	// Nothing alpha wrote as a member is lost: the new host admits and merges
	// alpha's record change.
	hubPublishes(t, bravo, recordPath("alpha"), "https://alpha.moved.example")
	if r, ok := bravo.mgr.FleetRefusals()[own]; ok {
		t.Fatalf("the new host refused alpha's branch: %+v", r)
	}
	// The next round stays clean (the refusal used to repeat on every one).
	alpha.round(t)
	if ast, _ := alpha.mgr.FleetStatus(ctx); ast.LastError != "" {
		t.Fatalf("the old master's second round: %q", ast.LastError)
	}
}
