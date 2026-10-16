package cmd

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// TestFleetHubMovesToAForge is the hub → forge move end to end (user ruling
// 2026-10-16): alpha hosts the fleet and bravo is its member; bravo writes a
// change alpha has not merged yet. Alpha moves the fleet onto an empty forge
// (POST /fleet/follow with the forge URL): the forge's main is alpha's, its
// host record is the forge URL, and a new bundle names the forge. Bravo, still
// on the old hub, sees follows_host false once its fetch brings main, and its
// push there is refused (the old hub has an origin now). Bravo points at the
// forge with its own credential (POST /fleet/follow): its branch, the unmerged
// change included, reaches the forge on that first sync, and alpha, now the
// fetching master, judges and merges it and publishes main.
// SABOTAGE: drop the master branch of followForge (attach as a member would)
// -> no host record and the forge's main is not alpha's -> red; drop
// `if master { peers.admit(...) }` in runReconcileLoop -> bravo's change never
// reaches the forge's main -> red.
func TestFleetHubMovesToAForge(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	forgeDir := filepath.Join(dir, "forge")
	if err := os.MkdirAll(forgeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(forgeDir, "fleet.git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	forge := fileuri.New(bare)
	allow := func(c *config.Config) { c.LocalOriginRoot = forgeDir }

	alpha := newFleetNode(t, dir, "alpha", allow)
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{}); err != nil {
		t.Fatal(err)
	}
	alpha.serve(t)
	bravo := newFleetNode(t, dir, "bravo", allow)
	bravo.join(t, alpha)
	hubPublishes(t, alpha, recordPath("bravo"), "state: active")
	bravo.round(t)
	// Bravo's change that the hub has not merged: it rides bravo's branch.
	editOwnRecord(t, bravo, bravo.cfg.ExternalAddresses[0], "https://bravo.moved.example")

	// The move.
	alpha.asClient(t)
	if rec := alpha.localAPI(http.MethodPost, "/fleet/follow", `{"url":"`+forge+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("the hub moves the fleet onto the forge: %d %s", rec.Code, rec.Body.String())
	}
	ast, err := alpha.mgr.FleetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ast.Master || ast.Hosting != forge || len(ast.HostRecord) != 1 || ast.HostRecord[0] != forge || ast.FollowsHost == nil || !*ast.FollowsHost {
		t.Fatalf("alpha after the move: master %v hosting %q host_record %v follows %v", ast.Master, ast.Hosting, ast.HostRecord, ast.FollowsHost)
	}
	if got, want := bareRev(t, bare, "main"), localRev(t, alpha.fleet(t), "main"); got != want {
		t.Fatalf("the forge's main %s, want alpha's %s", got, want)
	}
	if got := bareShow(t, bare, "main", fact.FleetHostPath); !strings.Contains(got, "url: "+forge) {
		t.Fatalf("the host record on the forge's main: %q", got)
	}
	enr, err := alpha.mgr.EnrollFleetMember(ctx, "charlie", newFleetNode(t, dir, "charlie").keyLine(), "")
	if err != nil || enr.FleetURL != forge {
		t.Fatalf("a bundle after the move names %q (%v), want the forge %q", enr.FleetURL, err, forge)
	}

	// Bravo on the old hub: its fetch brings main with the forge's host
	// record; its push is refused there.
	bravo.round(t)
	if err := bravo.fleet(t).WithRead(func(s *store.Service) {
		r, err := s.Remote().GetRemote("origin")
		if err != nil || r == nil || r.LastPushStatus == nil || *r.LastPushStatus != "error" {
			t.Fatalf("bravo's push to the old hub: %+v (%v), want refused", r, err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if bareRev(t, bare, "agent/bravo") != "" {
		t.Fatal("fixture: bravo's branch is on the forge before bravo follows it")
	}
	bst, err := bravo.mgr.FleetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bst.FollowsHost == nil || *bst.FollowsHost || len(bst.HostRecord) != 1 || bst.HostRecord[0] != forge {
		t.Fatalf("bravo on the old hub: follows %v host_record %v, want false and [%s]", bst.FollowsHost, bst.HostRecord, forge)
	}

	// Bravo follows the forge with its own credential (none, for file://).
	bravo.asClient(t)
	if rec := bravo.localAPI(http.MethodPost, "/fleet/follow", `{"url":"`+forge+`","auth_method":"none"}`); rec.Code != http.StatusOK {
		t.Fatalf("bravo points at the forge: %d %s", rec.Code, rec.Body.String())
	}
	bst, err = bravo.mgr.FleetStatus(ctx)
	if err != nil || bst.Hosting != forge || bst.FollowsHost == nil || !*bst.FollowsHost {
		t.Fatalf("bravo on the forge: hosting %q follows %v (%v)", bst.Hosting, bst.FollowsHost, err)
	}
	if got, want := bareRev(t, bare, "agent/bravo"), localRev(t, bravo.fleet(t), "agent/bravo"); got == "" || got != want {
		t.Fatalf("bravo's branch on the forge %q, want its own %q: its first sync pushes it", got, want)
	}

	// Alpha, the fetching master now, merges bravo's change and publishes.
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(bareShow(t, bare, "main", recordPath("bravo")), "https://bravo.moved.example") {
		if time.Now().After(deadline) {
			t.Fatalf("bravo's unmerged change never reached the forge's main: %q", bareShow(t, bare, "main", recordPath("bravo")))
		}
		alpha.round(t)
		time.Sleep(50 * time.Millisecond)
	}
}

func bareRev(t *testing.T, bare, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "rev-parse", "--verify", "-q", "refs/heads/"+ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func bareShow(t *testing.T, bare, rev, p string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "show", rev+":"+p).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func localRev(t *testing.T, ri *repos.RepoInstance, branch string) string {
	t.Helper()
	var tip string
	if err := ri.WithRead(func(s *store.Service) {
		tip, _ = s.Branches().HeadCommit(context.Background(), branch)
	}); err != nil {
		t.Fatal(err)
	}
	return tip
}
