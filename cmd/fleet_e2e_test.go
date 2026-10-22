package cmd

import (
	"bytes"
	"context"
	"crypto/x509"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki"
	"knomit/internal/platform/fileuri"
)

// installedLeafOf is the instance certificate installed in n's [tls].dir.
func installedLeafOf(t *testing.T, n *fleetNode) *x509.Certificate {
	t.Helper()
	c, err := pki.LoadInstanceCert(n.cfg.TLS.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// renewalDue is a decision clock inside the final third of leaf's own
// lifetime (and so of every younger artifact of the same lifetime).
func renewalDue(leaf *x509.Certificate) func() time.Time {
	at := leaf.NotBefore.Add(pki.Lifetime(leaf) * 3 / 4)
	return func() time.Time { return at }
}

// The tick duties end to end on a knomit-hosted fleet (fleet PR8 item 7),
// over real mTLS listeners: create -> join -> revoke -> a member installs
// the CRL -> renewal -> leave -> rejoin.
//   - alpha creates and hosts; bravo and charlie join and are accepted.
//   - alpha revokes charlie: bravo's round installs CRL #2; charlie's round
//     is refused at the hub's handshake: revoked, sync_stopped.
//   - with the decision clock in the final third of the leaves' lifetimes,
//     ONE hub round re-signs the CRL (#3) and renews bravo's and alpha's own
//     leaves (charlie's is revoked: not renewed); main moved, so the hub's own
//     judge admitted them. Bravo's round installs its renewed leaf and CRL #3
//     and keeps syncing with it; alpha's next round installs its own renewal,
//     and bravo still reaches the hub presenting it.
//   - bravo leaves: the hub's round revokes it (CRL #4); it rejoins with the
//     same key and is accepted.
//   - charlie leaves and rejoins: the new certificate syncs again (not
//     revoked, not stopped).
func TestFleetE2E_KnomitHosted(t *testing.T) {
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

	// Revoke; a member installs the CRL; the revoked one says so and stops.
	if _, err := alpha.mgr.RevokeFleetMember(ctx, "charlie", "e2e"); err != nil {
		t.Fatal(err)
	}
	bravo.round(t)
	if n := installedCRLNumber(t, bravo); n != 2 {
		t.Fatalf("bravo's round installed CRL #%d, want #2", n)
	}
	loopRound(t, charlie)
	if got := fleetJSON(t, charlie); got["revoked"] != true || got["sync_stopped"] != true || got["last_error_refusal"] != "revoked" {
		t.Fatalf("charlie after the revoke: %v", got)
	}

	// Renewal.
	oldA, oldB, oldC := mainLeaf(t, alpha, "alpha"), mainLeaf(t, alpha, "bravo"), mainLeaf(t, alpha, "charlie")
	restore := repos.SetFleetClockForTest(renewalDue(oldB))
	hubRound(t, alpha)
	restore()
	newA, newB := mainLeaf(t, alpha, "alpha"), mainLeaf(t, alpha, "bravo")
	if newA.SerialNumber.Cmp(oldA.SerialNumber) == 0 || newB.SerialNumber.Cmp(oldB.SerialNumber) == 0 {
		t.Fatal("the hub's round renewed no leaf")
	}
	if !newB.NotBefore.After(oldB.NotBefore) || pki.Lifetime(newB) != pki.Lifetime(oldB) {
		t.Fatalf("bravo's renewal: NotBefore %s (was %s), lifetime %s (was %s)", newB.NotBefore, oldB.NotBefore, pki.Lifetime(newB), pki.Lifetime(oldB))
	}
	if mainLeaf(t, alpha, "charlie").SerialNumber.Cmp(oldC.SerialNumber) != 0 {
		t.Fatal("the revoked charlie's leaf was renewed")
	}
	bravo.round(t)
	if !bytes.Equal(installedLeafOf(t, bravo).Raw, newB.Raw) || installedCRLNumber(t, bravo) != 3 {
		t.Fatalf("bravo after its round: leaf %s, CRL #%d; want the renewal and #3", installedLeafOf(t, bravo).SerialNumber.Text(16), installedCRLNumber(t, bravo))
	}
	// The renewal is what bravo PRESENTS (review S2): the old leaf is still
	// valid and unrevoked, so "no sync error" alone would prove nothing.
	seenBefore := len(alpha.peerSerials())
	bravo.round(t)
	if st, _ := bravo.mgr.FleetStatus(ctx); st.LastError != "" {
		t.Fatalf("bravo syncs with its renewed leaf: %s", st.LastError)
	}
	seen := alpha.peerSerials()[seenBefore:]
	if len(seen) == 0 {
		t.Fatal("bravo's round reached the hub's /git with no request")
	}
	for _, s := range seen {
		if s != newB.SerialNumber.Text(16) {
			t.Fatalf("bravo presented serial %s to the hub, want its renewal %s (old %s)", s, newB.SerialNumber.Text(16), oldB.SerialNumber.Text(16))
		}
	}
	hubRound(t, alpha)
	if !bytes.Equal(installedLeafOf(t, alpha).Raw, newA.Raw) {
		t.Fatal("the hub did not install its own renewal")
	}
	// And the hub presents its renewal to bravo's client: the certificate
	// bravo's mTLS client verified on a fresh handshake.
	c, err := pki.HTTPClient(bravo.cfg.TLS.Dir, bravo.key)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("https://" + alpha.tlsAddr + "/api/v1/fleet")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.TLS.PeerCertificates[0].SerialNumber; got.Cmp(newA.SerialNumber) != 0 {
		t.Fatalf("the hub presented serial %s, want its renewal %s (old %s)", got.Text(16), newA.SerialNumber.Text(16), oldA.SerialNumber.Text(16))
	}
	bravo.round(t)
	if st, _ := bravo.mgr.FleetStatus(ctx); st.LastError != "" {
		t.Fatalf("bravo against the hub's renewed certificate: %s", st.LastError)
	}

	// Leave (revoked by the hub's round) and rejoin.
	bravo.asClient(t)
	if left, err := bravo.mgr.LeaveFleet(ctx); err != nil || !left.Pushed {
		t.Fatalf("bravo leaves: %v %+v", err, left)
	}
	hubPublishes(t, alpha, recordPath("bravo"), "state: left")
	hubRound(t, alpha)
	if s := memberStates(t, alpha)["bravo"]; s != fact.MemberRevoked {
		t.Fatalf("bravo after the hub's round: %q, want revoked", s)
	}
	if st := joinAnswer(t, bravo, alpha, ""); st.RecordState != repos.RecordPending {
		t.Fatalf("bravo's rejoin: %+v", st)
	}
	hubPublishes(t, alpha, recordPath("bravo"), "state: active")
	bravo.round(t)
	if got := recordStateOverHTTP(t, bravo); got != fact.MemberActive {
		t.Fatalf("bravo after the rejoin: %q", got)
	}

	charlie.asClient(t)
	if _, err := charlie.mgr.LeaveFleet(ctx); err != nil {
		t.Fatal(err)
	}
	joinAnswer(t, charlie, alpha, "")
	hubPublishes(t, alpha, recordPath("charlie"), "state: active")
	charlie.round(t)
	if got := fleetJSON(t, charlie); got["revoked"] != nil || got["sync_stopped"] != nil || got["record_state"] != "active" {
		t.Fatalf("charlie after its rejoin: %v", got)
	}
}

// The same on a forge-hosted fleet (the master fetches, judges, merges and
// publishes main; members read main from the forge, so a revoked member
// learns it from main's CRL in its own round).
func TestFleetE2E_Forge(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	forgeDir := filepath.Join(dir, "forge")
	if err := os.MkdirAll(forgeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(forgeDir, "fleet.git")
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
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{Origin: &repos.OriginSpec{URL: forge, AuthMethod: "none"}}); err != nil {
		t.Fatal(err)
	}
	bravo := newFleetNode(t, dir, "bravo", allow)
	charlie := newFleetNode(t, dir, "charlie", allow)
	until := func(what string, cond func() bool) {
		t.Helper()
		untilMasterRounds(t, alpha, what, cond)
	}
	joinAnswer(t, bravo, alpha, "none")
	joinAnswer(t, charlie, alpha, "none")
	until("both records onto main", func() bool {
		s := memberStates(t, alpha)
		return s["bravo"] == fact.MemberActive && s["charlie"] == fact.MemberActive
	})
	bravo.round(t)
	charlie.round(t)

	if rv, err := alpha.mgr.RevokeFleetMember(ctx, "charlie", "e2e"); err != nil || !rv.Published {
		t.Fatalf("revoke: %v %+v", err, rv)
	}
	bravo.round(t)
	if n := installedCRLNumber(t, bravo); n != 2 {
		t.Fatalf("bravo's round installed CRL #%d, want #2", n)
	}
	loopRound(t, charlie)
	if got := fleetJSON(t, charlie); got["revoked"] != true || got["sync_stopped"] != true || got["record_state"] != "revoked" {
		t.Fatalf("charlie after its round: %v", got)
	}

	oldA, oldB, oldC := mainLeaf(t, alpha, "alpha"), mainLeaf(t, alpha, "bravo"), mainLeaf(t, alpha, "charlie")
	restore := repos.SetFleetClockForTest(renewalDue(oldB))
	alpha.round(t)
	restore()
	if !strings.Contains(bareShow(t, bare, "main", "artifacts/fleet/certs/bravo.pem"), "CERTIFICATE") {
		t.Fatal("fixture: bravo's leaf on the forge's main")
	}
	alpha.round(t) // local main follows the published one
	newA, newB := mainLeaf(t, alpha, "alpha"), mainLeaf(t, alpha, "bravo")
	if newA.SerialNumber.Cmp(oldA.SerialNumber) == 0 || newB.SerialNumber.Cmp(oldB.SerialNumber) == 0 {
		t.Fatal("the forge master's round renewed no leaf")
	}
	if mainLeaf(t, alpha, "charlie").SerialNumber.Cmp(oldC.SerialNumber) != 0 {
		t.Fatal("the revoked charlie's leaf was renewed")
	}
	bravo.round(t)
	if !bytes.Equal(installedLeafOf(t, bravo).Raw, newB.Raw) || installedCRLNumber(t, bravo) != 3 {
		t.Fatalf("bravo after its round: CRL #%d; want its renewal and #3", installedCRLNumber(t, bravo))
	}
	if !bytes.Equal(installedLeafOf(t, alpha).Raw, newA.Raw) {
		t.Fatal("the forge master did not install its own renewal")
	}

	bravo.asClient(t)
	if left, err := bravo.mgr.LeaveFleet(ctx); err != nil || !left.Pushed {
		t.Fatalf("bravo leaves: %v %+v", err, left)
	}
	until("bravo's revocation", func() bool { return memberStates(t, alpha)["bravo"] == fact.MemberRevoked })
	joinAnswer(t, bravo, alpha, "none")
	until("bravo's rejoin", func() bool { return memberStates(t, alpha)["bravo"] == fact.MemberActive })
	bravo.round(t)
	if got := recordStateOverHTTP(t, bravo); got != fact.MemberActive {
		t.Fatalf("bravo after the rejoin: %q", got)
	}

	if _, err := charlie.mgr.LeaveFleet(ctx); err != nil {
		t.Fatal(err)
	}
	joinAnswer(t, charlie, alpha, "none")
	until("charlie's rejoin", func() bool { return memberStates(t, alpha)["charlie"] == fact.MemberActive })
	charlie.round(t)
	if got := fleetJSON(t, charlie); got["revoked"] != nil || got["sync_stopped"] != nil || got["record_state"] != "active" {
		t.Fatalf("charlie after its rejoin: %v", got)
	}
}
