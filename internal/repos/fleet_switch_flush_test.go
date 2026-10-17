package repos

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/pki"
	"knomit/internal/store"
)

// PR #537 re-review R1 (the reviewer's probe, kept as a regression test): a master switch while a revoke is
// unpublished. The old master revokes bravo with the forge unreachable
// (Published false), hands its root files to alpha, and stands down; alpha
// claims the role. alpha's main and agent branch do not hold the
// revocation, so its renewal round, with bravo's leaf due, renews it. The
// old master's branch (holding the revocation) may reach main later; the
// question is whether main ends with an unrevoked leaf for bravo.
func TestFleetSwitch_AnUnpublishedRevokeSurvivesAMasterSwitch(t *testing.T) {
	t.Run("renewal due", func(t *testing.T) { switchRevokeProbe(t, true) })
	t.Run("control: nothing due", func(t *testing.T) { switchRevokeProbe(t, false) })
	t.Run("only the CRL (and 1h leaves) due", func(t *testing.T) { switchRevokeProbeAt(t, time.Now().Add(50*time.Minute)) })
}

func switchRevokeProbe(t *testing.T, renew bool) { switchRevokeProbeAt(t, time.Time{}, renew) }

func switchRevokeProbeAt(t *testing.T, at time.Time, renewOpt ...bool) {
	renew := len(renewOpt) == 0 || renewOpt[0]
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	bravo := f.namedMember(t, "bravo")
	f.enrollAndJoin(t, alpha, "alpha")
	f.enrollAndJoin(t, bravo, "bravo")
	f.masterRounds(t)
	oldBravo := f.mainLeaf(t, "bravo")

	aside := f.bare + ".aside"
	require.NoError(t, os.Rename(f.bare, aside))
	rv, err := f.master.RevokeFleetMember(ctx, "bravo", "compromised")
	require.NoError(t, os.Rename(aside, f.bare))
	require.NoError(t, err)
	require.False(t, rv.Published, "fixture: the revoke's publish failed")

	root, err := f.master.FleetRootFiles()
	require.NoError(t, err)
	_, err = alpha.InstallFleetRoot(root.Files)
	require.NoError(t, err)
	require.NoError(t, f.master.StandDown(ctx))
	ari := alpha.fleetRepo()
	round(t, ari) // alpha claims the role
	st, err := alpha.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.Master, "fixture: alpha is the master: %+v", st)

	due := time.Now().Add(pki.Lifetime(oldBravo) * 3 / 4)
	if !renew {
		due = time.Now()
	}
	if !at.IsZero() {
		due = at
	}
	restore := SetFleetClockForTest(func() time.Time { return due })
	round(t, ari)
	restore()
	for i := 0; i < 4; i++ {
		round(t, f.mri) // the old master, now a member, pushes its branch
		round(t, ari)
		settleMerger(t, ari)
	}

	var revoked bool
	var serial string
	require.NoError(t, ari.WithRead(func(svc *store.Service) {
		fl, err := svc.FleetAt(svc.UpstreamBranch())
		require.NoError(t, err)
		revoked = fl.PKI.Revoked("bravo")
		c, _ := fl.PKI.Cert("bravo")
		serial = c.SerialNumber.Text(16)
	}))
	t.Logf("bravo: old serial %s, main's serial %s, revoked on main %v, forge CRL #%d",
		oldBravo.SerialNumber.Text(16), serial, revoked, f.mainCRLNumber(t))
	if ost, err := f.master.FleetStatus(ctx); err == nil {
		t.Logf("old master: last_error %q", ost.LastError)
	}
	t.Logf("forge agent/master has the revocation statement: %v", exec.Command("git", "-C", f.bare, "cat-file", "-e", "agent/master:artifacts/fleet/revoked/"+oldBravo.SerialNumber.Text(16)+".pem").Run() == nil)
	require.True(t, revoked, "bravo was revoked before the switch, but main holds an unrevoked leaf for it")
}

