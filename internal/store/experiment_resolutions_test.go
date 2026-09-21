package store

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// Resolutions settle a refused commit INSIDE the same three-way merge. The
// tests below pin the three things that make that safe rather than merely
// convenient: an unresolved path still refuses, a resolution that adjudicated
// nothing is an error rather than a no-op, and everything the caller did not
// mention merges exactly as it would have.

// TestCommitExperiment_RefusalCarriesTheThreeCommits: a refusal that names
// paths without naming where to read them leaves the caller unable to see the
// versions that made each path a conflict. The three must be the REAL tips and
// merge base, not merely non-empty.
func TestCommitExperiment_RefusalCarriesTheThreeCommits(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	exp, err := svc.Experiments().OpenExperiment(ctx, "three", "", testAgentBranch)
	require.NoError(t, err)
	forkCommit := exp.ForkCommit

	writeMergeFact(t, svc, "exp/three", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	expHead, err := svc.Branches().HeadCommit(ctx, "exp/three")
	require.NoError(t, err)
	agentHead, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)

	_, err = svc.Experiments().CommitExperiment(ctx, "three", nil)
	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict)

	require.Equal(t, expHead, conflict.SrcCommit,
		"SrcCommit must be the experiment tip — that is where its version of the fact is read")
	require.Equal(t, agentHead, conflict.DstCommit,
		"DstCommit must be the parent tip")
	// Asserted against the COMPUTED merge base, not against the recorded fork
	// commit. They happen to be equal here, and pinning fork_commit would let
	// an implementation that reads the record keep passing — which is exactly
	// the bug the next test catches.
	base := mergeBaseOf(t, svc, "exp/three", testAgentBranch)
	require.Equal(t, base, conflict.BaseCommit)
	require.Equal(t, forkCommit, base, "with no sync in between, the two coincide")
}

// TestCommitExperiment_BaseIsTheMergeBaseNotTheForkCommit: a sync merges the
// parent INTO the experiment, which moves the merge base forward. The recorded
// fork_commit stays where it was, so reporting it would hand the agent a base
// version the merge did not use — authoritative-looking and wrong.
func TestCommitExperiment_BaseIsTheMergeBaseNotTheForkCommit(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	exp, err := svc.Experiments().OpenExperiment(ctx, "resynced", "", testAgentBranch)
	require.NoError(t, err)
	forkCommit := exp.ForkCommit

	// A first round of divergence, settled by a sync.
	writeMergeFact(t, svc, "exp/resynced", "kb/one.md", "one", "experiment one")
	writeMergeFact(t, svc, testAgentBranch, "kb/one.md", "one", "agent one")
	_, err = svc.Experiments().SyncExperiment(ctx, "resynced")
	require.NoError(t, err)

	// Now a fresh conflict on a different path.
	writeMergeFact(t, svc, "exp/resynced", "kb/two.md", "two", "experiment two")
	writeMergeFact(t, svc, testAgentBranch, "kb/two.md", "two", "agent two")

	_, err = svc.Experiments().CommitExperiment(ctx, "resynced", nil)
	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict)

	base := mergeBaseOf(t, svc, "exp/resynced", testAgentBranch)
	require.Equal(t, base, conflict.BaseCommit, "the refusal must quote the real merge base")
	require.NotEqual(t, forkCommit, conflict.BaseCommit,
		"after a sync the merge base has moved past the recorded fork commit — "+
			"reporting fork_commit would send the reader to a version the merge never used")
}

// TestCommitExperiment_DualAddHasNoBaseVersion: both sides added the same
// path, so there is nothing to read at the base. It must be reported as such:
// knomit_explain for a path absent at a commit does not error, it answers with
// the nearest earlier version, so an agent sent to read a base that does not
// exist is shown a different fact and cannot tell.
func TestCommitExperiment_DualAddHasNoBaseVersion(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "dualadd", "", testAgentBranch)
	require.NoError(t, err)
	// Neither side had kb/new.md at the fork point.
	writeMergeFact(t, svc, "exp/dualadd", "kb/new.md", "new", "experiment version")
	writeMergeFact(t, svc, testAgentBranch, "kb/new.md", "new", "agent version")
	// Plus an ordinary modify conflict, which DOES have a base.
	writeMergeFact(t, svc, "exp/dualadd", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	_, err = svc.Experiments().CommitExperiment(ctx, "dualadd", nil)
	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict)

	require.Equal(t, []string{"kb/base.md", "kb/new.md"}, conflict.Paths)
	require.Equal(t, []string{"kb/new.md"}, conflict.PathsWithoutBase,
		"only the dual add is baseless")
	require.False(t, conflict.HasBase("kb/new.md"))
	require.True(t, conflict.HasBase("kb/base.md"),
		"a modify conflict has a base version and must not be marked baseless")
}

