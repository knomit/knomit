package store

// The #538 review's probes as committed tests: the rebuild is ONE commit from
// the three-way merge a plain commit would make (base = where the experiment
// last met the old agent branch), so what a sync merge settled stays settled.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func expRwRead(t *testing.T, svc *Service, branch, path string) string {
	t.Helper()
	c, err := svc.rh.Repository().CommitObject(mustHeadHash(t, svc, branch))
	require.NoError(t, err)
	f, err := c.File(path)
	require.NoError(t, err)
	s, err := f.Contents()
	require.NoError(t, err)
	return s
}

// P2: a path a parent-wins sync settled before the rewind is not raised again
// by the rebuild, and the agent's version stays.
func TestExperimentAcrossARewind_SyncSettledPathStaysSettled(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	x := expRwPushSecret(t, svc)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/a.md", "A", "exp edit")
	writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "agent edit")
	r, err := svc.Experiments().SyncExperiment(ctx, "e")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, r.Mode, "setup: a real sync merge")
	require.Contains(t, expRwRead(t, svc, "exp/e", "kb/a.md"), "agent edit", "setup: the sync kept the parent's version")
	writeMergeFact(t, svc, "exp/e", "kb/e2.md", "E2", "after sync")
	expRwRewind(t, svc, c1, "kb/u.md")

	_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err, "the sync already settled kb/a.md")
	require.Contains(t, expRwRead(t, svc, "agent/test", "kb/a.md"), "agent edit")
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/e2.md"))
	require.False(t, hasFileAt(t, svc, "agent/test", scrubbedPath))
	require.False(t, reaches(t, svc, "agent/test", x))
}

// P2b: since the path does not conflict, an "ours" for it is refused as a
// resolution of nothing, rather than landing the experiment's pre-sync
// version (which the experiment no longer held) over the agent's edit.
func TestExperimentAcrossARewind_OursCannotRevertASyncSettledPath(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	expRwPushSecret(t, svc)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/a.md", "A", "exp edit")
	writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "agent edit")
	_, err = svc.Experiments().SyncExperiment(ctx, "e")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/e2.md", "E2", "after sync")
	expRwRewind(t, svc, c1, "kb/u.md")
	agentBefore := mustHeadHash(t, svc, "agent/test")

	_, err = svc.Experiments().CommitExperiment(ctx, "e", map[string]Resolution{"kb/a.md": {Side: ResolveSrc}})
	require.ErrorContains(t, err, "did not conflict")
	require.Equal(t, agentBefore, mustHeadHash(t, svc, "agent/test"))
	require.Contains(t, expRwRead(t, svc, "agent/test", "kb/a.md"), "agent edit")
}

// P6: the secret reached the experiment through a NON-FF sync merge. It stays
// out, and the experiment's commits from both sides of the sync land.
func TestExperimentAcrossARewind_SecretThroughASyncMerge(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/e1.md", "E1", "before sync")
	x := expRwPushSecret(t, svc)
	r, err := svc.Experiments().SyncExperiment(ctx, "e")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, r.Mode, "setup: a real sync merge")
	writeMergeFact(t, svc, "exp/e", "kb/e2.md", "E2", "after sync")
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	expRwRewind(t, svc, c1, "kb/u.md")

	_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.False(t, hasFileAt(t, svc, "agent/test", scrubbedPath))
	require.False(t, reaches(t, svc, "agent/test", x))
	for _, p := range []string{"kb/e1.md", "kb/e2.md", "kb/w.md", "kb/u.md"} {
		require.True(t, hasFileAt(t, svc, "agent/test", p), p)
	}
}

// The rebuild is one single-parent commit on the agent branch's tip, and its
// message says the experiment's per-commit history was squashed.
func TestExperimentAcrossARewind_RebuildIsOneSquashedCommit(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	expRwPushSecret(t, svc)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", "kb/e.md", "E", "exp")
	writeMergeFact(t, svc, "exp/e", "kb/e2.md", "E2", "exp two")
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	expRwRewind(t, svc, c1, "kb/u.md")
	parentTip := mustHeadHash(t, svc, "agent/test")

	r, err := svc.Experiments().SyncExperiment(ctx, "e")
	require.NoError(t, err)
	require.Equal(t, ModeRebase, r.Mode)
	require.Equal(t, 2, r.NumReplayed)
	tip, err := svc.rh.Repository().CommitObject(mustHeadHash(t, svc, "exp/e"))
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{parentTip}, tip.ParentHashes)
	require.Contains(t, tip.Message, "squashed")
	require.Contains(t, tip.Message, "per-commit history is not kept")
}

