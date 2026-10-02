package store

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// appendTrailersToParagraph joins the trace to a merge commit's EXISTING
// trailer paragraph (its conflict record), so TrailerValues still reads the
// record and TrailerValue reads the trace; with no trailer paragraph it opens
// one. Sabotage: appendTrailers instead (red: the record is no longer the
// last paragraph, TrailerValues reads nothing).
func TestAppendTrailersToParagraph_JoinsTheRecord(t *testing.T) {
	tr := Trailers{Trace: "task-1", Run: "run-" + "0123456789abcdef0123456789abcdef"}
	withRecord := appendTrailerLines("merge: exp/x into agent/a (refuse)", []string{TrailerMerge + ": kb/a.md src", TrailerMerge + ": kb/b.md dst"})
	got := appendTrailersToParagraph(withRecord, tr)
	require.Equal(t, "merge: exp/x into agent/a (refuse)\n\n"+
		"Knomit-Merge: kb/a.md src\nKnomit-Merge: kb/b.md dst\n"+
		"Knomit-Trace: task-1\nKnomit-Run: run-0123456789abcdef0123456789abcdef\n", got)
	require.Len(t, TrailerValues(got, TrailerMerge), 2, "the record is still read")
	require.Equal(t, "task-1", TrailerValue(got, TrailerTrace))

	plain := appendTrailersToParagraph("merge: exp/x into agent/a (refuse)", tr)
	require.Equal(t, "merge: exp/x into agent/a (refuse)\n\nKnomit-Trace: task-1\nKnomit-Run: run-0123456789abcdef0123456789abcdef\n", plain)
	require.Equal(t, "merge: m", appendTrailersToParagraph("merge: m", Trailers{}), "no set: unchanged")
}

// Only an experiment's OWN merge reads the ctx trace: a plain merge
// (agent sync, reconcile, peer merge) under a ctx that carries one stays a
// transport commit with no trailer; CommitExperiment under the same ctx is
// stamped. Sabotage: read the ctx trace in mergeIntoBranchLockedOpts for every
// merge (red: the plain merge is stamped).
func TestExperimentTrace_OnlyTheExperimentsOwnMerge(t *testing.T) {
	svc := newExperimentTestStore(t)
	ctx, err := WithAgentTrace(context.Background(), Trailers{Trace: "task-2"})
	require.NoError(t, err)
	msgOf := func(branch string) string {
		h, err := svc.Branches().HeadCommit(ctx, branch)
		require.NoError(t, err)
		info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(h))
		require.NoError(t, err)
		return info.Message
	}

	// A plain merge between two branches, under a traced ctx.
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "agent/peer", testAgentBranch))
	writeMergeFact(t, svc, "agent/peer", "kb/peer.md", "peer", "body")
	writeMergeFact(t, svc, testAgentBranch, "kb/own.md", "own", "body")
	require.NoError(t, svc.rh.MergeBranch(ctx, "agent/peer", testAgentBranch, StrategyLocalWins))
	plain := msgOf(testAgentBranch)
	require.Contains(t, plain, "merge: agent/peer into", "fixture: a merge commit was written")
	require.Empty(t, TrailerValue(plain, TrailerTrace), "a transport merge is never stamped: %q", plain)

	// The experiment's own merge, under the same ctx.
	_, err = svc.Experiments().OpenExperiment(context.Background(), "own", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/own", "kb/exp.md", "exp", "body")
	writeMergeFact(t, svc, testAgentBranch, "kb/own-2.md", "own 2", "body")
	res, err := svc.Experiments().CommitExperiment(ctx, "own", nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode)
	require.Equal(t, "task-2", TrailerValue(msgOf(testAgentBranch), TrailerTrace))
}
