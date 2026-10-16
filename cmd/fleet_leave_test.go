package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/platform/fileuri"
	"knomit/internal/repos"
)

// hubRound runs one round of the hub's local reconcile, as its loop does
// every interval: a detach of its (absent) origin restarts the Sync stage,
// which, Synchronous, runs one local round inline (the hub's duties, then
// main follows its agent branch).
func hubRound(t *testing.T, hub *fleetNode) {
	t.Helper()
	ri := hub.fleet(t)
	if _, err := hub.mgr.Send(context.Background(), ri, repos.DetachOrigin()); err != nil {
		t.Fatalf("%s: hub round: %v", hub.id, err)
	}
}

// recordStateOverHTTP is GET /fleet's record_state on n, as the page reads it.
func recordStateOverHTTP(t *testing.T, n *fleetNode) string {
	t.Helper()
	rec := n.api(http.MethodGet, "/fleet", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: GET /fleet: %d %s", n.id, rec.Code, rec.Body.String())
	}
	var st struct {
		RecordState string `json:"record_state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st.RecordState
}

// installedCRLNumber is the Number of the CRL installed in n's [tls].dir.
func installedCRLNumber(t *testing.T, n *fleetNode) int64 {
	t.Helper()
	crl, err := pki.LoadCRL(filepath.Join(n.cfg.TLS.Dir, pki.CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	return crl.Number.Int64()
}

// joinAnswer enrolls n on master m and joins with the bundle (auth for a
// forge), returning the join's answer.
func joinAnswer(t *testing.T, n, m *fleetNode, authMethod string) repos.FleetStatus {
	t.Helper()
	enr, err := m.mgr.EnrollFleetMember(context.Background(), n.id, n.keyLine(), "")
	if err != nil || !enr.Published {
		t.Fatalf("enroll %s: %v (%+v)", n.id, err, enr)
	}
	n.asClient(t)
	st, err := n.mgr.JoinFleet(context.Background(), repos.FleetJoinSpec{Bundle: []byte(enr.Bundle), AuthMethod: authMethod})
	if err != nil {
		t.Fatalf("join %s: %v", n.id, err)
	}
	return st
}

// Leave means revoke on a knomit-hosted fleet, end to end (user ruling
// 2026-10-16): bravo joins (pending, then accepted once the hub's main holds
// its record), leaves, and the hub's next round revokes its leaf with no
// call (the CRL on main, installed on the hub); another round revokes
// nothing more. The master enrolls bravo again with no manual revoke, bravo
// rejoins (pending: main still says left), and once the hub's main holds
// the active record bravo is accepted and its fresh leaf is NOT revoked.
// SABOTAGE: drop the leave duty from fleetHostRound -> bravo is never
// revoked and the re-enroll is refused `enrolled` -> red.
func TestFleetLeave_KnomitHostedLeaveRevokesAndTheRejoinIsAccepted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	alpha := newFleetNode(t, dir, "alpha")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{}); err != nil {
		t.Fatal(err)
	}
	alpha.serve(t)
	bravo := newFleetNode(t, dir, "bravo")

	if st := joinAnswer(t, bravo, alpha, ""); st.State != repos.FleetRegistered || st.RecordState != repos.RecordPending {
		t.Fatalf("the join answers state %q record_state %q, want registered and pending", st.State, st.RecordState)
	}
	if got := recordStateOverHTTP(t, bravo); got != repos.RecordPending {
		t.Fatalf("GET /fleet after the join: record_state %q, want pending", got)
	}
	hubPublishes(t, alpha, recordPath("bravo"), "state: active")
	bravo.round(t)
	if got := recordStateOverHTTP(t, bravo); got != fact.MemberActive {
		t.Fatalf("GET /fleet once main holds the record: %q, want active", got)
	}

	bravo.asClient(t)
	if left, err := bravo.mgr.LeaveFleet(ctx); err != nil || !left.Pushed {
		t.Fatalf("leave: %v %+v", err, left)
	}
	hubPublishes(t, alpha, recordPath("bravo"), "state: left")
	if s := memberStates(t, alpha)["bravo"]; s != fact.MemberLeft {
		t.Fatalf("fixture: bravo on the hub's main before its round: %q, want left", s)
	}
	hubRound(t, alpha)
	if s := memberStates(t, alpha)["bravo"]; s != fact.MemberRevoked {
		t.Fatalf("bravo after the hub's round: %q, want revoked (leave means revoke)", s)
	}
	if n := installedCRLNumber(t, alpha); n != 2 {
		t.Fatalf("the hub's installed CRL is #%d, want #2", n)
	}
	tip := localRev(t, alpha.fleet(t), "main")
	hubRound(t, alpha)
	if got := localRev(t, alpha.fleet(t), "main"); got != tip {
		t.Fatalf("a second round moved main (%s -> %s): the revoke is not once", tip, got)
	}

	// The re-enroll needs no manual revoke. A hub round between it and the
	// rejoin sees main with the left record AND the fresh leaf, and must not
	// revoke the fresh leaf: it landed after the left record.
	enr, err := alpha.mgr.EnrollFleetMember(ctx, "bravo", bravo.keyLine(), "")
	if err != nil || !enr.Published {
		t.Fatalf("re-enroll bravo after its leave: %v (%+v)", err, enr)
	}
	hubRound(t, alpha)
	if s := memberStates(t, alpha)["bravo"]; s != fact.MemberLeft {
		t.Fatalf("bravo between the re-enroll and the rejoin: %q, want left (the fresh leaf is not revoked)", s)
	}
	bravo.asClient(t)
	if st, err := bravo.mgr.JoinFleet(ctx, repos.FleetJoinSpec{Bundle: []byte(enr.Bundle)}); err != nil || st.RecordState != repos.RecordPending {
		t.Fatalf("the rejoin: %v, record_state %q, want pending (main still says left)", err, st.RecordState)
	}
	hubPublishes(t, alpha, recordPath("bravo"), "state: active")
	hubRound(t, alpha)
	if s := memberStates(t, alpha)["bravo"]; s != fact.MemberActive {
		t.Fatalf("bravo after the rejoin and the hub's round: %q, want active (its fresh leaf is not revoked)", s)
	}
	if n := installedCRLNumber(t, alpha); n != 2 {
		t.Fatalf("the hub's installed CRL is #%d after the rejoin, want still #2", n)
	}
	bravo.round(t)
	if got := recordStateOverHTTP(t, bravo); got != fact.MemberActive {
		t.Fatalf("GET /fleet after the rejoin is merged: %q, want active", got)
	}
}

// The same on a forge-hosted fleet, where nothing judges a join at push
// time: the join answers pending and GET /fleet says pending until the
// master's round has merged the record into the forge's main and bravo has
// fetched it; then active. Bravo leaves; the master's rounds revoke it on the
// forge's main with no call; it rejoins (pending, no manual revoke) and is
// accepted, its fresh leaf not revoked.
// SABOTAGE: drop `peers.runDuty` from the origin round -> bravo is never
// revoked -> red; answer main's state whenever main holds a record
// (recordStateOf) -> the rejoin answers left -> red.
func TestFleetLeave_ForgeJoinIsPendingThenAcceptedAndLeaveRevokes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	forgeDir := filepath.Join(dir, "forge")
	if err := os.MkdirAll(forgeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(forgeDir, "fleet.git")
	// A forge create initializes onto a main that has one commit.
	work := filepath.Join(dir, "seed")
	for _, args := range [][]string{
		{"init", "--bare", "--initial-branch=main", bare},
		{"init", "--initial-branch=main", work},
		{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "seed"},
		{"-C", work, "push", "-q", bare, "main"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	forge := fileuri.New(bare)
	allow := func(c *config.Config) { c.LocalOriginRoot = forgeDir }
	alpha := newFleetNode(t, dir, "alpha", allow)
	if st, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{Origin: &repos.OriginSpec{URL: forge, AuthMethod: "none"}}); err != nil || st.Hosting != forge {
		t.Fatalf("forge create: %v %+v", err, st)
	}
	bravo := newFleetNode(t, dir, "bravo", allow)

	// until runs the master's round until cond holds on the forge.
	until := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("the master's rounds never brought %s", what)
			}
			alpha.round(t)
			time.Sleep(50 * time.Millisecond)
		}
	}

	if st := joinAnswer(t, bravo, alpha, "none"); st.State != repos.FleetRegistered || st.RecordState != repos.RecordPending {
		t.Fatalf("the forge join answers state %q record_state %q, want registered and pending", st.State, st.RecordState)
	}
	if bareShow(t, bare, "main", recordPath("bravo")) != "" {
		t.Fatal("fixture: the record is on the forge's main before the master's round")
	}
	if got := recordStateOverHTTP(t, bravo); got != repos.RecordPending {
		t.Fatalf("GET /fleet after the forge join: %q, want pending", got)
	}
	until("bravo's record onto main", func() bool {
		return strings.Contains(bareShow(t, bare, "main", recordPath("bravo")), "state: active")
	})
	if got := recordStateOverHTTP(t, bravo); got != repos.RecordPending {
		t.Fatalf("GET /fleet before bravo fetched the new main: %q, want pending", got)
	}
	bravo.round(t)
	if got := recordStateOverHTTP(t, bravo); got != fact.MemberActive {
		t.Fatalf("GET /fleet once bravo's main holds its record: %q, want active", got)
	}

	bravo.asClient(t)
	if left, err := bravo.mgr.LeaveFleet(ctx); err != nil || !left.Pushed {
		t.Fatalf("leave: %v %+v", err, left)
	}
	until("bravo's revocation onto main", func() bool { return memberStates(t, alpha)["bravo"] == fact.MemberRevoked })
	if !strings.Contains(bareShow(t, bare, "main", recordPath("bravo")), "state: left") {
		t.Fatal("bravo is revoked without its left record on the forge's main")
	}
	if n := installedCRLNumber(t, alpha); n != 2 {
		t.Fatalf("the master's installed CRL is #%d, want #2", n)
	}

	if st := joinAnswer(t, bravo, alpha, "none"); st.RecordState != repos.RecordPending {
		t.Fatalf("the rejoin answers record_state %q, want pending", st.RecordState)
	}
	until("bravo's rejoin onto main", func() bool { return memberStates(t, alpha)["bravo"] == fact.MemberActive })
	alpha.round(t)
	if s := memberStates(t, alpha)["bravo"]; s != fact.MemberActive {
		t.Fatalf("bravo after one more master round: %q, want active (its fresh leaf is not revoked)", s)
	}
	if n := installedCRLNumber(t, alpha); n != 2 {
		t.Fatalf("the master's installed CRL is #%d after the rejoin, want still #2", n)
	}
	bravo.round(t)
	if got := recordStateOverHTTP(t, bravo); got != fact.MemberActive {
		t.Fatalf("GET /fleet after the rejoin is merged: %q, want active", got)
	}
}
