package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGraphAtCommit_ResolvesThroughMergeCommit reproduces the "empty
// connections after a merge" bug: a fact brought into a branch via a MERGE
// commit shows no in/out edges at HEAD.
//
// Mechanism: DERIVED_FROM edges are anchored to the fact's original
// content-write commit (source_commit / target_commit on the feature branch),
// but resolveActiveCommitForPath resolves HEAD to the MERGE commit — commit_log
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
	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))
	_, err := svc.Facts().WriteFact(ctx, "feature", "kb/b.md", testFactBody("b", 0.9, nil), "b", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "feature", "kb/a.md", testFactBody("a", 0.8, []string{"kb/b.md"}), "a->b", "")
	require.NoError(t, err)

	// Diverge main so the merge is a real 3-way merge (a merge commit, not a
	// fast-forward) that re-touches A and B into main's first-parent history.
	writeMergeFact(t, svc, "main", "kb/x.md", "x", "diverge")

	require.NoError(t, svc.Branches().MergeBranch(ctx, "feature", "main", StrategyLocalWins))
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))

	head, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)

	si := svc.si

	// Precondition: on main, A's effective write-commit resolves to the MERGE
	// commit (not the feature write) — the exact condition that breaks the
	// commit-equality edge filter.
	eff, ok, err := si.rh.resolveActiveCommitForPath(ctx, "main", "kb/a.md", head)
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