// N1 (P3): an experiment whose only change was to a file the rewind removed,
// resolved to the agent's side, adds nothing: no empty commit lands.
func TestExperimentAcrossARewind_NoEmptyCommit(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	expRwPushSecret(t, svc)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/e", scrubbedPath, "S", "edited")
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	expRwRewind(t, svc, c1, "kb/u.md")
	before := mustHeadHash(t, svc, "agent/test")

	res, err := svc.Experiments().CommitExperiment(ctx, "e", map[string]Resolution{scrubbedPath: {Side: ResolveDst}})
	require.NoError(t, err)
	require.Equal(t, ModeNoop, res.Mode)
	require.Equal(t, before, mustHeadHash(t, svc, "agent/test"), "nothing to add, so no commit")
	require.False(t, hasFileAt(t, svc, "agent/test", scrubbedPath))
}

// SF1 (P5): a rewind that arrives while OpenExperiment is between its fork
// and its experiments row waits for the row, so it records the experiment.
func TestExperimentAcrossARewind_OpenRacingTheRewind(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	x := expRwPushSecret(t, svc)
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	u := upstreamCommit(t, svc, c1, "kb/u.md", "---\ntype: observation\n---\n# U\n\nu\n")
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), u)))

	done := make(chan error, 1)
	hooked := false
	testHookOpenExperimentForked = func() {
		hooked = true
		// The rewind starts now, between the fork and the row. Give it the
		// time to finish if nothing holds it back.
		go func() {
			_, err := svc.rh.reconcileAgentRebase(context.Background(), "agent/test", "main", StrategyLocalWins)
			done <- err
		}()
		select {
		case err := <-done:
			done <- err
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Cleanup(func() { testHookOpenExperimentForked = nil })
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	testHookOpenExperimentForked = nil
	require.True(t, hooked, "OpenExperiment must run the hook between the fork and the row")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the rewind never finished")
	}
	require.False(t, hasFileAt(t, svc, "agent/test", scrubbedPath), "setup: the rewind scrubbed it")

	writeMergeFact(t, svc, "exp/e", "kb/e.md", "E", "exp")
	_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.False(t, hasFileAt(t, svc, "agent/test", scrubbedPath))
	require.False(t, reaches(t, svc, "agent/test", x))
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/e.md"))
}

// N2: opening an experiment clears a rewind record left under its name, so
// the new experiment commits as a plain merge.
func TestExperimentAcrossARewind_OpenClearsAStaleRecord(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	stale := upstreamCommit(t, svc, c1, "kb/stale.md", "---\ntype: observation\n---\n# S\n\ns\n")
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(experimentRewoundRefName("e"), stale)))

	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	_, err = svc.rh.Storer().Reference(experimentRewoundRefName("e"))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
	writeMergeFact(t, svc, "exp/e", "kb/e.md", "E", "exp")
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	res, err := svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode, "a plain merge, not a rebuild")
}

// N4: the experiment's pipeline watermarks move with the rebuild: one at the
// old tip to the rebuilt tip, any other to the new fork point.
func TestExperimentAcrossARewind_PipelineWatermarksFollow(t *testing.T) {
	ctx := context.Background()
	svc, c1 := expRwSetup(t)
	expRwPushSecret(t, svc)
	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	mid := writeMergeFact(t, svc, "exp/e", "kb/e.md", "E", "exp")
	writeMergeFact(t, svc, "exp/e", "kb/e2.md", "E2", "exp two")
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", "exp/e", mustHeadHash(t, svc, "exp/e").String()))
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "hypothesize", "exp/e", mid))
	writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "w")
	expRwRewind(t, svc, c1, "kb/u.md")
	parentTip := mustHeadHash(t, svc, "agent/test")

	_, err = svc.Experiments().SyncExperiment(ctx, "e")
	require.NoError(t, err)
	review, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", "exp/e")
	require.NoError(t, err)
	require.Equal(t, mustHeadHash(t, svc, "exp/e").String(), review)
	hypo, err := svc.Pipeline().GetPipelineWatermark(ctx, "hypothesize", "exp/e")
	require.NoError(t, err)
	require.Equal(t, parentTip.String(), hypo)
	require.False(t, strings.EqualFold(review, hypo))
}
