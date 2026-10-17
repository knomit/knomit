package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/repos"
)

// peerGet makes one fresh mTLS request from peer's installed identity to
// n's running listener: nil when n let it in.
func peerGet(t *testing.T, peer, n *fleetNode) error {
	t.Helper()
	c, err := pki.HTTPClient(peer.cfg.TLS.Dir, peer.key)
	if err != nil {
		t.Fatal(err)
	}
	c.Transport.(*http.Transport).DisableKeepAlives = true
	resp, err := c.Get("https://" + n.tlsAddr + "/api/v1/fleet")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// loopRound runs one origin round of n's fleet copy as its sync loop does,
// without re-attaching the origin (round's attach probes it first, and a
// refused probe never reaches the round).
func loopRound(t *testing.T, n *fleetNode) {
	t.Helper()
	n.asClient(t)
	if err := n.fleet(t).OriginRoundForTest(context.Background()); err != nil {
		t.Fatalf("%s: round: %v", n.id, err)
	}
}

// fleetJSON is GET /fleet on n as the page reads it, decoded into a map.
func fleetJSON(t *testing.T, n *fleetNode) map[string]any {
	t.Helper()
	rec := n.api(http.MethodGet, "/fleet", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: GET /fleet: %d %s", n.id, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The S2 classification on a knomit-hosted fleet, end to end over real mTLS
// listeners, where a revoked member can NOT read the CRL that revokes it:
//   - alpha revokes bravo; bravo's next round is refused at the hub's
//     handshake (bad_certificate) while bravo's own certificate is valid by
//     its own material: GET /fleet says last_error_refusal revoked, revoked,
//     sync_stopped, while record_state still says active (bravo's main is
//     stale, the hub refuses its fetch). The next round attempts nothing
//     (last_attempt unchanged).
//   - charlie, still a member, sees the hub decommissioned and a NEW fleet
//     (another root) created at the same address: its round refuses the
//     hub's certificate itself: last_error_refusal moved, not revoked, and
//     its rounds keep running.
//
// SABOTAGE: drop peers.refusedFetch from the round -> the refused fetch is
// not recorded and the no-op push clears last_error -> red; drop
// noteFleetRevoked from fleetFetchRefused -> bravo's second round attempts
// again (last_attempt moves) -> red; drop the moved arm of
// classifyFleetRefusal -> charlie's refusal reads "" -> red.
func TestFleetRefusal_KnomitHostedRevokedAndMoved(t *testing.T) {
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
	if got := fleetJSON(t, bravo); got["record_state"] != "active" || got["revoked"] != nil || got["last_error"] != nil {
		t.Fatalf("fixture: bravo before the revoke: %v", got)
	}

	if _, err := alpha.mgr.RevokeFleetMember(ctx, "bravo", "test"); err != nil {
		t.Fatal(err)
	}
	loopRound(t, bravo)
	got := fleetJSON(t, bravo)
	lastErr, _ := got["last_error"].(string)
	if !strings.Contains(lastErr, pki.ErrRefusedByPeer.Error()) || !strings.Contains(lastErr, "bad certificate") {
		t.Fatalf("bravo's refused round recorded %q", lastErr)
	}
	if got["last_error_refused"] != true || got["last_error_refusal"] != "revoked" || got["revoked"] != true || got["sync_stopped"] != true {
		t.Fatalf("bravo after the refused round: %v", got)
	}
	if got["record_state"] != "active" {
		t.Fatalf("fixture: bravo cannot read the CRL, its main still says %v", got["record_state"])
	}
	attempted := got["last_attempt"]
	time.Sleep(1100 * time.Millisecond) // last_attempt has whole seconds: a second attempt would move it
	loopRound(t, bravo)
	if again := fleetJSON(t, bravo); again["last_attempt"] != attempted || again["sync_stopped"] != true {
		t.Fatalf("a stopped round attempted a push: last_attempt %v -> %v", attempted, again["last_attempt"])
	}

	// The hub's fleet ends and another fleet (another root) answers there.
	if _, err := alpha.mgr.DecommissionFleet(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{}); err != nil {
		t.Fatal(err)
	}
	loopRound(t, charlie)
	got = fleetJSON(t, charlie)
	lastErr, _ = got["last_error"].(string)
	if !strings.Contains(lastErr, pki.ErrUntrustedRoot.Error()) {
		t.Fatalf("charlie's round against the other root recorded %q", lastErr)
	}
	if got["last_error_refusal"] != "moved" || got["revoked"] != nil || got["sync_stopped"] != nil {
		t.Fatalf("charlie after the other root: %v", got)
	}
	before := got["last_attempt"]
	time.Sleep(1100 * time.Millisecond) // last_attempt has whole seconds
	loopRound(t, charlie)
	if again := fleetJSON(t, charlie); again["last_attempt"] == before {
		t.Fatal("a moved fleet stopped charlie's rounds; only a revoked certificate stops them")
	}
}

// The P6 duty on a knomit-hosted fleet, end to end over real mTLS listeners:
// bravo and charlie are members of alpha's hub, and bravo serves its own
// listener, which charlie reaches. Alpha revokes charlie: the hub refuses
// charlie at once (the revoke installs the CRL there), but bravo still lets
// charlie in until bravo's own round, which installs main's CRL #2; from then
// on bravo refuses charlie too, with no restart and no reload.
// SABOTAGE: drop peers.runTick from the origin round -> bravo keeps CRL #1
// and still lets charlie in -> red.
func TestFleetTick_KnomitHostedMemberRefusesTheRevokedPeerAfterItsRound(t *testing.T) {
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
	bravo.serve(t)
	if err := peerGet(t, charlie, bravo); err != nil {
		t.Fatalf("fixture: charlie reaches bravo's listener before the revoke: %v", err)
	}

	rv, err := alpha.mgr.RevokeFleetMember(ctx, "charlie", "test")
	if err != nil || !rv.Published || rv.CRLNumber != "2" {
		t.Fatalf("revoke: %v %+v", err, rv)
	}
	if s := memberStates(t, alpha)["charlie"]; s != fact.MemberRevoked {
		t.Fatalf("charlie on the hub's main: %q", s)
	}
	if err := peerGet(t, charlie, alpha); err == nil {
		t.Fatal("the hub still lets the revoked charlie in")
	}
	if n := installedCRLNumber(t, bravo); n != 1 {
		t.Fatalf("fixture: bravo holds CRL #%d before its round, want #1", n)
	}
	if err := peerGet(t, charlie, bravo); err != nil {
		t.Fatalf("before its round bravo still knows only CRL #1 and lets charlie in: %v", err)
	}

	bravo.round(t)
	if n := installedCRLNumber(t, bravo); n != 2 {
		t.Fatalf("bravo's round installed CRL #%d, want main's #2", n)
	}
	if err := peerGet(t, charlie, bravo); err == nil {
		t.Fatal("bravo installed the CRL that revokes charlie and still lets charlie in")
	}
	// Positive control: alpha, not revoked, still reaches bravo.
	if err := peerGet(t, alpha, bravo); err != nil {
		t.Fatalf("alpha is refused by bravo: %v", err)
	}
}
