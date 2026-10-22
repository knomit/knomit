package store

import (
	"context"
	"testing"

	"knomit/internal/store/model"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/store/merge"
)

// AppendTrailersToParagraph joins the trace to a merge commit's EXISTING
// trailer paragraph (its conflict record), so TrailerValues still reads the
// record and TrailerValue reads the trace; with no trailer paragraph it opens
// one. Sabotage: AppendTrailers instead (red: the record is no longer the
// last paragraph, TrailerValues reads nothing).
func TestAppendTrailersToParagraph_JoinsTheRecord(t *testing.T) {
	tr := model.Trailers{Trace: "task-1", Run: "run-" + "0123456789abcdef0123456789abcdef"}
	withRecord := merge.AppendTrailerLines("merge: exp/x into agent/a (refuse)", []string{merge.TrailerMerge + ": kb/a.md src", merge.TrailerMerge + ": kb/b.md dst"})
	got := model.AppendTrailersToParagraph(withRecord, tr)
	require.Equal(t, "merge: exp/x into agent/a (refuse)\n\n"+
		"Knomit-Merge: kb/a.md src\nKnomit-Merge: kb/b.md dst\n"+
		"Knomit-Trace: task-1\nKnomit-Run: run-0123456789abcdef0123456789abcdef\n", got)
	require.Len(t, merge.TrailerValues(got, merge.TrailerMerge), 2, "the record is still read")
	require.Equal(t, "task-1", model.TrailerValue(got, model.TrailerTrace))

	plain := model.AppendTrailersToParagraph("merge: exp/x into agent/a (refuse)", tr)
	require.Equal(t, "merge: exp/x into agent/a (refuse)\n\nKnomit-Trace: task-1\nKnomit-Run: run-0123456789abcdef0123456789abcdef\n", plain)
	require.Equal(t, "merge: m", model.AppendTrailersToParagraph("merge: m", model.Trailers{}), "no set: unchanged")
}

// Only an experiment's OWN merge reads the ctx trace: a plain merge
// (agent sync, reconcile, peer merge) under a ctx that carries one stays a
// transport commit with no trailer; CommitExperiment under the same ctx is
// stamped. Sabotage: read the ctx trace in mergeIntoBranchLockedOpts for every
// merge (red: the plain merge is stamped).
func TestExperimentTrace_OnlyTheExperimentsOwnMerge(t *testing.T) {
	svc := newExperimentTestStore(t)
	ctx, err := model.WithAgentTrace(context.Background(), model.Trailers{Trace: "task-2"})
	require.NoError(t, err)
	msgOf := func(branch string) string {
		h, err := svc.HeadCommit(ctx, branch)
		require.NoError(t, err)
		info, err := svc.FactsRepo().CommitInfo(ctx, plumbing.NewHash(h))
		require.NoError(t, err)
		return info.Message
	}

	// A plain merge between two branches, under a traced ctx.
	require.NoError(t, svc.CreateBranch(context.Background(), "agent/peer", testAgentBranch))
	writeMergeFact(t, svc, "agent/peer", "kb/peer.md", "peer", "body")
	writeMergeFact(t, svc, testAgentBranch, "kb/own.md", "own", "body")
	require.NoError(t, svc.mr.MergeBranch(ctx, "agent/peer", testAgentBranch, merge.StrategyLocalWins))
	plain := msgOf(testAgentBranch)
	require.Contains(t, plain, "merge: agent/peer into", "fixture: a merge commit was written")
	require.Empty(t, model.TrailerValue(plain, model.TrailerTrace), "a transport merge is never stamped: %q", plain)

	// The experiment's own merge, under the same ctx.
	_, err = svc.Merge().OpenExperiment(context.Background(), "own", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/own", "kb/exp.md", "exp", "body")
	writeMergeFact(t, svc, testAgentBranch, "kb/own-2.md", "own 2", "body")
	res, err := svc.Merge().CommitExperiment(ctx, "own", nil)
	require.NoError(t, err)
	require.Equal(t, merge.ModeMerge, res.Mode)
	require.Equal(t, "task-2", model.TrailerValue(msgOf(testAgentBranch), model.TrailerTrace))
}
