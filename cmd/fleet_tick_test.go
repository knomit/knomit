package cmd

import (
	"context"
	"net/http"
	"testing"

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