// TestCommitExperiment_AllTheirsIsANoop: resolving every conflict to the
// parent's side makes the merged tree identical to the parent's, so no merge
// commit is written and the branch does not move — but the experiment is still
// gone. A caller told "merged into <parent>" would go looking for a commit
// that was never created.
func TestCommitExperiment_AllTheirsIsANoop(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "alltheirs", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/alltheirs", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	agentBefore, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)

	res, err := svc.Experiments().CommitExperiment(ctx, "alltheirs", map[string]Resolution{
		"kb/base.md": {Side: ResolveDst},
	})
	require.NoError(t, err)
	require.Equal(t, ModeNoop, res.Mode,
		"an all-theirs resolution produces a tree identical to the parent's")

	agentAfter, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, agentBefore, agentAfter, "no merge commit, so the parent must not move")

	_, ok, err := svc.Experiments().GetExperiment(ctx, "alltheirs")
	require.NoError(t, err)
	require.False(t, ok, "the experiment is still deleted — the action did happen")
}

// TestCommitExperiment_LeftoverResolutionOnACleanMerge: nothing conflicts, so
// there is no merge walk to notice the stray resolution. The check has to run
// before the fast-forward and noop returns or it never runs at all.
func TestCommitExperiment_LeftoverResolutionOnACleanMerge(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "clean", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/clean", "kb/exp-only.md", "exp only", "body")

	agentBefore, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)

	_, err = svc.Experiments().CommitExperiment(ctx, "clean", map[string]Resolution{
		"kb/exp-only.md": {Side: ResolveSrc},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "did not conflict")
	require.Contains(t, err.Error(), "kb/exp-only.md")

	agentAfter, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, agentBefore, agentAfter,
		"the rejection must happen BEFORE the fast-forward moves the branch")

	_, ok, err := svc.Experiments().GetExperiment(ctx, "clean")
	require.NoError(t, err)
	require.True(t, ok, "and the experiment must survive a rejected commit")
}

// TestCommitExperiment_ResolutionForDstOnlyPathIsRefused: a path only the
// PARENT changed never appears in DiffTree(base, src), so the merge walk
// cannot see it. Only an up-front check against the computed conflict set
// catches a resolution naming it.
func TestCommitExperiment_ResolutionForDstOnlyPathIsRefused(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "dstonly", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/dstonly", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")
	// Changed on the parent ONLY: not a conflict, and invisible to the walk.
	writeMergeFact(t, svc, testAgentBranch, "kb/agent-only.md", "agent only", "body")

	_, err = svc.Experiments().CommitExperiment(ctx, "dstonly", map[string]Resolution{
		"kb/base.md":       {Side: ResolveSrc},
		"kb/agent-only.md": {Side: ResolveSrc},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "did not conflict")
	require.Contains(t, err.Error(), "kb/agent-only.md")
}

// mergeBaseOf returns the real merge base of two branches.
func mergeBaseOf(t *testing.T, svc *Service, a, b string) string {
	t.Helper()
	ctx := context.Background()
	aHead, err := svc.Branches().HeadCommit(ctx, a)
	require.NoError(t, err)
	bHead, err := svc.Branches().HeadCommit(ctx, b)
	require.NoError(t, err)
	ac, err := svc.rh.repo.CommitObject(plumbing.NewHash(aHead))
	require.NoError(t, err)
	bc, err := svc.rh.repo.CommitObject(plumbing.NewHash(bHead))
	require.NoError(t, err)
	bases, err := bc.MergeBase(ac)
	require.NoError(t, err)
	require.NotEmpty(t, bases)
	return bases[0].Hash.String()
}

