package cmd

import (
	"context"
	"net/http"

	"strings"
	"testing"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// The self-hosted master switch as the user ruled it (2026-10-16): the old
// master stands down FIRST, the new master syncs and hosts, and nobody asks
// the other side anything. In the order a user meets it: bravo (the new
// master) last synced BEFORE alpha merged charlie's pushed branch and revoked
// dave. alpha stands down: its last commit as master marks main, its root key
// goes, and its hub refuses charlie's next push. bravo's Host here is refused
// until its own sync brings the mark; then it hosts with all of alpha's
// writes. alpha and charlie point at bravo: alpha's sync replays nothing and
// is refused nothing; charlie's refused change reaches bravo; bravo's main
// holds the merged change and the revoke. Then the same protocol back to
// alpha (A -> C -> A): a master that stood down can be the master again.
// SABOTAGE: StandDown skips writeStoodDown -> bravo's host is refused
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
	if err != nil || ast.Master || ast.HostStoodDown != "alpha" {
		t.Fatalf("alpha after its stand-down: master %v host_stood_down %q (%v)", ast.Master, ast.HostStoodDown, err)
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

	// bravo has not synced since: its main lacks the mark.
	bravo.serve(t)
	if rec := bravo.localAPI(http.MethodPost, "/fleet/host", "{}"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "old_master_active") {
		t.Fatalf("bravo hosts before its sync brought the stand-down: %d %s", rec.Code, rec.Body.String())
	}
	bravo.round(t)
	if rec := bravo.localAPI(http.MethodPost, "/fleet/host", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("bravo hosts once its main shows the stand-down: %d %s", rec.Code, rec.Body.String())
	}
	bst, err := bravo.mgr.FleetStatus(ctx)
	if err != nil || !bst.Master || bst.Hosting != "none" || bst.MasterPaused != "" || bst.LastError != "" {
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
	if ast, _ := alpha.mgr.FleetStatus(ctx); !ast.Master || ast.MasterPaused != repos.FleetPausedNotHosting {
		t.Fatalf("alpha with the root, following bravo: master %v master_paused %q", ast.Master, ast.MasterPaused)
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
// main has no mark and Host here asks for the confirm that alpha's changes
// after bravo's last sync are not included; with it, bravo hosts. alpha comes
// back and points at bravo FIRST: its copy follows another instance now, so
// it does no master work although it still holds the root key (enroll is
// master_paused), its sync is refused nothing, and its stand-down writes no
// mark (it is not the host).
// SABOTAGE: hostFleetHere skips refuseOldMasterActive -> the first host is
// 200 -> red; fleetMasterPause ignores a cert origin -> alpha's enroll
// succeeds -> red.
func TestFleetMasterSwitch_DeadMasterFallback(t *testing.T) {
	ctx := context.Background()
	alpha, bravo := switchPair(t)

	bravo.serve(t)
	rec := bravo.localAPI(http.MethodPost, "/fleet/host", "{}")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "old_master_active") || !strings.Contains(rec.Body.String(), "leaves out every change") {
		t.Fatalf("bravo hosts with no stand-down on its main: %d %s", rec.Code, rec.Body.String())
	}
	if rec := bravo.localAPI(http.MethodPost, "/fleet/host", `{"confirm_loss":true}`); rec.Code != http.StatusOK {
		t.Fatalf("bravo hosts with the confirm: %d %s", rec.Code, rec.Body.String())
	}

	alpha.asClient(t)
	if rec := alpha.localAPI(http.MethodPost, "/fleet/follow", `{"url":"`+bravo.fleetURL()+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("alpha points at bravo: %d %s", rec.Code, rec.Body.String())
	}
	ast, err := alpha.mgr.FleetStatus(ctx)
	if err != nil || !ast.Master || ast.MasterPaused != repos.FleetPausedNotHosting {
		t.Fatalf("alpha following bravo: master %v master_paused %q (%v)", ast.Master, ast.MasterPaused, err)
	}
	if _, err := alpha.mgr.EnrollFleetMember(ctx, "zed", bravo.keyLine(), ""); fleetCodeOf(t, err) != "master_paused" {
		t.Fatalf("alpha's enroll while it follows bravo: %v", err)
	}
	alpha.round(t)
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

// The fallback's residue (PR #531 review S1/S4), driven through a real judge
// refusal: alpha comes back after bravo hosted with the confirm and stands
// down BEFORE pointing at bravo. As the hub it still acts as the master, so
// its stand-down writes the mark: a host-record change bravo's main lacks.
// Pointed at bravo, alpha's sync replays it and bravo's judge refuses it
// (host-record). The rule reads the commit against its own parents, so the
// refusal is permanent: GET /fleet says last_error_commit_refused.
// SABOTAGE: FleetStatus sets LastErrorCommitRefused false -> red.
func TestFleetMasterSwitch_FallbackThenStandDownIsRefusedForGood(t *testing.T) {
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
	for i := 0; i < 2; i++ {
		ast, err := alpha.mgr.FleetStatus(ctx)
		if err != nil || !ast.LastErrorCommitRefused || !strings.Contains(ast.LastError, "host-record") {
			t.Fatalf("round %d: alpha's mark at bravo: commit_refused %v last_error %q (%v)", i+1, ast.LastErrorCommitRefused, ast.LastError, err)
		}
		alpha.round(t)
	}
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
