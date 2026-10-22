package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
)

// PR #537 re-review (the reviewer's probe of B2 on the hub, kept as a regression test): bravo's `left` record
// is merged into the hub's agent branch, main still says active, and the
// hub's round runs with bravo's leaf due. The renewal must not re-issue it,
// and the leave duty must then revoke the leaf the record names.
func TestFleetRenew_HubLeaveOnTheAgentBranchIsNotRenewed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	alpha := newFleetNode(t, dir, "alpha")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{}); err != nil {
		t.Fatal(err)
	}
	alpha.serve(t)
	bravo := newFleetNode(t, dir, "bravo")
	bravo.join(t, alpha)
	hubPublishes(t, alpha, recordPath("bravo"), "state: active")
	bravo.round(t)
	oldB := mainLeaf(t, alpha, "bravo")

	bravo.asClient(t)
	if left, err := bravo.mgr.LeaveFleet(ctx); err != nil || !left.Pushed {
		t.Fatalf("bravo leaves: %v %+v", err, left)
	}
	ri := alpha.fleet(t)
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(readAt(t, ri, "agent/alpha", recordPath("bravo")), "state: left") {
		if time.Now().After(deadline) {
			t.Fatal("the merger never brought bravo's left record")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var onMain string
	_ = ri.WithRead(func(s *store.Service) {
		onMain = readAt(t, ri, s.Remote().UpstreamBranch(), recordPath("bravo"))
	})
	if !strings.Contains(onMain, "state: active") {
		t.Fatalf("fixture: main should still say active: %q", onMain)
	}

	restore := repos.SetFleetClockForTest(renewalDue(oldB))
	hubRound(t, alpha)
	restore()
	for i := 0; i < 3; i++ {
		hubRound(t, alpha)
	}
	got := mainLeaf(t, alpha, "bravo")
	state := memberStates(t, alpha)["bravo"]
	t.Logf("bravo: old serial %s, main's %s, state %q", oldB.SerialNumber.Text(16), got.SerialNumber.Text(16), state)
	if got.SerialNumber.Cmp(oldB.SerialNumber) != 0 || state != fact.MemberRevoked {
		t.Fatalf("the leaver's leaf was renewed or never revoked: serial %s, state %q", got.SerialNumber.Text(16), state)
	}
}
