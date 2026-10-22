package cmd

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
)

// The self-hosted master switch as the user ruled it (2026-10-16): the old
// master stands down FIRST, the new master syncs and hosts, and nobody asks
// the other side anything. In the order a user meets it: bravo (the new
// master) last synced BEFORE alpha merged charlie's pushed branch and revoked
// dave. alpha stands down: its last commit as master deletes the master marker
// (artifacts/fleet/master), its root key goes, and its hub refuses charlie's
// next push. bravo's Host here is refused while its own main still names
// alpha; after its sync it hosts (writing the marker) with all of alpha's
// writes. alpha and charlie point at bravo: alpha's sync replays nothing and
// is refused nothing; charlie's refused change reaches bravo; bravo's main
// holds the merged change and the revoke. Then the same protocol back to
// alpha (A -> C -> A): a master that stood down can be the master again.
// SABOTAGE: StandDown skips deleteFleetMaster -> bravo's host is refused
// old_master_active after its sync -> red; FleetPushRefusal admits a push to
// a hub whose master stood down -> charlie's push is admitted at alpha -> red.
func TestFleetMasterSwitch_StandDownFirst(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	alpha := newFleetNode(t, dir, "alpha")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{}); err != nil {
		t.Fatal(err)
	}
	alpha.serve(t)
	bravo := newFleetNode(t, dir, "bravo")
	charlie := newFleetNode(t, dir, "charlie")
	dave := newFleetNode(t, dir, "dave")
	for _, n := range []*fleetNode{bravo, charlie, dave} {
		n.join(t, alpha)
		hubPublishes(t, alpha, recordPath(n.id), "state: active")
	}
	files, err := alpha.mgr.FleetRootFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bravo.mgr.InstallFleetRoot(files.Files); err != nil {
		t.Fatal(err)
	}
	// bravo's last sync before alpha's writes below.
	bravo.round(t)

	// After it, alpha merges charlie's pushed change and revokes dave.
	editOwnRecord(t, charlie, charlie.cfg.ExternalAddresses[0], "https://charlie.one.example")
	charlie.round(t)
	hubPublishes(t, alpha, recordPath("charlie"), "https://charlie.one.example")
	if _, err := alpha.mgr.RevokeFleetMember(ctx, "dave", "gone"); err != nil {
		t.Fatal(err)
	}

	// alpha stands down first: nothing else is asked of it.
	if err := alpha.mgr.StandDown(ctx); err != nil {
		t.Fatalf("the hub stands down first: %v", err)
	}
	ast, err := alpha.mgr.FleetStatus(ctx)
	if err != nil || ast.Master || ast.ActingMaster != "" {
		t.Fatalf("alpha after its stand-down: master %v acting_master %q (%v)", ast.Master, ast.ActingMaster, err)
	}
	// Its hub takes no push now: charlie's next change waits.
	editOwnRecord(t, charlie, "https://charlie.one.example", "https://charlie.two.example")
	charlie.round(t)
	if cst, _ := charlie.mgr.FleetStatus(ctx); !cst.LastErrorStoodDown {
		t.Fatalf("charlie's push to the stood-down hub: last_error %q, want the stood-down refusal", cst.LastError)
	}
	if strings.Contains(readAt(t, alpha.fleet(t), "main", recordPath("charlie")), "charlie.two.example") {
		t.Fatal("the stood-down hub merged charlie's change")
	}

	// bravo has not synced since: its main still names alpha.
	bravo.serve(t)
	if rec := bravo.localAPI(http.MethodPost, "/fleet/host", "{}"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "old_master_active") {
		t.Fatalf("bravo hosts before its sync brought the stand-down: %d %s", rec.Code, rec.Body.String())
	}
	bravo.round(t)
	if rec := bravo.localAPI(http.MethodPost, "/fleet/host", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("bravo hosts once its main shows the stand-down: %d %s", rec.Code, rec.Body.String())
	}
	bst, err := bravo.mgr.FleetStatus(ctx)
	if err != nil || !bst.Master || bst.Hosting != "none" || bst.MasterPaused != "" || bst.LastError != "" || bst.ActingMaster != "bravo" {
		t.Fatalf("bravo after hosting: %+v (%v)", bst, err)
	}
	if !strings.Contains(readAt(t, bravo.fleet(t), "main", recordPath("charlie")), "charlie.one.example") {
		t.Fatal("bravo's main lacks the change alpha merged before it stood down")
	}
	if s := memberStates(t, bravo); s["dave"] != fact.MemberRevoked {
		t.Fatalf("bravo's main lacks alpha's revoke: %v", s)
	}

	// alpha and charlie point at the new host.
	follow := `{"url":"` + bravo.fleetURL() + `"}`
	alpha.asClient(t)
	if rec := alpha.localAPI(http.MethodPost, "/fleet/follow", follow); rec.Code != http.StatusOK {
		t.Fatalf("alpha points at bravo: %d %s", rec.Code, rec.Body.String())
	}
	alpha.round(t)
	if ast, _ := alpha.mgr.FleetStatus(ctx); ast.LastError != "" {
		t.Fatalf("alpha's sync at the new host: %q", ast.LastError)
	}
	charlie.asClient(t)
	if rec := charlie.localAPI(http.MethodPost, "/fleet/follow", follow); rec.Code != http.StatusOK {
		t.Fatalf("charlie points at bravo: %d %s", rec.Code, rec.Body.String())
	}
	charlie.round(t)
	hubPublishes(t, bravo, recordPath("charlie"), "https://charlie.two.example")
	for _, b := range []string{alpha.fleet(t).AgentBranch(), charlie.fleet(t).AgentBranch()} {
		if r, ok := bravo.mgr.FleetRefusals()[b]; ok {
			t.Fatalf("bravo refused %s: %+v", b, r)
		}
	}
	alpha.round(t)
	if ast, _ := alpha.mgr.FleetStatus(ctx); ast.LastError != "" {
		t.Fatalf("alpha's second sync: %q", ast.LastError)
	}

	// And back (user clarification 2026-10-16): standing down never bars
	// alpha from the role. The same protocol the other way: bravo's root to
	// alpha, bravo stands down first, alpha syncs and hosts, bravo and
	// charlie point at alpha.
	files, err = bravo.mgr.FleetRootFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alpha.mgr.InstallFleetRoot(files.Files); err != nil {
		t.Fatalf("alpha imports the root again: %v", err)
	}
	if ast, _ := alpha.mgr.FleetStatus(ctx); ast.Master || !ast.RootHeld || ast.MasterPaused != repos.FleetPausedNotHosting {
		t.Fatalf("alpha with the root, following bravo: master %v root_held %v master_paused %q", ast.Master, ast.RootHeld, ast.MasterPaused)
	}
	editOwnRecord(t, charlie, "https://charlie.two.example", "https://charlie.three.example")
	charlie.round(t)
	hubPublishes(t, bravo, recordPath("charlie"), "https://charlie.three.example")
	if err := bravo.mgr.StandDown(ctx); err != nil {
		t.Fatalf("bravo stands down: %v", err)
	}
	alpha.round(t)
	if rec := alpha.localAPI(http.MethodPost, "/fleet/host", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("alpha hosts again once its main shows bravo's stand-down: %d %s", rec.Code, rec.Body.String())
	}
	if ast, _ := alpha.mgr.FleetStatus(ctx); !ast.Master || ast.Hosting != "none" || ast.MasterPaused != "" {
		t.Fatalf("alpha after hosting again: %+v", ast)
	}
	if !strings.Contains(readAt(t, alpha.fleet(t), "main", recordPath("charlie")), "charlie.three.example") {
		t.Fatal("alpha's main lacks the change bravo merged before it stood down")
	}
	back := `{"url":"` + alpha.fleetURL() + `"}`
	for _, n := range []*fleetNode{bravo, charlie} {
		n.asClient(t)
		if rec := n.localAPI(http.MethodPost, "/fleet/follow", back); rec.Code != http.StatusOK {
			t.Fatalf("%s points back at alpha: %d %s", n.id, rec.Code, rec.Body.String())
		}
		n.round(t)
		if st, _ := n.mgr.FleetStatus(ctx); st.LastError != "" {
			t.Fatalf("%s's sync at alpha again: %q", n.id, st.LastError)
		}
	}
	editOwnRecord(t, charlie, "https://charlie.three.example", "https://charlie.four.example")
	charlie.round(t)
	hubPublishes(t, alpha, recordPath("charlie"), "https://charlie.four.example")
	if r, ok := alpha.mgr.FleetRefusals()[charlie.fleet(t).AgentBranch()]; ok {
		t.Fatalf("alpha refused charlie after the round trip: %+v", r)
	}
}

