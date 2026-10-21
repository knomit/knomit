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
	"knomit/internal/store/remotesync"
	"knomit/internal/store/storetest"
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
// main never holds the fact. GET /fleet names the commit, and
// DropFleetCommit (POST /fleet/drop-commit) is the way out: the branch is
// rebuilt on main without it, a later good commit kept, and main moves.
// SABOTAGE: drop the judgeOwnRange call from AdvanceLocalUpstream -> main
// takes the fact -> red; drop fleetMainAdvanced from the loop (pass nil) ->
// last_error stays empty -> red; keep the dropped commit in
// DropUnpublishedCommit's replay -> main stays held -> red.
func TestFleetJudgeOwn_TheHubRefusesItsOwnInvalidRecordBeforeMainMoves(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	ri := m.fleetRepo()
	mainBefore := localTip(t, ri, "main")
	require.Equal(t, localTip(t, ri, ri.AgentBranch()), mainBefore, "fixture: main follows the branch")

	bad := path.Join("kb/members", m.AgentID(), "note.md")
	good := path.Join("kb/members", m.AgentID(), "good.md")
	const goodNote = "---\nkind: pragmatic\ntype: policy\n---\n# a good note\n\na policy\n"
	var badCommit string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		// Past the store's write cores, which refuse it at the source.
		h, err := storetest.CommitFiles(svc, ri.AgentBranch(), map[string]string{bad: notPolicy}, "a bad note")
		require.NoError(t, err)
		badCommit = h
		// A legitimate write after it, which the way out must keep.
		_, err = svc.Facts().WriteFact(ctx, ri.AgentBranch(), good, goodNote, "a good note", "created")
		require.NoError(t, err)
	}))
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		runLocalReconcileLoop(ctx, svc, ri.Name(), ri.agentBranch, time.Hour, nil, nil, nil, true, ri.fleetHostRound, ri.fleetMainAdvanced)
	}))
	require.Equal(t, mainBefore, localTip(t, ri, "main"), "main did not move")
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Contains(t, st.LastError, remotesync.OwnCommitRefusedText)
	require.Contains(t, st.LastError, `"members-are-policy"`)
	require.False(t, st.LastErrorCommitRefused, "not a host refusing this copy's push: the member's leave/revoke advice does not apply")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, ok, err := svc.FileAt("main", bad, 1<<20)
		require.NoError(t, err)
		require.False(t, ok, "main never holds the invalid fact")
	}))
	require.True(t, st.LastErrorMainHeld)
	require.Equal(t, badCommit, st.LastErrorMainHeldCommit, "GET /fleet names the commit to drop")

	// The way out (POST /fleet/drop-commit): the branch is rebuilt on main
	// without the refused commit, keeping the good one, and main moves.
	out, err := m.DropFleetCommit(ctx, st.LastErrorMainHeldCommit)
	require.NoError(t, err)
	require.True(t, out.Published, out.PublishError)
	require.Equal(t, 1, out.Replayed)
	require.Equal(t, localTip(t, ri, ri.AgentBranch()), localTip(t, ri, "main"), "main moved to the rebuilt branch")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, ok, err := svc.FileAt("main", bad, 1<<20)
		require.NoError(t, err)
		require.False(t, ok, "the dropped commit's fact is gone")
		_, ok, err = svc.FileAt("main", good, 1<<20)
		require.NoError(t, err)
		require.True(t, ok, "the later good commit was kept")
	}))
	st, err = m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Empty(t, st.LastError, "main moved: the refusal is cleared")
	require.False(t, st.LastErrorMainHeld)

	_, err = m.DropFleetCommit(ctx, badCommit)
	_, code := fleetCode(t, err)
	require.Equal(t, "not_unpublished", code, "the commit is no longer on the branch")
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
		// Past the store's write cores, which refuse it at the source.
		_, err := storetest.CommitFiles(svc, f.mri.AgentBranch(), map[string]string{"kb/members/master/note.md": notPolicy}, "a bad note")
		require.NoError(t, err)
	}))
	roundOnPush(t, f.mri, f.master.fleetPushed)
	require.Equal(t, mainBefore, f.rev(t, "main"), "the forge's main did not move")
	require.False(t, f.onForgeMain(t, "kb/members/master/note.md"))
	st, err := f.master.FleetStatus(ctx)
	require.NoError(t, err)
	require.Contains(t, st.LastError, `"members-are-policy"`)
	require.Contains(t, st.LastError, remotesync.OwnCommitRefusedText)

	other := fact.RenderFleetMaster("other")
	err = f.master.publishMarkerChange(ctx, f.mri, &other, "forged")
	require.ErrorIs(t, err, remotesync.ErrOwnCommitRefused)
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
		// Past the store's write cores, which refuse it at the source.
		_, err := storetest.CommitFiles(svc, ari.AgentBranch(), map[string]string{"kb/members/alpha/note.md": notPolicy}, "a bad note")
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
