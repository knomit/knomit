package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/remotesync"
	"knomit/internal/testsupport/testsigner"
)

// User ruling 6 (2026-10-16): the master is the instance the marker on MAIN
// names. These tests run on a forge, where no Host here exists and the
// marker is all that tells two root holders apart.

// forgeMarker is the agent id the forge's main names, "" when none.
func (f *forgeFleet) forgeMarker(t *testing.T) string {
	t.Helper()
	if !f.onForgeMain(t, fact.FleetMasterPath) {
		return ""
	}
	out, err := runGitOut(f.bare, "show", "main:"+fact.FleetMasterPath)
	require.NoError(t, err)
	id, err := fact.ParseFleetMaster([]byte(out))
	require.NoError(t, err)
	return id
}

// A root holder that main does not name does NO master work (#531 re-review
// S3 and S4): alpha imports the root while main names the master; a member's
// pushed change is neither admitted nor merged by alpha, and alpha's round
// publishes nothing to the forge's main. The named master still admits it.
// SABOTAGE: make Manager.isFleetMaster ignore the marker (holdsFleetRoot
// alone) -> alpha admits beta's branch and publishes main -> red; drop the
// "main never held the root" clause of publishesFleetMain's #518 arm ->
// alpha publishes main with its own commit -> red.
func TestFleetMaster_ARootHolderMainDoesNotNameDoesNoMasterWork(t *testing.T) {
	alpha, beta := testsigner.Named("alpha"), testsigner.Named("beta")
	f := newForgeFleet(t, map[string]ssh.Signer{"alpha": alpha, "beta": beta})
	am, ari := f.join(t, "alpha", alpha)
	_, bri := f.join(t, "beta", beta)
	round(t, f.mri) // the master admits and merges both records, publishes main
	settleMerger(t, f.mri)
	round(t, f.mri)
	installFleetRoot(t, am, f.root)
	round(t, ari) // alpha fetches the current main

	editOwnRecord(t, bri, "beta", "beta was here")
	round(t, bri) // beta pushes its change to the forge
	// alpha has a commit of its own beyond main: a publish of main would
	// carry it (#531 review 3 N2), so "main unchanged" means something.
	editOwnRecord(t, ari, "alpha", "alpha's own line")
	mainBefore := f.rev(t, "main")

	round(t, ari)
	settleMerger(t, ari)
	require.NoError(t, ari.WithRead(func(svc *store.Service) {
		require.True(t, am.holdsFleetRoot(svc), "setup: alpha holds the root")
		require.False(t, ari.isFleetMaster(svc), "main names the master, not alpha")
	}))
	require.Empty(t, localTip(t, ari, "agent/beta"), "alpha admits no member branch")
	require.Zero(t, ari.consensus.snapshot().Merges, "alpha merges nothing")
	require.Equal(t, mainBefore, f.rev(t, "main"), "alpha's round publishes nothing to main")
	require.NotEmpty(t, f.rev(t, "agent/alpha"), "alpha still pushes its own branch, as a member does")
	mainRec, err := runGitOut(f.bare, "show", "main:"+memberRecordPath("kb", "alpha"))
	require.NoError(t, err)
	require.NotContains(t, mainRec, "alpha's own line", "alpha's own commit reaches main only through the master's merge")
	require.Equal(t, "master", f.forgeMarker(t), "alpha claims nothing while main names the master")
	st, err := am.FleetStatus(context.Background())
	require.NoError(t, err)
	require.False(t, st.Master)
	require.True(t, st.RootHeld)
	require.Equal(t, "master", st.ActingMaster)
	_, err = am.EnrollFleetMember(context.Background(), "zed", keyLine("zed"), "")
	_, code := fleetCode(t, err)
	require.Equal(t, "not_master", code)

	// The named master still does its work.
	round(t, f.mri)
	require.NotEmpty(t, localTip(t, f.mri, "agent/beta"), "the master admits beta's change")
}

