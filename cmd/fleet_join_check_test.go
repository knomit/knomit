package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/corpus/repos"
	"knomit/internal/foundation/pki"
)

// requireNothingInstalled: no trust material, no fleet state, no repository.
func requireNothingInstalled(t *testing.T, n *fleetNode, why string) {
	t.Helper()
	for _, f := range []string{pki.InstanceCertFile, pki.RootCertFile, pki.CRLFile} {
		if _, err := os.Stat(filepath.Join(n.cfg.TLS.Dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: %s is installed (%v)", why, f, err)
		}
	}
	if names := n.mgr.Names(); len(names) != 0 {
		t.Fatalf("%s: repositories %v", why, names)
	}
	if st, _ := n.mgr.FleetStatus(context.Background()); st.State != repos.FleetStandalone {
		t.Fatalf("%s: state %q", why, st.State)
	}
}

// joinCheckTemp points os.TempDir at a directory of this test's, so
// requireNoJoinCheckTemp can see what the check left there.
func joinCheckTemp(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("TMPDIR", d)
	return d
}

// requireNoJoinCheckTemp: the check removed its private material directory.
func requireNoJoinCheckTemp(t *testing.T, d, why string) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(d, "knomit-join-check-*"))
	if err != nil || len(left) != 0 {
		t.Fatalf("%s: the check left %v (%v)", why, left, err)
	}
}

// POST /fleet/join:check against a knomit-hosted fleet: the hub is reached
// with the BUNDLE's certificate and judged against the BUNDLE's root while
// nothing is installed here, so the answer is hosting master with the root
// fingerprint the operator confirms; a credential for the hub is
// auth_invalid; a bundle naming a host that does not answer is unreachable;
// a bundle for another agent is agent_mismatch, and the join of that dead
// host says unreachable too. Each check leaves nothing
// behind (its temporary material directory included), and the join after a
// good check succeeds.
// SABOTAGE: classify against this instance's [tls].dir instead of the
// bundle's material -> no trust here, so the hub is "forge" -> red; drop the
// removal of the temporary directory -> red.
func TestFleetJoinCheck_KnomitHosted(t *testing.T) {
	ctx := context.Background()
	tmp := joinCheckTemp(t)
	dir := t.TempDir()
	alpha := newFleetNode(t, dir, "alpha")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{}); err != nil {
		t.Fatal(err)
	}
	alpha.serve(t)
	bravo := newFleetNode(t, dir, "bravo")
	enr, err := alpha.mgr.EnrollFleetMember(ctx, "bravo", bravo.keyLine(), "")
	if err != nil || !enr.Published {
		t.Fatalf("enroll: %v %+v", err, enr)
	}

	before := alpha.receives.Load()
	chk, err := bravo.mgr.CheckFleetJoin(ctx, repos.FleetJoinSpec{Bundle: []byte(enr.Bundle)})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if chk.URL != alpha.fleetURL() || chk.Hosting != "master" || chk.Host != alpha.tlsAddr ||
		chk.RootFingerprint != enr.RootFingerprint || chk.AgentID != "bravo" || chk.AuthRequired {
		t.Fatalf("check: %+v; want master at %s with root %s", chk, alpha.fleetURL(), enr.RootFingerprint)
	}
	if alpha.receives.Load() != before {
		t.Fatal("the check pushed to the hub")
	}
	requireNothingInstalled(t, bravo, "after the check")
	requireNoJoinCheckTemp(t, tmp, "after the check")

	_, err = bravo.mgr.CheckFleetJoin(ctx, repos.FleetJoinSpec{Bundle: []byte(enr.Bundle), AuthMethod: "token", AuthToken: "ghp_x"})
	if code := fleetCodeOf(t, err); code != "auth_invalid" {
		t.Fatalf("check with a token: %s (%v)", code, err)
	}
	requireNothingInstalled(t, bravo, "after the token check")
	requireNoJoinCheckTemp(t, tmp, "after the token check")

	// The same certificates naming a host that does not answer.
	bare := enr.Bundle[strings.Index(enr.Bundle, "-----BEGIN "):]
	dead, err := pki.FleetBundle([]byte(bare), "https://127.0.0.1:"+fleetFreePort(t)+"/git/fleet")
	if err != nil {
		t.Fatal(err)
	}
	_, err = bravo.mgr.CheckFleetJoin(ctx, repos.FleetJoinSpec{Bundle: dead})
	if code := fleetCodeOf(t, err); code != "unreachable" {
		t.Fatalf("check of a dead host: %s (%v)", code, err)
	}
	requireNothingInstalled(t, bravo, "after the unreachable check")
	requireNoJoinCheckTemp(t, tmp, "after the unreachable check")
	// The join says the same for the same cause (NEW-1): unreachable 502,
	// not clone_failed, and nothing is kept.
	_, err = bravo.mgr.JoinFleet(ctx, repos.FleetJoinSpec{Bundle: dead})
	var fe *repos.FleetError
	if !errors.As(err, &fe) || fe.Code != "unreachable" || fe.Status != 502 {
		t.Fatalf("join of a dead host: %v, want 502 unreachable", err)
	}
	requireNothingInstalled(t, bravo, "after the unreachable join")

	charlie := newFleetNode(t, dir, "charlie")
	_, err = charlie.mgr.CheckFleetJoin(ctx, repos.FleetJoinSpec{Bundle: []byte(enr.Bundle)})
	if code := fleetCodeOf(t, err); code != "key_mismatch" && code != "agent_mismatch" {
		t.Fatalf("another instance's check: %s (%v)", code, err)
	}
	requireNothingInstalled(t, charlie, "after another instance's check")

	bravo.asClient(t)
	if st, err := bravo.mgr.JoinFleet(ctx, repos.FleetJoinSpec{Bundle: []byte(enr.Bundle)}); err != nil || st.RootFingerprint != chk.RootFingerprint {
		t.Fatalf("join after the check: %v %+v", err, st)
	}
	requireCertOrigin(t, bravo.mgr, bravo.fleet(t), alpha.fleetURL())
}