// TestCommitExperiment_ResolveOurs_TakesTheExperimentVersion.
// "ours" is translated to ResolveSrc at the tool boundary; commit merges
// exp -> parent, so src IS the experiment.
func TestCommitExperiment_ResolveOurs_TakesTheExperimentVersion(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "ours", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/ours", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	_, err = svc.Experiments().CommitExperiment(ctx, "ours", map[string]Resolution{
		"kb/base.md": {Side: ResolveSrc},
	})
	require.NoError(t, err)

	got := readExperimentFact(t, svc, testAgentBranch, "kb/base.md")
	require.Contains(t, got, "experiment rewrite")
	require.NotContains(t, got, "agent rewrite",
		"ResolveSrc must REPLACE the parent's version, not merge into it")

	_, ok, err := svc.Experiments().GetExperiment(ctx, "ours")
	require.NoError(t, err)
	require.False(t, ok, "a resolved commit still deletes the experiment")
}

// TestCommitExperiment_ResolveTheirs_KeepsTheParentVersion.
func TestCommitExperiment_ResolveTheirs_KeepsTheParentVersion(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "theirs", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/theirs", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	_, err = svc.Experiments().CommitExperiment(ctx, "theirs", map[string]Resolution{
		"kb/base.md": {Side: ResolveDst},
	})
	require.NoError(t, err)

	got := readExperimentFact(t, svc, testAgentBranch, "kb/base.md")
	require.Contains(t, got, "agent rewrite",
		"ResolveDst keeps what the parent already had")
	require.NotContains(t, got, "experiment rewrite")
}

// TestCommitExperiment_ResolveBody_LandsContentFromNeitherSide is the case the
// other two cannot express: the merged text.
func TestCommitExperiment_ResolveBody_LandsContentFromNeitherSide(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "merged", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/merged", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	merged := "---\ntype: observation\nconfidence: 0.9\n---\n# base\n\nboth readings, reconciled\n"
	_, err = svc.Experiments().CommitExperiment(ctx, "merged", map[string]Resolution{
		"kb/base.md": {Body: []byte(merged)},
	})
	require.NoError(t, err)

	got := readExperimentFact(t, svc, testAgentBranch, "kb/base.md")
	require.Contains(t, got, "both readings, reconciled")
	require.NotContains(t, got, "experiment rewrite")
	require.NotContains(t, got, "agent rewrite")
}

// TestCommitExperiment_BodyWinsOverSide: an entry carrying both is not
// ambiguous by accident — Body is documented to win, and a caller that sends
// both gets the content it wrote.
func TestCommitExperiment_BodyWinsOverSide(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "both", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/both", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	_, err = svc.Experiments().CommitExperiment(ctx, "both", map[string]Resolution{
		"kb/base.md": {Side: ResolveDst, Body: []byte("---\ntype: observation\n---\n# base\n\nthe body\n")},
	})
	require.NoError(t, err)
	require.Contains(t, readExperimentFact(t, svc, testAgentBranch, "kb/base.md"), "the body")
}

// TestCommitExperiment_EmptyResolutionIsRefused: naming a path without naming
// an answer is not an adjudication, and defaulting a side for that caller is
// exactly the silent resolution StrategyRefuse exists to prevent.
func TestCommitExperiment_EmptyResolutionIsRefused(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "empty", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/empty", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	agentBefore, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)

	_, err = svc.Experiments().CommitExperiment(ctx, "empty", map[string]Resolution{
		"kb/base.md": {},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "neither a side nor a body")

	agentAfter, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, agentBefore, agentAfter, "a rejected resolution must move nothing")
}

// TestCommitExperiment_ResolutionForNonConflictingPathIsRefused: a caller that
// resolves a path which did not conflict believes the merge is something other
// than it is. Dropping it silently is how someone concludes they settled a
// path they never touched.
func TestCommitExperiment_ResolutionForNonConflictingPathIsRefused(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "extra", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/extra", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, "exp/extra", "kb/exp-only.md", "exp only", "body")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	agentBefore, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	commitsBefore := countRows(t, svc, `SELECT count(*) FROM branch_commits`)

	_, err = svc.Experiments().CommitExperiment(ctx, "extra", map[string]Resolution{
		"kb/base.md":     {Side: ResolveSrc},
		"kb/exp-only.md": {Side: ResolveSrc}, // added only on the experiment: no conflict
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "did not conflict")
	require.Contains(t, err.Error(), "kb/exp-only.md",
		"the error must name WHICH resolution adjudicated nothing")

	agentAfter, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, agentBefore, agentAfter)
	require.Equal(t, commitsBefore, countRows(t, svc, `SELECT count(*) FROM branch_commits`),
		"a rejected commit must append no branch_commits row")
}