// The switch's flush (PR #537 re-review R1, user ruling: flush, block new
// changes, THEN give up the master role): while the master's fleet branch
// holds a commit main does not (here a revoke whose publish failed) and the
// forge is still unreachable, both the root export and the stand-down are
// refused with unpublished_changes, and the stand-down keeps the root and
// main's master marker. Once the forge is back, the export's own flush
// publishes the revocation and hands the root over.
// SABOTAGE: drop flushBeforeSwitch from StandDown -> the stand-down goes
// through with the revoke on the branch only -> red; drop its refusal (flush
// only) -> red; drop it from FleetRootFiles -> the export goes through ->
// red.
func TestFleetSwitch_ExportAndStandDownWaitForTheFlush(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	bravo := f.namedMember(t, "bravo")
	f.enrollAndJoin(t, bravo, "bravo")
	f.masterRounds(t)

	aside := f.bare + ".aside"
	require.NoError(t, os.Rename(f.bare, aside))
	rv, err := f.master.RevokeFleetMember(ctx, "bravo", "compromised")
	require.NoError(t, err)
	require.False(t, rv.Published, "fixture: the revoke's publish failed")

	_, err = f.master.FleetRootFiles()
	_, code := fleetCode(t, err)
	require.Equal(t, ErrCodeUnpublishedChanges, code, "the export waits for the flush")
	err = f.master.StandDown(ctx)
	_, code = fleetCode(t, err)
	require.Equal(t, ErrCodeUnpublishedChanges, code, "the stand-down waits for the flush")
	require.Contains(t, err.Error(), "Let the publish succeed first")
	_, statErr := os.Stat(filepath.Join(f.master.fleetRootDir(), pki.RootKeyFile))
	require.NoError(t, statErr, "the root key is kept")

	require.NoError(t, os.Rename(aside, f.bare))
	require.Equal(t, "master", f.forgeMarker(t), "main still names the master")
	require.Equal(t, int64(1), f.mainCRLNumber(t), "fixture: the revocation is not on the forge yet")
	root, err := f.master.FleetRootFiles()
	require.NoError(t, err, "with the forge back, the export flushes and goes through")
	require.NotEmpty(t, root.Files)
	require.Equal(t, int64(2), f.mainCRLNumber(t), "the flush published the revocation")
	require.NoError(t, f.master.StandDown(ctx))
}

// PR #537 re-review 2 (the reviewer's probe, kept as a regression test):
// every way a forge master gives up the role flushes first. The root is
// exported to alpha EARLY (nothing unpublished yet, so that flush passes);
// then the master revokes bravo with the forge unreachable (unpublished);
// the forge comes back and the master gives up the role by stand-down or by
// decommission; alpha claims and runs its rounds, with and without bravo's
// renewal due. bravo stays revoked on main in all four.
// SABOTAGE: drop flushBeforeSwitch from DecommissionFleet -> the
// decommission cases lose the revocation (renewal due: bravo is renewed)
// -> red.
func TestFleetSwitch_GivingUpTheRoleAfterAnEarlyExport(t *testing.T) {
	for _, how := range []string{"standdown", "decommission"} {
		for _, renew := range []bool{false, true} {
			name := how
			if renew {
				name += "/renewal_due"
			} else {
				name += "/nothing_due"
			}
			t.Run(name, func(t *testing.T) { giveUpAfterEarlyExport(t, how, renew) })
		}
	}
}

