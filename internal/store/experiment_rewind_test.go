package store

// An experiment left open while the agent branch is rewound onto an upstream
// that scrubbed content (a force-pushed origin that removed a fact or a
// secret). exp/* branches are never rebased by the rewind itself, so before
// this fix the experiment still held the scrubbed commit and CommitExperiment
// (or a sync followed by a commit) merged it back onto the agent branch.

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

const scrubbedPath = "kb/secret.md"

// expRwSetup: an agent branch at c1 (= main = watermark).
func expRwSetup(t *testing.T) (*Service, plumbing.Hash) {
	t.Helper()
	svc, err := Open(t.TempDir() + "/k.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	c1 := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1"))
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), c1)))
	require.NoError(t, svc.rh.writeAgentBase("agent/test", c1))
	return svc, c1
}

// expRwPushSecret writes the secret on the agent branch and makes it upstream
// (main = the secret commit, watermark too): pushed and consumed.
func expRwPushSecret(t *testing.T, svc *Service) plumbing.Hash {
	t.Helper()
	x := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", scrubbedPath, "S", "scrub me"))
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), x)))
	require.NoError(t, svc.rh.writeAgentBase("agent/test", x))
	return x
}

// expRwRewind force-moves main to a new commit on parent and runs the rewind
// reconcile of the agent branch.
func expRwRewind(t *testing.T, svc *Service, parent plumbing.Hash, path string) (plumbing.Hash, AgentReconcileResult) {
	t.Helper()
	u := upstreamCommit(t, svc, parent, path, "---\ntype: observation\n---\n# U\n\n"+path+"\n")
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), u)))
	res, err := svc.rh.reconcileAgentRebase(context.Background(), "agent/test", "main", StrategyLocalWins)
	require.NoError(t, err)
	// Each test gives the agent branch a write of its own first: a branch with
	// nothing of its own beyond the watermark is kept as it stands by a rewind
	// (the empty-walk shape), scrubbed content included, which is not the
	// shape under test here.
	require.Equal(t, ModeRebase, res.Mode, "setup: the rewind rebuilt the agent branch")
	require.False(t, hasFileAt(t, svc, "agent/test", scrubbedPath), "setup: the rewind scrubbed it")
	return u, res
}

// reaches reports whether commit h is in the history of branch's tip: a
// commit the branch reaches is pushed with it, blobs included, whatever the
// tip's tree says.
func reaches(t *testing.T, svc *Service, branch string, h plumbing.Hash) bool {
	t.Helper()
	found := false
	require.NoError(t, WalkHistory(svc.rh.Storer(), mustHeadHash(t, svc, branch), nil, func(c *object.Commit) {
		if c.Hash == h {
			found = true
		}
	}))
	return found
}

// Q3 (the #535 re-review probe), as a committed test: an experiment open
// across a scrubbing rewind must not bring the scrubbed content back when it
// is committed, nor carry the scrubbed commit into the agent branch's history.
func TestExperimentAcrossARewind_CommitKeepsTheScrub(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	x := expRwPushSecret(t, svc)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/e.md", "E", "exp")
	writeMergeFact(t, svc, "exp/e", "kb/e2.md", "E2", "exp two")
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")

	u1, _ := expRwRewind(t, svc, c1, "kb/u.md")

	_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.False(t, hasFileAt(t, svc, "agent/test", scrubbedPath), "the commit must not bring the scrubbed fact back")
	require.False(t, reaches(t, svc, "agent/test", x), "the scrubbed commit must not reach the agent branch's history (it would be pushed)")
	for _, p := range []string{"kb/e.md", "kb/e2.md", "kb/w.md", "kb/u.md", "kb/a.md"} {
		require.True(t, hasFileAt(t, svc, "agent/test", p), p)
	}

	// A later write and a second rewind keep it out too.
	writeMergeFact(t, svc, "agent/test", "kb/later.md", "L", "later")
	_, _ = expRwRewind(t, svc, u1, "kb/u2.md")
	require.False(t, hasFileAt(t, svc, "agent/test", scrubbedPath))
	for _, p := range []string{"kb/e.md", "kb/e2.md", "kb/w.md", "kb/later.md"} {
		require.True(t, hasFileAt(t, svc, "agent/test", p), p)
	}
}

// The sync variant: syncing the experiment after the rewind rebuilds it on
// the new agent branch without the scrubbed content, and the commit after it
// keeps the scrub.
func TestExperimentAcrossARewind_SyncKeepsTheScrub(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	x := expRwPushSecret(t, svc)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/e.md", "E", "exp")
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	expRwRewind(t, svc, c1, "kb/u.md")

	_, err = svc.Experiments().SyncExperiment(ctx, "e")
	require.NoError(t, err)
	require.False(t, hasFileAt(t, svc, "exp/e", scrubbedPath), "the sync must drop the scrubbed fact from the experiment")
	require.False(t, reaches(t, svc, "exp/e", x), "the experiment must no longer hold the scrubbed commit")
	for _, p := range []string{"kb/e.md", "kb/w.md", "kb/u.md"} {
		require.True(t, hasFileAt(t, svc, "exp/e", p), p)
	}
	exp, ok, err := svc.Experiments().GetExperiment(ctx, "e")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, mustHeadHash(t, svc, "agent/test").String(), exp.ForkCommit,
		"the fork point moves to where the experiment was rebuilt, so 'since the fork' shows only its own work")

	_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.False(t, hasFileAt(t, svc, "agent/test", scrubbedPath))
	require.False(t, reaches(t, svc, "agent/test", x))
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/e.md"))
}