// TestCommitExperiment_PartialResolutionsRefuseOnlyTheRest: the retry loop has
// to converge. A caller that settles two of three paths is told about the
// third ALONE — being re-told about work already done reads as "my
// resolutions were ignored".
func TestCommitExperiment_PartialResolutionsRefuseOnlyTheRest(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "partial", "", testAgentBranch)
	require.NoError(t, err)
	for _, p := range []string{"kb/a.md", "kb/b.md", "kb/c.md"} {
		writeMergeFact(t, svc, "exp/partial", p, "base", "experiment rewrite")
		writeMergeFact(t, svc, testAgentBranch, p, "base", "agent rewrite")
	}

	expBefore, err := svc.Branches().HeadCommit(ctx, "exp/partial")
	require.NoError(t, err)
	agentBefore, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)

	_, err = svc.Experiments().CommitExperiment(ctx, "partial", map[string]Resolution{
		"kb/a.md": {Side: ResolveSrc},
		"kb/b.md": {Side: ResolveDst},
	})
	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"kb/c.md"}, conflict.Paths,
		"only the unresolved path may be reported")

	// Byte-identical tips: a partial resolution is still a refusal, and a
	// refusal writes nothing at all.
	expAfter, err := svc.Branches().HeadCommit(ctx, "exp/partial")
	require.NoError(t, err)
	agentAfter, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, expBefore, expAfter)
	require.Equal(t, agentBefore, agentAfter)

	// And the retry that settles the last one goes through.
	_, err = svc.Experiments().CommitExperiment(ctx, "partial", map[string]Resolution{
		"kb/a.md": {Side: ResolveSrc},
		"kb/b.md": {Side: ResolveDst},
		"kb/c.md": {Side: ResolveSrc},
	})
	require.NoError(t, err)
	require.Contains(t, readExperimentFact(t, svc, testAgentBranch, "kb/a.md"), "experiment rewrite")
	require.Contains(t, readExperimentFact(t, svc, testAgentBranch, "kb/b.md"), "agent rewrite")
	require.Contains(t, readExperimentFact(t, svc, testAgentBranch, "kb/c.md"), "experiment rewrite")
}

// TestCommitExperiment_EverythingElseStillMerges: resolutions are per-path and
// must not turn the merge into "only what was resolved". Both sides' untouched
// work has to land.
func TestCommitExperiment_EverythingElseStillMerges(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "rest", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/rest", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, "exp/rest", "kb/exp-only.md", "exp only", "from the experiment")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/agent-only.md", "agent only", "from the agent branch")

	_, err = svc.Experiments().CommitExperiment(ctx, "rest", map[string]Resolution{
		"kb/base.md": {Side: ResolveSrc},
	})
	require.NoError(t, err)

	require.Contains(t, readExperimentFact(t, svc, testAgentBranch, "kb/exp-only.md"), "from the experiment",
		"the experiment's non-conflicting work must still merge")
	require.Contains(t, readExperimentFact(t, svc, testAgentBranch, "kb/agent-only.md"), "from the agent branch",
		"the parent's own work must survive the merge")
}

// TestCommitExperiment_NewConflictAfterParentMovesIsListedAlone: between a
// refusal and its retry the parent keeps moving. A path that becomes
// conflicting AFTER the caller chose its resolutions must be reported on its
// own, and the already-settled path must not reappear.
func TestCommitExperiment_NewConflictAfterParentMovesIsListedAlone(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "moving", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/moving", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, "exp/moving", "kb/second.md", "second", "experiment second")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	// First refusal names one path.
	_, err = svc.Experiments().CommitExperiment(ctx, "moving", nil)
	var first *MergeConflictError
	require.ErrorAs(t, err, &first)
	require.Equal(t, []string{"kb/base.md"}, first.Paths)

	// The parent moves under us, colliding with a second path.
	writeMergeFact(t, svc, testAgentBranch, "kb/second.md", "second", "agent second")

	_, err = svc.Experiments().CommitExperiment(ctx, "moving", map[string]Resolution{
		"kb/base.md": {Side: ResolveSrc},
	})
	var second *MergeConflictError
	require.ErrorAs(t, err, &second)
	require.Equal(t, []string{"kb/second.md"}, second.Paths,
		"only the NEW conflict — the resolved path must not be re-reported")
	require.NotEqual(t, first.DstCommit, second.DstCommit,
		"the parent tip moved, so the refusal must quote the newer one")
}