func giveUpAfterEarlyExport(t *testing.T, how string, renew bool) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	bravo := f.namedMember(t, "bravo")
	f.enrollAndJoin(t, alpha, "alpha")
	f.enrollAndJoin(t, bravo, "bravo")
	f.masterRounds(t)
	oldBravo := f.mainLeaf(t, "bravo")

	root, err := f.master.FleetRootFiles() // early export: nothing unpublished yet
	require.NoError(t, err)
	_, err = alpha.InstallFleetRoot(root.Files)
	require.NoError(t, err)

	aside := f.bare + ".aside"
	require.NoError(t, os.Rename(f.bare, aside))
	rv, err := f.master.RevokeFleetMember(ctx, "bravo", "compromised")
	require.NoError(t, os.Rename(aside, f.bare))
	require.NoError(t, err)
	require.False(t, rv.Published, "fixture: the revoke's publish failed")

	switch how {
	case "standdown":
		err = f.master.StandDown(ctx)
	case "decommission":
		_, err = f.master.DecommissionFleet(ctx)
	}
	require.NoError(t, err, how)

	ari := alpha.fleetRepo()
	round(t, ari) // alpha claims the role
	st, err := alpha.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.Master, "fixture: alpha is the master: %+v", st)

	due := time.Now()
	if renew {
		due = time.Now().Add(pki.Lifetime(oldBravo) * 3 / 4)
	}
	restore := SetFleetClockForTest(func() time.Time { return due })
	round(t, ari)
	restore()
	for i := 0; i < 3; i++ {
		if how == "standdown" {
			round(t, f.mri)
		}
		round(t, ari)
		settleMerger(t, ari)
	}
	var revoked bool
	var serial string
	require.NoError(t, ari.WithRead(func(svc *store.Service) {
		fl, err := svc.FleetAt(svc.UpstreamBranch())
		require.NoError(t, err)
		revoked = fl.PKI.Revoked("bravo")
		c, _ := fl.PKI.Cert("bravo")
		serial = c.SerialNumber.Text(16)
	}))
	t.Logf("%s renew=%v: bravo old serial %s, main's %s, revoked on main %v, CRL #%d", how, renew, oldBravo.SerialNumber.Text(16), serial, revoked, f.mainCRLNumber(t))
	require.True(t, revoked, "bravo's revocation was lost across the role change")
	require.Equal(t, oldBravo.SerialNumber.Text(16), serial, "bravo's revoked leaf was not renewed")
}

// A forge decommission refuses (409 unpublished_changes) while the flush
// cannot publish: nothing is decommissioned, the repository and the root
// key stay, and main still names this master.
// SABOTAGE: drop flushBeforeSwitch from DecommissionFleet -> the refusal
// is decommission_unpublished (the marker's own publish failing), not
// unpublished_changes -> red.
func TestFleetSwitch_ADecommissionWaitsForTheFlush(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	bravo := f.namedMember(t, "bravo")
	f.enrollAndJoin(t, bravo, "bravo")
	f.masterRounds(t)

	aside := f.bare + ".aside"
	require.NoError(t, os.Rename(f.bare, aside))
	rv, err := f.master.RevokeFleetMember(ctx, "bravo", "compromised")
	require.NoError(t, err)
	require.False(t, rv.Published, "fixture: the revoke's publish failed")
	_, err = f.master.DecommissionFleet(ctx)
	_, code := fleetCode(t, err)
	require.Equal(t, ErrCodeUnpublishedChanges, code, "the decommission waits for the flush")
	require.Contains(t, err.Error(), "fix the fleet's origin")
	require.NotNil(t, f.master.fleetRepo(), "the repository is kept")
	_, statErr := os.Stat(filepath.Join(f.master.fleetRootDir(), pki.RootKeyFile))
	require.NoError(t, statErr, "the root key is kept")

	require.NoError(t, os.Rename(aside, f.bare))
	require.Equal(t, "master", f.forgeMarker(t), "main still names the master")
	_, err = f.master.DecommissionFleet(ctx)
	require.NoError(t, err, "with the forge back, the decommission flushes and goes through")
	require.Equal(t, int64(2), f.mainCRLNumber(t), "the flush published the revocation")
}

// On the hub (no origin) with main held by the judge of its own commits,
// the unpublished_changes refusal says what lets main move: dropping that
// commit (the drop-commit card), not fixing an origin the hub does not have,
// nor waiting, which never helps.
// SABOTAGE: drop the ErrOwnCommitRefused case from unpublishedWayOut -> the
// message lacks the drop advice -> red.
func TestFleetSwitch_TheRefusalOnAHeldHubMainPointsAtTheDrop(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	ri := m.fleetRepo()
	var bad string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		// Past the store's write cores, which refuse it at the source.
		h, err := svc.TestingCommitFiles(ri.AgentBranch(), map[string]string{"kb/members/" + m.AgentID() + "/note.md": notPolicy}, "a bad note")
		require.NoError(t, err)
		bad = h
	}))

	_, err = m.FleetRootFiles()
	_, code := fleetCode(t, err)
	require.Equal(t, ErrCodeUnpublishedChanges, code)
	require.Contains(t, err.Error(), `"Drop commit `+bad[:8]+`" card`)
	require.Contains(t, err.Error(), "waiting does not help")
	require.NotContains(t, err.Error(), "origin")
}
