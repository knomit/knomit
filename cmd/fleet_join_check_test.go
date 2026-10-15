package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/pki"
	"knomit/internal/repos"
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

// POST /fleet/join:check against a knomit-hosted fleet: the hub is reached
// with the BUNDLE's certificate and judged against the BUNDLE's root while
// nothing is installed here, so the answer is hosting master with the root
// fingerprint the operator confirms; a credential for the hub is
// auth_invalid; a bundle naming a host that does not answer is unreachable;
// a bundle for another agent is agent_mismatch. Each check leaves nothing
// behind, and the join after a good check succeeds.
// SABOTAGE: classify against this instance's [tls].dir instead of the
// bundle's material -> no trust here, so the hub is "forge" -> red.
func TestFleetJoinCheck_KnomitHosted(t *testing.T) {
	ctx := context.Background()
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

	_, err = bravo.mgr.CheckFleetJoin(ctx, repos.FleetJoinSpec{Bundle: []byte(enr.Bundle), AuthMethod: "token", AuthToken: "ghp_x"})
	if code := fleetCodeOf(t, err); code != "auth_invalid" {
		t.Fatalf("check with a token: %s (%v)", code, err)
	}
	requireNothingInstalled(t, bravo, "after the token check")

	// The same certificates naming a host that does not answer.
	bare := enr.Bundle[strings.Index(enr.Bundle, "-----BEGIN "):]
	dead, err := pki.FleetBundle([]byte(bare), "https://127.0.0.1:"+freePort(t)+"/git/fleet")
	if err != nil {
		t.Fatal(err)
	}
	_, err = bravo.mgr.CheckFleetJoin(ctx, repos.FleetJoinSpec{Bundle: dead})
	if code := fleetCodeOf(t, err); code != "unreachable" {
		t.Fatalf("check of a dead host: %s (%v)", code, err)
	}
	requireNothingInstalled(t, bravo, "after the unreachable check")

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