// The dead-master fallback: alpha never stood down (it is gone), so bravo's
// main still names alpha as the acting master and Host here asks for the
// confirm that alpha's changes after bravo's last sync are not included;
// with it, bravo hosts. Before alpha went, it wrote a change of its own
// record that bravo never fetched. alpha comes back and points at bravo
// FIRST: main names bravo now, so alpha, although it still holds the root
// key, is no master (user ruling 6: enroll is not_master), and its first sync
// at bravo is a rewind whose replay carries only alpha's record change (the
// stop rule: everything bravo's main holds is never replayed), which bravo
// admits and merges. Its stand-down then commits nothing.
// SABOTAGE: hostFleetHere skips refuseOldMasterActive -> the first host is
// 200 -> red; drop the replay's stop at commits upstream holds -> alpha's
// create-time host record is replayed and refused -> red (#531 re-review N6).
func TestFleetMasterSwitch_DeadMasterFallback(t *testing.T) {
	ctx := context.Background()
	alpha, bravo := switchPair(t)
	editOwnRecord(t, alpha, alpha.cfg.ExternalAddresses[0], "https://alpha.before-it-went.example")
	hubPublishes(t, alpha, recordPath("alpha"), "https://alpha.before-it-went.example")

	bravo.serve(t)
	rec := bravo.localAPI(http.MethodPost, "/fleet/host", "{}")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "old_master_active") || !strings.Contains(rec.Body.String(), "leaves out every change") {
		t.Fatalf("bravo hosts while its main names alpha: %d %s", rec.Code, rec.Body.String())
	}
	if rec := bravo.localAPI(http.MethodPost, "/fleet/host", `{"confirm_loss":true}`); rec.Code != http.StatusOK {
		t.Fatalf("bravo hosts with the confirm: %d %s", rec.Code, rec.Body.String())
	}

	alpha.asClient(t)
	if rec := alpha.localAPI(http.MethodPost, "/fleet/follow", `{"url":"`+bravo.fleetURL()+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("alpha points at bravo: %d %s", rec.Code, rec.Body.String())
	}
	alpha.round(t)
	ast, err := alpha.mgr.FleetStatus(ctx)
	if err != nil || ast.Master || !ast.RootHeld || ast.ActingMaster != "bravo" || ast.LastError != "" {
		t.Fatalf("alpha following bravo: master %v root_held %v acting %q last_error %q (%v)", ast.Master, ast.RootHeld, ast.ActingMaster, ast.LastError, err)
	}
	if _, err := alpha.mgr.EnrollFleetMember(ctx, "zed", bravo.keyLine(), ""); fleetCodeOf(t, err) != "not_master" {
		t.Fatalf("alpha's enroll while main names bravo: %v", err)
	}
	hubPublishes(t, bravo, recordPath("alpha"), "https://alpha.before-it-went.example")
	if err := alpha.mgr.StandDown(ctx); err != nil {
		t.Fatal(err)
	}
	alpha.round(t)
	if ast, _ := alpha.mgr.FleetStatus(ctx); ast.LastError != "" {
		t.Fatalf("alpha's sync at the new host: %q", ast.LastError)
	}
	if r, ok := bravo.mgr.FleetRefusals()[alpha.fleet(t).AgentBranch()]; ok {
		t.Fatalf("bravo refused alpha: %+v", r)
	}
}