// A sync BEFORE the rewind that fast-forwarded the experiment onto the agent
// branch (the experiment had no commit of its own yet) puts the agent's
// commits on the experiment's first-parent chain. They are not the
// experiment's own, and the scrubbed one among them stays out.
func TestExperimentAcrossARewind_FastForwardSyncBeforeTheRewind(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	x := expRwPushSecret(t, svc)
	r, err := svc.Experiments().SyncExperiment(ctx, "e")
	require.NoError(t, err)
	require.Equal(t, ModeFF, r.Mode, "setup: the sync fast-forwards")
	writeMergeFact(t, svc, "exp/e", "kb/e.md", "E", "exp")
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	expRwRewind(t, svc, c1, "kb/u.md")

	_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.False(t, hasFileAt(t, svc, "agent/test", scrubbedPath))
	require.False(t, reaches(t, svc, "agent/test", x))
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/e.md"))
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/w.md"))
}

// An experiment that holds nothing the rewind removed commits exactly as
// before: a merge commit whose second parent is the experiment's own tip.
func TestExperimentAcrossARewind_NoScrubbedOverlapCommitsNormally(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/e.md", "E", "exp")
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	expRwRewind(t, svc, c1, "kb/u.md")

	expTip := mustHeadHash(t, svc, "exp/e")
	res, err := svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode)
	mc, err := svc.rh.Repository().CommitObject(mustHeadHash(t, svc, "agent/test"))
	require.NoError(t, err)
	require.Len(t, mc.ParentHashes, 2)
	require.Equal(t, expTip, mc.ParentHashes[1], "the experiment's own commits are merged as they are, not rebuilt")
	for _, p := range []string{"kb/e.md", "kb/w.md", "kb/u.md"} {
		require.True(t, hasFileAt(t, svc, "agent/test", p), p)
	}
}

// The experiment EDITED the file the rewind scrubbed. That is its own work
// and a removal at once, so neither side is picked silently: commit refuses
// with the path, and the caller's resolution decides.
func TestExperimentAcrossARewind_EditOfAScrubbedFileIsAConflict(t *testing.T) {
	for _, tc := range []struct {
		side    ResolutionSide
		present bool
	}{{ResolveDst, false}, {ResolveSrc, true}} {
		t.Run(string(tc.side), func(t *testing.T) {
			ctx := context.Background()
			svc, c1 := expRwSetup(t)
			x := expRwPushSecret(t, svc)
			_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
			require.NoError(t, err)
			writeMergeFact(t, svc, "exp/e", scrubbedPath, "S", "edited in the experiment")
			writeMergeFact(t, svc, "exp/e", "kb/e.md", "E", "exp")
			writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
			expRwRewind(t, svc, c1, "kb/u.md")
			agentBefore := mustHeadHash(t, svc, "agent/test")
			expBefore := mustHeadHash(t, svc, "exp/e")

			_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
			var conflict *MergeConflictError
			require.True(t, errors.As(err, &conflict), "want a conflict, got %v", err)
			require.Equal(t, []string{scrubbedPath}, conflict.Paths)
			require.True(t, conflict.Rewound)
			require.Equal(t, x.String(), conflict.BaseCommit, "the base is where the experiment met the old branch")
			require.Equal(t, agentBefore.String(), conflict.DstCommit)
			require.Equal(t, expBefore.String(), conflict.SrcCommit)
			require.Equal(t, agentBefore, mustHeadHash(t, svc, "agent/test"), "a refusal changes nothing")
			require.Equal(t, expBefore, mustHeadHash(t, svc, "exp/e"), "a refusal changes nothing")

			// A sync cannot settle it either: it would drop the experiment's
			// own edit for good, or bring the scrubbed file back.
			_, err = svc.Experiments().SyncExperiment(ctx, "e")
			require.True(t, errors.As(err, &conflict), "sync: want a conflict, got %v", err)
			require.Equal(t, expBefore, mustHeadHash(t, svc, "exp/e"))

			_, err = svc.Experiments().CommitExperiment(ctx, "e", map[string]Resolution{scrubbedPath: {Side: tc.side}})
			require.NoError(t, err)
			require.Equal(t, tc.present, hasFileAt(t, svc, "agent/test", scrubbedPath))
			require.True(t, hasFileAt(t, svc, "agent/test", "kb/e.md"))
			require.False(t, reaches(t, svc, "agent/test", x))
		})
	}
}

// Two rewinds before the commit: the experiment's own work is still told
// apart from the scrubbed commit by the agent branch as it stood before the
// FIRST rewind.
func TestExperimentAcrossARewind_TwoRewindsBeforeTheCommit(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	x := expRwPushSecret(t, svc)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/e.md", "E", "exp")
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	u1, _ := expRwRewind(t, svc, c1, "kb/u.md")
	writeMergeFact(t, svc, "agent/test", "kb/w2.md", "W2", "w2")
	expRwRewind(t, svc, u1, "kb/u2.md")

	_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.False(t, hasFileAt(t, svc, "agent/test", scrubbedPath))
	require.False(t, reaches(t, svc, "agent/test", x))
	for _, p := range []string{"kb/e.md", "kb/w.md", "kb/w2.md", "kb/u2.md"} {
		require.True(t, hasFileAt(t, svc, "agent/test", p), p)
	}
}

// Rolling an experiment back clears what the rewind recorded for it, so a
// new experiment of the same name starts clean.
func TestExperimentAcrossARewind_RollbackClearsTheRecord(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	expRwPushSecret(t, svc)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	expRwRewind(t, svc, c1, "kb/u.md")
	_, err = svc.rh.Storer().Reference(plumbing.ReferenceName("refs/knomit/experiment-rewound/e"))
	require.NoError(t, err, "setup: the rewind recorded the experiment")
	require.NoError(t, svc.Experiments().RollbackExperiment(ctx, "e"))
	_, err = svc.rh.Storer().Reference(plumbing.ReferenceName("refs/knomit/experiment-rewound/e"))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}
