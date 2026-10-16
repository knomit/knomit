package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// hasFileAt reports whether path is in the tree of branch's tip.
func hasFileAt(t *testing.T, svc *Service, branch, path string) bool {
	t.Helper()
	c, err := svc.rh.repo.CommitObject(mustHeadHash(t, svc, branch))
	require.NoError(t, err)
	tree, err := c.Tree()
	require.NoError(t, err)
	_, err = tree.File(path)
	return err == nil
}

// PR #531 review B1, kept as a regression test. An ORDINARY repo with an
// origin, no fleet: the agent branch was fast-forwarded onto main m1 (the
// watermark is m1); an experiment opened before that is committed as a MERGE
// commit (the agent moved since it opened), which deletes the experiment, so
// the merge is the only reference to its work. Origin main is then rewound to
// a commit that lacks m1. Nothing of the agent's own is a plain commit beyond
// the watermark, so the replay's walk is empty: the branch must stay where it
// is. Moving it onto the rewound main ("only merges beyond upstream") drops
// the experiment's committed knowledge.
// SABOTAGE: in replayOntoUpstream, move the branch onto upstream when the
// walk is empty and the two diverged on shared history (the dropped "shape
// 3") -> kb/exp.md is gone -> red.
func TestReconcileAgentRebase_ExperimentMergeSurvivesARewind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(ctx, map[string]string{}, "agent/test"))

	c1 := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1"))
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), c1)))

	// The experiment opens at c1.
	_, err = svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/exp.md", "Exp", "experiment knowledge")

	// Origin main gains m1; the steady reconcile fast-forwards the agent
	// branch onto it and sets the watermark.
	m1 := upstreamCommit(t, svc, c1, "kb/m1.md", "---\ntype: observation\n---\n# M1\n\nm1\n")
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), m1)))
	r, err := svc.rh.reconcileAgentMerge(ctx, "agent/test", "main", StrategyLocalWins)
	require.NoError(t, err)
	require.Equal(t, ModeFF, r.Mode, "setup: the agent branch fast-forwards onto main")

	// The experiment commit: the agent moved, so it is a merge commit.
	cr, err := svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, cr.Mode, "setup: the experiment commit is a merge")
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/exp.md"), "setup: the experiment's content is on the agent branch")

	// The rewind: origin main is force-pushed to u, on c1, lacking m1.
	u := upstreamCommit(t, svc, c1, "kb/u.md", "---\ntype: observation\n---\n# U\n\nu\n")
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), u)))
	_, err = svc.rh.reconcileAgentRebase(ctx, "agent/test", "main", StrategyLocalWins)
	require.NoError(t, err)
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/exp.md"), "the experiment's committed knowledge must survive the rewind")
}