// The fallback, with the returning master standing down BEFORE pointing at
// the new host: as the hub of its own main it is still the master there, so
// its stand-down commits the marker's deletion on its own main. That commit
// changes only the marker, which a replay never carries (user ruling 6: the
// demoted copy drops its own claim, and its stand-down), so pointed at
// bravo, alpha's sync is refused nothing. (Before ruling 6 this was a
// permanent host-record refusal.)
// SABOTAGE: do not set the replay skip path on the fleet repository -> the
// deletion is replayed and refused host-record -> red.
func TestFleetMasterSwitch_FallbackStandDownBeforePointingIsDropped(t *testing.T) {
	ctx := context.Background()
	alpha, bravo := switchPair(t)
	bravo.serve(t)
	if rec := bravo.localAPI(http.MethodPost, "/fleet/host", `{"confirm_loss":true}`); rec.Code != http.StatusOK {
		t.Fatalf("bravo hosts with the confirm: %d %s", rec.Code, rec.Body.String())
	}
	if err := alpha.mgr.StandDown(ctx); err != nil {
		t.Fatal(err)
	}
	alpha.asClient(t)
	if rec := alpha.localAPI(http.MethodPost, "/fleet/follow", `{"url":"`+bravo.fleetURL()+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("alpha points at bravo: %d %s", rec.Code, rec.Body.String())
	}
	alpha.round(t)
	alpha.round(t)
	if ast, _ := alpha.mgr.FleetStatus(ctx); ast.LastError != "" || ast.ActingMaster != "bravo" {
		t.Fatalf("alpha at bravo: last_error %q acting %q", ast.LastError, ast.ActingMaster)
	}
}