// TestCommitExperiment_ResolutionsRejectedForResolvingStrategies: resolutions
// are meaningless to a strategy that already picks a side, and accepting them
// there would imply an adjudication the caller never got.
func TestCommitExperiment_ResolutionsRejectedForResolvingStrategies(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "strat", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/strat", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	_, err = svc.rh.mergeIntoBranchResolved(ctx, "exp/strat", testAgentBranch, StrategyLocalWins,
		map[string]Resolution{"kb/base.md": {Side: ResolveSrc}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolutions are only valid with")
}

// TestResolvedCommit_IsSigned: the merge commit a resolution produces is a
// commit like any other and must carry a signature.
//
// The signer is proved LIVE on an ordinary write first. Without that
// precondition an empty signature on the merge commit is ambiguous — it could
// mean the merge path skips signing, or simply that no signer was installed in
// the fixture — and the test would pass for the wrong reason the day signing
// broke.
func TestResolvedCommit_IsSigned(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)
	svc.SetSigner(newTestSigner(t))

	guardHash := writeMergeFact(t, svc, testAgentBranch, "kb/guard.md", "guard", "body")
	guard, err := svc.rh.repo.CommitObject(plumbing.NewHash(guardHash))
	require.NoError(t, err)
	require.NotEmpty(t, guard.PGPSignature, "precondition: the test signer is installed and live")

	_, err = svc.Experiments().OpenExperiment(ctx, "signedres", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/signedres", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	// ResolveSrc, not ResolveDst: taking the parent's side would produce a
	// tree identical to dst and therefore NO merge commit to inspect.
	res, err := svc.Experiments().CommitExperiment(ctx, "signedres", map[string]Resolution{
		"kb/base.md": {Side: ResolveSrc},
	})
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode, "precondition: a real merge commit was synthesized")

	hash, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	commit, err := svc.rh.repo.CommitObject(plumbing.NewHash(hash))
	require.NoError(t, err)
	require.NotEmpty(t, commit.PGPSignature, "the merge commit a resolution produces must be signed")
}

// TestResolvedCommit_ReachesNotifyCommitOnDst: every branch-ref mutation must
// call notifyCommit while holding the DST branch lock
// (kb/invariants/store/notify-commit-single-chokepoint). A resolved commit
// moves the parent ref, so it must go through that chokepoint too — a merge
// that moved the ref without it would leave SQL stale relative to git.
//
// Asserted through the chokepoint's OBSERVABLE effect on dst: the commit log
// gains the new tip. Asserting the lock itself is not possible from here; the
// branch_commits row is what notifyCommit writes, and its absence is exactly
// the staleness the invariant exists to prevent.
func TestResolvedCommit_ReachesNotifyCommitOnDst(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "notify", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/notify", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	res, err := svc.Experiments().CommitExperiment(ctx, "notify", map[string]Resolution{
		"kb/base.md": {Side: ResolveSrc},
	})
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode)

	newTip, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, newTip, res.NewTip, "the reported tip is the one the ref now holds")

	// The commit log row for the NEW tip on dst is notifyCommit's work.
	logged := countRows(t, svc, `SELECT count(*) FROM branch_commits bc
		JOIN branches b ON b.id = bc.branch_id
		WHERE b.name = ? AND bc.commit_hash = ?`, testAgentBranch, newTip)
	require.Equal(t, 1, logged,
		"the merge commit must be in dst's commit log — that row is what notifyCommit appends")
}

// The three conflict arms are not one code path. reviewer2 probed all four
// resolution shapes by hand and found them correct; these close the gap so the
// next change cannot quietly break Insert or Delete while Modify keeps passing.