// Two root holders on a forge whose main names no master (a fleet created
// before the marker): both are candidates, the first to publish its claim
// wins, and the loser's claim is refused by the forge (not a fast-forward of
// the main it had). The loser is then a member: nothing of its claim is on
// its branch, its next round is clean and the master admits its branch.
// SABOTAGE: publish the claim with force (or commit it on the agent branch)
// -> two names or a refused branch -> red.
func TestFleetMaster_TwoRootHoldersRaceTheFirstPublishWins(t *testing.T) {
	alpha := testsigner.Named("alpha")
	f := newForgeFleet(t, map[string]ssh.Signer{"alpha": alpha})
	am, ari := f.join(t, "alpha", alpha)
	installFleetRoot(t, am, f.root)
	// A fleet with no marker: both root holders are candidates.
	f.forgeMainCommit(t, fact.FleetMasterPath, nil, "a fleet created before the marker")
	syncFleet(t, f.master)
	syncFleet(t, am)
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) { require.True(t, f.mri.fleetCandidate(svc)) }))
	require.NoError(t, ari.WithRead(func(svc *store.Service) { require.True(t, ari.fleetCandidate(svc)) }))

	// Both claim from the same main; the master publishes first.
	ctx := context.Background()
	f.master.claimFleetMaster(ctx, f.mri)
	// alpha's claim from the main it fetched is not a fast-forward any more:
	// a lost race, told apart from other push failures (review 3 N3).
	body := fact.RenderFleetMaster("alpha")
	require.ErrorIs(t, am.publishMarkerChange(ctx, ari, &body, "claim"), remotesync.ErrUpstreamMoved)
	am.claimFleetMaster(ctx, ari)
	require.Equal(t, "master", f.forgeMarker(t), "the first publish wins")

	// The loser's next round: a member, its branch clean.
	round(t, ari)
	require.NoError(t, ari.WithRead(func(svc *store.Service) { require.False(t, ari.isFleetMaster(svc)) }))
	onBranch, err := am.fleetMasterAt(ari, ari.AgentBranch())
	require.NoError(t, err)
	require.Equal(t, "master", onBranch, "alpha's branch holds main's marker, never its own claim")
	st, err := am.FleetStatus(ctx)
	require.NoError(t, err)
	require.Empty(t, st.LastError)
	require.False(t, st.Master)

	round(t, f.mri)
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) { require.True(t, f.mri.isFleetMaster(svc)) }))
	_, refused := f.mri.peers.Refusals()["agent/alpha"]
	require.False(t, refused, "the master admits the loser's branch: %+v", f.mri.peers.Refusals())
}

// User ruling 7: a forge-hosted fleet's decommission publishes the marker's
// deletion, so another holder of the root key takes the fleet over by the
// same claim.
// SABOTAGE: skip deleteFleetMaster in DecommissionFleet -> main still names
// the master -> alpha never claims -> red.
func TestFleetMaster_AForgeDecommissionLetsAnotherHolderTakeOver(t *testing.T) {
	alpha := testsigner.Named("alpha")
	f := newForgeFleet(t, map[string]ssh.Signer{"alpha": alpha})
	am, ari := f.join(t, "alpha", alpha)
	installFleetRoot(t, am, f.root)
	round(t, ari)
	require.NoError(t, ari.WithRead(func(svc *store.Service) { require.False(t, ari.isFleetMaster(svc)) }))

	_, err := f.master.DecommissionFleet(context.Background())
	require.NoError(t, err)
	require.Empty(t, f.forgeMarker(t), "the forge's main names no master")

	round(t, ari) // fetches the master-less main and claims
	round(t, ari)
	require.Equal(t, "alpha", f.forgeMarker(t))
	require.NoError(t, ari.WithRead(func(svc *store.Service) { require.True(t, ari.isFleetMaster(svc), "alpha took the fleet over") }))
	_, err = am.EnrollFleetMember(context.Background(), "zed", keyLine("zed"), "")
	require.NoError(t, err, "and does master work")
}