// The fallback's residue that remains (PR #531 review S1/S4), driven through a
// real judge refusal: alpha comes back after bravo hosted with the confirm,
// with a new external address, so as the hub of its own main it re-asserts
// the host record (master work it did as the only master it knew of). Pointed
// at bravo, alpha's sync replays that host-record change and bravo's judge
// refuses it (host-record), reading it against its own parents, so the
// refusal is permanent: GET /fleet says last_error_commit_refused, and the
// way out is leave, revoke, enroll, join.
// SABOTAGE: FleetStatus sets LastErrorCommitRefused false -> red.
func TestFleetMasterSwitch_FallbackHostRecordRewriteIsRefusedForGood(t *testing.T) {
	ctx := context.Background()
	alpha, bravo := switchPair(t)
	bravo.serve(t)
	if rec := bravo.localAPI(http.MethodPost, "/fleet/host", `{"confirm_loss":true}`); rec.Code != http.StatusOK {
		t.Fatalf("bravo hosts with the confirm: %d %s", rec.Code, rec.Body.String())
	}
	restartNode(t, alpha, func(c *config.Config) { c.ExternalAddresses = []string{"https://alpha.moved.example"} })
	if got := readAt(t, alpha.fleet(t), "main", fact.FleetHostPath); !strings.Contains(got, "alpha.moved.example") {
		t.Fatalf("setup: alpha's round did not re-assert its host record: %q", got)
	}
	alpha.asClient(t)
	if rec := alpha.localAPI(http.MethodPost, "/fleet/follow", `{"url":"`+bravo.fleetURL()+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("alpha points at bravo: %d %s", rec.Code, rec.Body.String())
	}
	alpha.round(t)
	for i := 0; i < 2; i++ {
		ast, err := alpha.mgr.FleetStatus(ctx)
		if err != nil || !ast.LastErrorCommitRefused || !strings.Contains(ast.LastError, "host-record") {
			t.Fatalf("round %d: alpha's host record at bravo: commit_refused %v last_error %q (%v)", i+1, ast.LastErrorCommitRefused, ast.LastError, err)
		}
		alpha.round(t)
	}
}