// TestCommitExperiment_ResolveInsertArm: both sides ADDED the same path. The
// Insert arm is the one the resolving strategies treat as non-conflicting, so
// it is the easiest to get wrong — and it has no base version to fall back on.
func TestCommitExperiment_ResolveInsertArm(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  Resolution
		want string
		gone string
	}{
		{"ours takes the experiment's addition", Resolution{Side: ResolveSrc}, "experiment version", "agent version"},
		{"theirs keeps the parent's addition", Resolution{Side: ResolveDst}, "agent version", "experiment version"},
		{"body lands neither", Resolution{Body: []byte("---\ntype: observation\n---\n# new\n\nthird version\n")}, "third version", "agent version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc := newExperimentTestStore(t)

			_, err := svc.Experiments().OpenExperiment(ctx, "insertarm", "", testAgentBranch)
			require.NoError(t, err)
			// Neither side had kb/new.md at the fork point.
			writeMergeFact(t, svc, "exp/insertarm", "kb/new.md", "new", "experiment version")
			writeMergeFact(t, svc, testAgentBranch, "kb/new.md", "new", "agent version")

			_, err = svc.Experiments().CommitExperiment(ctx, "insertarm", map[string]Resolution{
				"kb/new.md": tc.res,
			})
			require.NoError(t, err)

			got := readExperimentFact(t, svc, testAgentBranch, "kb/new.md")
			require.Contains(t, got, tc.want)
			require.NotContains(t, got, tc.gone)
		})
	}
}

// TestCommitExperiment_ResolveDeleteArm: the experiment DELETED a path the
// parent modified. "ours" here means absence — the delete — which is the one
// case where taking the source's version is not a blob to write.
func TestCommitExperiment_ResolveDeleteArm(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	// Seed, fork, then delete on the experiment and modify on the parent.
	writeMergeFact(t, svc, testAgentBranch, "kb/doomed.md", "doomed", "original")
	_, err := svc.Experiments().OpenExperiment(ctx, "deletearm", "", testAgentBranch)
	require.NoError(t, err)
	_, err = svc.Facts().DeleteFact(ctx, "exp/deletearm", "kb/doomed.md", "retract on the experiment")
	require.NoError(t, err)
	writeMergeFact(t, svc, testAgentBranch, "kb/doomed.md", "doomed", "agent rewrite")

	// Unresolved, this refuses.
	_, err = svc.Experiments().CommitExperiment(ctx, "deletearm", nil)
	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"kb/doomed.md"}, conflict.Paths)
	require.True(t, conflict.HasBase("kb/doomed.md"),
		"a delete conflict requires a base version, so it is never baseless")

	// "ours" is the experiment's version, and the experiment's version is that
	// the fact is GONE.
	_, err = svc.Experiments().CommitExperiment(ctx, "deletearm", map[string]Resolution{
		"kb/doomed.md": {Side: ResolveSrc},
	})
	require.NoError(t, err)

	paths, err := svc.Facts().ListAll(ctx, testAgentBranch)
	require.NoError(t, err)
	require.NotContains(t, paths, "kb/doomed.md",
		"resolving a delete conflict to the experiment's side must delete the path")
}

// TestCommitExperiment_ResolveDeleteArm_TheirsKeepsTheFact is the other half:
// the parent's modification survives and the experiment's delete is discarded.
func TestCommitExperiment_ResolveDeleteArm_TheirsKeepsTheFact(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	writeMergeFact(t, svc, testAgentBranch, "kb/doomed.md", "doomed", "original")
	_, err := svc.Experiments().OpenExperiment(ctx, "keepit", "", testAgentBranch)
	require.NoError(t, err)
	_, err = svc.Facts().DeleteFact(ctx, "exp/keepit", "kb/doomed.md", "retract on the experiment")
	require.NoError(t, err)
	writeMergeFact(t, svc, testAgentBranch, "kb/doomed.md", "doomed", "agent rewrite")

	_, err = svc.Experiments().CommitExperiment(ctx, "keepit", map[string]Resolution{
		"kb/doomed.md": {Side: ResolveDst},
	})
	require.NoError(t, err)

	require.Contains(t, readExperimentFact(t, svc, testAgentBranch, "kb/doomed.md"), "agent rewrite")
}