// A bundle whose certificate the master has since revoked (an old bundle
// kept after a revoke): the hub IS reached and refuses the certificate, so
// the check and the join both answer bundle_refused (422) telling the
// operator to ask for a new bundle, never unreachable; and nothing is left.
// SABOTAGE: CheckFleetJoin mapping every classification error to
// unreachable -> red; JoinFleet answering clone_failed -> red.
func TestFleetJoinCheck_ARevokedBundleIsRefusedNotUnreachable(t *testing.T) {
	ctx := context.Background()
	tmp := joinCheckTemp(t)
	dir := t.TempDir()
	alpha := newFleetNode(t, dir, "alpha")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{}); err != nil {
		t.Fatal(err)
	}
	alpha.serve(t)
	bravo := newFleetNode(t, dir, "bravo")
	enr, err := alpha.mgr.EnrollFleetMember(ctx, "bravo", bravo.keyLine(), "")
	if err != nil || !enr.Published {
		t.Fatalf("enroll: %v %+v", err, enr)
	}
	if rv, err := alpha.mgr.RevokeFleetMember(ctx, "bravo", "lost"); err != nil || rv.InstallError != "" {
		t.Fatalf("revoke: %v %+v", err, rv)
	}

	_, err = bravo.mgr.CheckFleetJoin(ctx, repos.FleetJoinSpec{Bundle: []byte(enr.Bundle)})
	requireBundleRefused(t, err, "check")
	requireNothingInstalled(t, bravo, "after the check")
	requireNoJoinCheckTemp(t, tmp, "after the refused check")
	requireNoJoinCheckTemp(t, tmp, "after the check")

	bravo.asClient(t)
	_, err = bravo.mgr.JoinFleet(ctx, repos.FleetJoinSpec{Bundle: []byte(enr.Bundle)})
	requireBundleRefused(t, err, "join")
	requireNothingInstalled(t, bravo, "after the join")
}

func requireBundleRefused(t *testing.T, err error, what string) {
	t.Helper()
	var fe *repos.FleetError
	if !errors.As(err, &fe) {
		t.Fatalf("%s: want a FleetError, got %v", what, err)
	}
	if fe.Code != "bundle_refused" || fe.Status != 422 || !strings.Contains(fe.Msg, pki.ErrRefusedByPeer.Error()) {
		t.Fatalf("%s: %d %s (%s), want 422 bundle_refused", what, fe.Status, fe.Code, fe.Msg)
	}
	if !strings.Contains(fe.Msg, "ask the fleet's master for a new bundle") {
		t.Fatalf("%s: message %q does not say what to do", what, fe.Msg)
	}
}
