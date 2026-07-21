package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIncomingAtCommit_TemporalBound pins the future-referrer over-return fix.
//
// The target Fact node is keyed by (path, blob_hash). When a target's blob is
// stable, EVERY source that ever refs it — including sources written AFTER the
// query anchor — points at the same node, so the blob-match returns them all.
// The branch post-filter alone (source_commit ∈ this branch) does not exclude a
// later same-branch referrer, so a query at an earlier anchor would surface a
// referrer that did not yet exist. The temporal bound (source committed_at ≤ the
// anchor's committed_at) closes that leak without dropping legitimate ancestors.
func TestIncomingAtCommit_TemporalBound(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	si := svc.Search().(*searchIndex)

	// B is the target; its blob never changes. A_old then A_new both ref B, so
	// both incoming edges point at the single (kb/b.md, blobX) node.
	_, err := svc.Facts().WriteFact(ctx, "main", "kb/b.md", testFactBody("b", 0.9, nil), "b", "")
	require.NoError(t, err)
	aOld, err := svc.Facts().WriteFact(ctx, "main", "kb/a_old.md", testFactBody("a_old", 0.8, []string{"kb/b.md"}), "a_old->b", "")
	require.NoError(t, err)
	aNew, err := svc.Facts().WriteFact(ctx, "main", "kb/a_new.md", testFactBody("a_new", 0.8, []string{"kb/b.md"}), "a_new->b", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))

	// git commit timestamps have 1-second resolution, so the three writes may
	// share a committed_at. Force a strict ordering directly in commit_log so the
	// temporal bound is exercised deterministically: A_old before A_new.
	setCommittedAt(t, si, aOld.CommitHash, 1000)
	setCommittedAt(t, si, aNew.CommitHash, 2000)

	// Anchor at A_old's commit: A_new (committed_at 2000 > 1000) did not yet
	// exist and must NOT be surfaced. Only A_old is a valid referrer.
	in, err := si.IncomingAtCommit(ctx, "main", "kb/b.md", aOld.CommitHash)
	require.NoError(t, err)
	require.Len(t, in, 1, "a referrer asserted after the anchor must be excluded (future-referrer over-return)")
	require.Equal(t, "kb/a_old.md", in[0].Path)

	// Anchor at A_new's commit: both referrers now exist as of the anchor and
	// must both be returned — the bound must not drop legitimate ancestors.
	in, err = si.IncomingAtCommit(ctx, "main", "kb/b.md", aNew.CommitHash)
	require.NoError(t, err)
	require.Len(t, in, 2, "both referrers exist as of the later anchor; neither may be dropped")
}

// setCommittedAt overwrites the committed_at of a commit in commit_log so a test
// can construct a deterministic temporal ordering independent of the 1-second
// git clock resolution.
func setCommittedAt(t *testing.T, si *searchIndex, commit string, ts int64) {
	t.Helper()
	res, err := si.rh.db.ExecContext(context.Background(),
		`UPDATE commit_log SET committed_at = ? WHERE commit_hash = ?`, ts, commit)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Greater(t, n, int64(0), "commit %s has no commit_log row to update", commit)
}

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

	si := svc.Search().(*searchIndex)

	// Precondition: on main, A's effective write-commit resolves to the MERGE
	// commit (not the feature write) — the exact condition that breaks the
	// commit-equality edge filter.
	eff, ok, err := si.resolveActiveCommitForPath(ctx, "main", "kb/a.md", head)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, head, eff, "precondition: A's effective write-commit on main IS the merge commit")

	// OUTGOING: A→B must be surfaced at HEAD even though the edge is anchored to
	// the feature write-commit, not the merge.
	out, err := si.OutgoingAtCommit(ctx, "main", "kb/a.md", head)
	require.NoError(t, err)
	require.Len(t, out, 1, "A's outgoing edge to B must survive resolving HEAD to the merge commit")
	require.Equal(t, "kb/b.md", out[0].Path)

	// INCOMING: B←A symmetric.
	in, err := si.IncomingAtCommit(ctx, "main", "kb/b.md", head)
	require.NoError(t, err)
	require.Len(t, in, 1, "B's incoming edge from A must survive resolving HEAD to the merge commit")
	require.Equal(t, "kb/a.md", in[0].Path)
}
