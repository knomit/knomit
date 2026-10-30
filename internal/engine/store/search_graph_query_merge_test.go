package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/merge"
)

// TestGraphAtCommit_ResolvesThroughMergeCommit reproduces the "empty
// connections after a merge" bug: a fact brought into a branch via a MERGE
// commit shows no in/out edges at HEAD.
//
// Mechanism: DERIVED_FROM edges are anchored to the fact's original
// content-write commit (source_commit / target_commit on the feature branch),
// but ResolveActiveCommitForPath resolves HEAD to the MERGE commit — commit_log
// records the merge as an add/modify of the path (fact_history diffs the merge
// against its first parent, which lacks the fact). The old queries filtered
// edges strictly by `r.source_commit = mergeCommit`, which never matches the
// feature-anchored edge, so both directions returned 0. Matching the edge by
// the fact's blob-version node (stable across the merge) fixes it.
func TestGraphAtCommit_ResolvesThroughMergeCommit(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()

	// feature branch carries BOTH facts: B, then A→B (the edge is created on
	// feature, anchored to feature's write-commits).
	require.NoError(t, svc.CreateBranch(ctx, "feature", "main"))
	_, err := svc.Facts().WriteFact(ctx, "feature", "kb/b.md", testFactBody("b", 0.9, nil), "b", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "feature", "kb/a.md", testFactBody("a", 0.8, []string{"kb/b.md"}), "a->b", "")
	require.NoError(t, err)

	// Diverge main so the merge is a real 3-way merge (a merge commit, not a
	// fast-forward) that re-touches A and B into main's first-parent history.
	writeMergeFact(t, svc, "main", "kb/x.md", "x", "diverge")

	require.NoError(t, svc.Merge().MergeBranch(ctx, "feature", "main", merge.StrategyLocalWins))
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))

	head, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)

	si := svc.si

	// Precondition: on main, A's effective write-commit resolves to the MERGE
	// commit (not the feature write) — the exact condition that breaks the
	// commit-equality edge filter.
	eff, ok, err := si.RepoForTest().ResolveActiveCommitForPath(ctx, "main", "kb/a.md", head)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, head, eff, "precondition: A's effective write-commit on main IS the merge commit")

	// OUTGOING: A→B must be surfaced at HEAD even though the edge is anchored to
	// the feature write-commit, not the merge.
	out, err := svc.gq.OutgoingAtCommit(ctx, "main", "kb/a.md", head)
	require.NoError(t, err)
	require.Len(t, out, 1, "A's outgoing edge to B must survive resolving HEAD to the merge commit")
	require.Equal(t, "kb/b.md", out[0].Path)

	// INCOMING: B←A symmetric.
	in, err := svc.gq.IncomingAtCommit(ctx, "main", "kb/b.md", head)
	require.NoError(t, err)
	require.Len(t, in, 1, "B's incoming edge from A must survive resolving HEAD to the merge commit")
	require.Equal(t, "kb/a.md", in[0].Path)
}

// ResolveActiveCommitForPath's early-stopping recursion (round 4 of PR
// #692) answers exactly what the old first-parent chain walk (ORDER BY
// depth LIMIT 1) answers, for every (path, commit) of a history with
// writes, a modify, a delete, a re-add and a real merge: a commit that wrote
// the path, one that did not, one that deleted it, the merge, and paths
// never written.
//
// Sabotage: count a deletion as a write in the recursion, and the commit
// that deleted kb/a.md resolves to itself instead of the write before it.
func TestResolveActiveCommitForPath_EarlyStopIsTheChainWalk(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	require.NoError(t, svc.CreateBranch(ctx, "feature", "main"))
	_, err := svc.Facts().WriteFact(ctx, "feature", "kb/b.md", testFactBody("b", 0.9, nil), "b", "")
	require.NoError(t, err)
	writeMergeFact(t, svc, "main", "kb/a.md", "a", "a one")
	writeMergeFact(t, svc, "main", "kb/x.md", "x", "x")
	writeMergeFact(t, svc, "main", "kb/a.md", "a", "a two")
	_, err = svc.Facts().DeleteFact(ctx, "main", "kb/a.md", "delete a")
	require.NoError(t, err)
	writeMergeFact(t, svc, "main", "kb/y.md", "y", "y")
	writeMergeFact(t, svc, "main", "kb/a.md", "a", "a three")
	require.NoError(t, svc.Merge().MergeBranch(ctx, "feature", "main", merge.StrategyLocalWins))
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))

	rh := svc.si.RepoForTest()
	head, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	var commits []string
	for c := head; c != ""; {
		commits = append(commits, c)
		c, err = rh.FirstParentCommit(ctx, c)
		require.NoError(t, err)
	}
	require.GreaterOrEqual(t, len(commits), 7)
	fast, chainDiffers := 0, 0
	for _, c := range commits {
		for _, p := range []string{"kb/a.md", "kb/b.md", "kb/x.md", "kb/y.md", "kb/never.md"} {
			got, gotOK, err := rh.ResolveActiveCommitForPath(ctx, "main", p, c)
			require.NoError(t, err)
			want, wantOK, err := rh.ResolveActiveCommitForPathChainForTest(ctx, "main", p, c)
			require.NoError(t, err)
			require.Equal(t, wantOK, gotOK, "%s at %s", p, c[:8])
			require.Equal(t, want, got, "%s at %s", p, c[:8])
			if gotOK && got == c {
				fast++
			} else if gotOK {
				chainDiffers++
			}
		}
	}
	require.Positive(t, fast, "some pairs take the fast path")
	require.Positive(t, chainDiffers, "and some resolve further down the chain")
}
