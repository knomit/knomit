package repos

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// notPolicy is a fact in a member's folder that fails the fleet ontology's
// members-are-policy validation.
const notPolicy = "---\ntype: observation\n---\n# a note\n\nnot a policy\n"

// The hub (a fleet master with no origin): a commit on its own agent branch
// that fails the fleet ontology's validations — written here past the fleet
// code, as a bug or a hand edit would — is refused by the judge of the
// master's own commits BEFORE the local reconcile moves main, and the
// refusal is the fleet's last error (GET /fleet last_error, commit refused).
// main never holds the fact.
// SABOTAGE: drop the judgeOwnRange call from AdvanceLocalUpstream -> main
// takes the fact -> red; drop fleetMainAdvanced from the loop (pass nil) ->
// last_error stays empty -> red.
func TestFleetJudgeOwn_TheHubRefusesItsOwnInvalidRecordBeforeMainMoves(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	ri := m.fleetRepo()
	mainBefore := localTip(t, ri, "main")
	require.Equal(t, localTip(t, ri, ri.AgentBranch()), mainBefore, "fixture: main follows the branch")

	bad := path.Join("kb/members", m.AgentID(), "note.md")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(ctx, ri.AgentBranch(), bad, notPolicy, "a bad note", "created")
		require.NoError(t, err)
	}))
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		runLocalReconcileLoop(ctx, svc, ri.Name(), ri.agentBranch, time.Hour, nil, nil, nil, true, ri.fleetHostRound, ri.fleetMainAdvanced)
	}))
	require.Equal(t, mainBefore, localTip(t, ri, "main"), "main did not move")
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Contains(t, st.LastError, store.OwnCommitRefusedText)
	require.Contains(t, st.LastError, `"members-are-policy"`)
	require.False(t, st.LastErrorCommitRefused, "not a host refusing this copy's push: the member's leave/revoke advice does not apply")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, ok, err := svc.FileAt("main", bad, 1<<20)
		require.NoError(t, err)
		require.False(t, ok, "main never holds the invalid fact")
	}))
}

// A forge-hosted master: the same refusal stops its publish of main
// (PublishUpstream) before anything is pushed to the forge's main, and the
// sync round records it as the fleet's last error. A forged marker it would
// publish straight onto the forge's main (PublishUpstreamChange) naming
// another agent is refused the same way.
// SABOTAGE: drop the judgeOwnRange call from PublishUpstream -> the forge's
// main takes the fact -> red; drop it from PublishUpstreamChange -> the
// forge's main names "other" -> red.
func TestFleetJudgeOwn_AForgeMasterRefusesItsOwnInvalidWritesBeforePublishing(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	round(t, f.mri)
	mainBefore := f.rev(t, "main")

	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(ctx, f.mri.AgentBranch(), "kb/members/master/note.md", notPolicy, "a bad note", "created")
		require.NoError(t, err)
	}))
	roundOnPush(t, f.mri, f.master.fleetPushed)
	require.Equal(t, mainBefore, f.rev(t, "main"), "the forge's main did not move")
	require.False(t, f.onForgeMain(t, "kb/members/master/note.md"))
	st, err := f.master.FleetStatus(ctx)
	require.NoError(t, err)
	require.Contains(t, st.LastError, `"members-are-policy"`)
	require.Contains(t, st.LastError, store.OwnCommitRefusedText)

	other := fact.RenderFleetMaster("other")
	err = f.master.publishMarkerChange(ctx, f.mri, &other, "forged")
	require.ErrorIs(t, err, store.ErrOwnCommitRefused)
	require.Contains(t, err.Error(), "host-record")
	require.Equal(t, mainBefore, f.rev(t, "main"), "the forge's main still names the master")
}

// A member's branch fetched by a forge-hosted master that adds a fact
// failing members-are-policy is refused by the fetch judge (AdmitPeerBranch)
// and never becomes the local branch; an invalid fact already on main
// (pushed to the forge's main past the master: trust the forge) that the
// member's branch does not touch refuses nothing: its join is admitted.
// SABOTAGE: drop the incomingValid call from judgeIncoming -> the invalid
// note is admitted (it is in the member's own folder) -> red.
func TestFleetJudgeOwn_TheFetchJudgeRefusesAMemberInvalidFact(t *testing.T) {
	ctx := context.Background()
	alpha := testsigner.Named("alpha")
	f := newForgeFleet(t, map[string]ssh.Signer{"alpha": alpha})

	// A legacy invalid fact on the forge's main, which the master adopts.
	work := t.TempDir()
	runGit(t, "", "clone", "-q", f.bare, work)
	require.NoError(t, os.MkdirAll(filepath.Join(work, "kb/members/legacy"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, "kb/members/legacy/old.md"), []byte(notPolicy), 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.email=h@h", "-c", "user.name=h", "commit", "-q", "-m", "legacy")
	runGit(t, work, "push", "-q", "origin", "main")
	round(t, f.mri)

	_, ari := f.join(t, "alpha", alpha)
	round(t, f.mri)
	require.Equal(t, f.rev(t, "agent/alpha"), localTip(t, f.mri, "agent/alpha"), "the join is admitted: the legacy fact is untouched")

	require.NoError(t, ari.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(ctx, ari.AgentBranch(), "kb/members/alpha/note.md", notPolicy, "a bad note", "created")
		require.NoError(t, err)
	}))
	round(t, ari)
	tip := f.rev(t, "agent/alpha")
	round(t, f.mri)
	r := f.mri.peers.Refusals()["agent/alpha"]
	require.Equal(t, tip, r.Tip, "the refusal is of the pushed tip")
	require.Contains(t, r.Reason, store.CommitRefusedByValidation)
	require.Contains(t, r.Reason, "kb/members/alpha/note.md")
	require.Contains(t, r.Reason, `"members-are-policy"`)
	require.NotEqual(t, tip, localTip(t, f.mri, "agent/alpha"), "the refused tip never becomes the local branch")
	settleMerger(t, f.mri)
	round(t, f.mri)
	require.False(t, f.onForgeMain(t, "kb/members/alpha/note.md"), "the invalid fact never reaches main")
}