// A fleet created before the marker (#531 re-review S2): alpha never wrote
// one, and dies without standing down; bravo hosts (no marker on its main,
// so nothing is asked) and names itself. alpha comes back: as a root holder
// whose own main names no master it claims the role on its own main. Pointed
// at bravo it is demoted (main names bravo), its claim is a marker-only
// commit the replay drops, and its sync is clean even though it never stood
// down.
// SABOTAGE: do not set the replay skip path -> alpha's claim is replayed and
// refused host-record -> red.
func TestFleetMasterSwitch_LegacyReturnedMasterIsDemotedCleanly(t *testing.T) {
	ctx := context.Background()
	alpha, bravo := switchPair(t)
	// Make it a fleet from before the marker: alpha's main loses it, and
	// bravo's sync brings that main; alpha "dies" before any round of its own.
	ri := alpha.fleet(t)
	if err := ri.WithRead(func(s *store.Service) {
		if _, _, err := s.Facts().BatchWriteFacts(ctx, ri.AgentBranch(), nil, []string{fact.FleetMasterPath}, "a fleet created before the marker", "deleted"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Remote().AdvanceLocalUpstream(ctx, ri.AgentBranch(), s.Remote().UpstreamBranch()); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	bravo.round(t)
	if bst, _ := bravo.mgr.FleetStatus(ctx); bst.ActingMaster != "" {
		t.Fatalf("setup: bravo's main still names %q", bst.ActingMaster)
	}
	bravo.serve(t)
	if rec := bravo.localAPI(http.MethodPost, "/fleet/host", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("bravo hosts (no marker on its main): %d %s", rec.Code, rec.Body.String())
	}

	restartNode(t, alpha)
	if got := strings.TrimSpace(readAt(t, alpha.fleet(t), "main", fact.FleetMasterPath)); got != "alpha" {
		t.Fatalf("setup: the returning legacy root holder did not claim on its own main: %q", got)
	}
	alpha.asClient(t)
	if rec := alpha.localAPI(http.MethodPost, "/fleet/follow", `{"url":"`+bravo.fleetURL()+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("alpha points at bravo: %d %s", rec.Code, rec.Body.String())
	}
	alpha.round(t)
	alpha.round(t)
	ast, _ := alpha.mgr.FleetStatus(ctx)
	if ast.LastError != "" || ast.Master || ast.ActingMaster != "bravo" {
		t.Fatalf("alpha after pointing at bravo: last_error %q master %v acting %q", ast.LastError, ast.Master, ast.ActingMaster)
	}
	if r, ok := bravo.mgr.FleetRefusals()[alpha.fleet(t).AgentBranch()]; ok {
		t.Fatalf("bravo refused alpha: %+v", r)
	}
}

// restartNode closes n's manager and starts a new one on the same home and
// key, with mods applied to its config: the instance coming back online.
func restartNode(t *testing.T, n *fleetNode, mods ...func(*config.Config)) {
	t.Helper()
	n.mgr.Close()
	for _, mod := range mods {
		mod(&n.cfg)
	}
	n.mgr = repos.New(context.Background(), repos.Deps{Cfg: n.cfg, KeyPath: n.key, Signer: n.signer, AgentID: n.id,
		AgentBranch: "agent/" + n.id, Machine: repos.Options{Synchronous: true}})
	if err := n.mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.mgr.Close() })
	n.srv = &web.Server{Manager: n.mgr, GitHandler: web.GitRemoteHandler(n.mgr), APIOnly: true,
		Auth: n.cfg.Auth, Grants: auth.NewSQLGrants(n.mgr.ControlDB())}
}

// switchPair is a fleet alpha hosts, bravo a member that synced and imported
// alpha's root (the new master, not hosting yet).
func switchPair(t *testing.T) (alpha, bravo *fleetNode) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	alpha = newFleetNode(t, dir, "alpha")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{}); err != nil {
		t.Fatal(err)
	}
	alpha.serve(t)
	bravo = newFleetNode(t, dir, "bravo")
	bravo.join(t, alpha)
	hubPublishes(t, alpha, recordPath("bravo"), "state: active")
	bravo.round(t)
	files, err := alpha.mgr.FleetRootFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bravo.mgr.InstallFleetRoot(files.Files); err != nil {
		t.Fatal(err)
	}
	return alpha, bravo
}
