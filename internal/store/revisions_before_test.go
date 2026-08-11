package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// writeThreeVersions writes kb/t.md three times and returns the three commit
// hashes (oldest → newest).
func writeThreeVersions(t *testing.T, svc *Service, ctx context.Context, branch string) (c1, c2, c3 string) {
	t.Helper()
	r1, err := svc.Facts().WriteFact(ctx, branch, "kb/t.md", testFactBody("v1", 0.9, nil), "create t", "")
	require.NoError(t, err)
	r2, err := svc.Facts().WriteFact(ctx, branch, "kb/t.md", testFactBody("v2", 0.8, nil), "edit t", "")
	require.NoError(t, err)
	r3, err := svc.Facts().WriteFact(ctx, branch, "kb/t.md", testFactBody("v3", 0.7, nil), "edit t again", "")
	require.NoError(t, err)
	return r1.CommitHash, r2.CommitHash, r3.CommitHash
}

// TestRevisionsBefore_WalksAncestryNewestFirst writes three versions across
// three commits and asserts RevisionsBefore returns them newest→oldest with
// commit metadata populated.
func TestRevisionsBefore_WalksAncestryNewestFirst(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()

	c1, c2, c3 := writeThreeVersions(t, svc, ctx, "main")

	revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/t.md", c3, 10)
	require.NoError(t, err)
	require.Len(t, revs, 3)
	require.Equal(t,
		[]string{c3, c2, c1},
		[]string{revs[0].Commit, revs[1].Commit, revs[2].Commit},
		"revisions must be newest → oldest")
	require.Equal(t, "edit t again", revs[0].Message)
	require.Equal(t, "create t", revs[2].Message)
	require.Equal(t, "added", revs[2].Action, "oldest revision is the creation")
	require.Equal(t, "modified", revs[0].Action)
	require.NotZero(t, revs[0].CommittedAt, "CommittedAt populated from commit_log")
}

// TestRevisionsBefore_BoundedToAnchor pins that history is the ancestry of the
// anchor commit — edits made after the anchor are not surfaced.
func TestRevisionsBefore_BoundedToAnchor(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()

	c1, c2, _ := writeThreeVersions(t, svc, ctx, "main")

	revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/t.md", c2, 10)
	require.NoError(t, err)
	require.Len(t, revs, 2, "anchoring at c2 must exclude the c3 edit made after it")
	require.Equal(t, []string{c2, c1}, []string{revs[0].Commit, revs[1].Commit})
}

// TestRevisionsBefore_ScopedToBranch pins that revisions reachable only via an
// off-branch lineage are not surfaced: a commit written on `feature` is in the
// first-parent ancestry of the feature anchor, but querying as `main` must drop
// it because it is not in main's branch_commits.
func TestRevisionsBefore_ScopedToBranch(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()

	r1, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("v1", 0.9, nil), "create t", "")
	require.NoError(t, err)

	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))
	r2, err := svc.Facts().WriteFact(ctx, "feature", "kb/t.md", testFactBody("v2", 0.8, nil), "edit t on feature", "")
	require.NoError(t, err)

	// Anchor is the feature commit; its first-parent ancestry includes r2 (feature)
	// and r1 (main). Querying as main must return only r1.
	revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/t.md", r2.CommitHash, 10)
	require.NoError(t, err)
	require.Len(t, revs, 1, "off-branch feature commit must be filtered out for branch=main")
	require.Equal(t, r1.CommitHash, revs[0].Commit)

	// Querying as feature surfaces both, newest → oldest.
	frevs, err := svc.Search().RevisionsBefore(ctx, "feature", "kb/t.md", r2.CommitHash, 10)
	require.NoError(t, err)
	require.Equal(t, []string{r2.CommitHash, r1.CommitHash},
		[]string{frevs[0].Commit, frevs[1].Commit})
}

// TestRevisionsBefore_RespectsLimit pins that limit caps the returned count
// (newest-first).
func TestRevisionsBefore_RespectsLimit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()

	_, c2, c3 := writeThreeVersions(t, svc, ctx, "main")

	revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/t.md", c3, 2)
	require.NoError(t, err)
	require.Len(t, revs, 2)
	require.Equal(t, []string{c3, c2}, []string{revs[0].Commit, revs[1].Commit})
}

// TestRevisionsBefore_MergeAnomalyPicksFirstParent is the guard on
// kb/invariants/store/resolver/first-parent-not-wall-clock/00a49427.md.
//
// Setup: main writes v1, then a feature branch writes v2 to the SAME path, then
// main writes v3, then feature merges into main. After the merge, the feature
// commit (v2) is reachable from main's tip only through the merge commit's
// SECOND parent — walking first parents from the tip stays on main's own line
// and never visits it. `branch_commits`, however, is populated by a full
// reachability walk (see derived_from.go's SCHEMA INVARIANT comment), so once
// the merge lands, v2 IS a branch_commits row for "main" even though it is off
// main's first-parent line. A query that joins commit_log straight to
// branch_commits — which is exactly what the (path, committed_at DESC) index
// on commit_log invites — will surface v2 as a candidate for "main"; only
// walking the first-parent CTE and intersecting with branch_commits correctly
// excludes it. If this test ever fails, do not relax it — the query regressed.
func TestRevisionsBefore_MergeAnomalyPicksFirstParent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()

	_, err = svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("v1", 0.9, nil), "v1 on main", "")
	require.NoError(t, err)

	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))
	featureRes, err := svc.Facts().WriteFact(ctx, "feature", "kb/t.md", testFactBody("v2", 0.8, nil), "v2 on feature", "")
	require.NoError(t, err)
	// Also touch a disjoint path on feature so the merged tree differs from
	// dst's tree even with LocalWins on kb/t.md — otherwise mergeIntoBranch
	// treats the merge as a no-op (merged tree identical to dst) and never
	// produces a real two-parent merge commit.
	_, err = svc.Facts().WriteFact(ctx, "feature", "kb/other.md", testFactBody("side", 0.5, nil), "side file on feature", "")
	require.NoError(t, err)

	mainRes, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("v3", 0.7, nil), "v3 on main", "")
	require.NoError(t, err)

	require.NoError(t, svc.Branches().MergeBranch(ctx, "feature", "main", StrategyLocalWins))

	tip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)

	mergeCommit, err := svc.rh.repo.CommitObject(plumbing.NewHash(tip))
	require.NoError(t, err)
	require.Len(t, mergeCommit.ParentHashes, 2, "setup must actually produce a two-parent merge commit, or this test proves nothing")

	revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/t.md", tip, 1)
	require.NoError(t, err)
	require.Len(t, revs, 1)
	require.Equal(t, mainRes.CommitHash, revs[0].Commit,
		"first-parent ancestry from main's tip must reach main's own v3, not the merged-in feature commit %s",
		featureRes.CommitHash)
	require.NotZero(t, revs[0].CommittedAt, "committed_at must be populated for display")

	// Tie-break-independent guard: regardless of limit or ordering, the
	// feature commit must never appear anywhere in the result. It is
	// reachable from main's tip only through the merge commit's second
	// parent, so a correct first-parent walk never visits it. This does not
	// depend on committed_at at all, unlike the limit=1 assertion above —
	// it catches an implementation that walks branch_commits (full
	// reachability) instead of the first-parent chain, even if that
	// implementation happens to break committed_at ties in main's favor.
	allRevs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/t.md", tip, 10)
	require.NoError(t, err)
	for _, r := range allRevs {
		require.NotEqual(t, featureRes.CommitHash, r.Commit,
			"feature commit %s is reachable from main's tip only via the merge commit's second parent; "+
				"it must never surface for branch=main regardless of limit or ordering", featureRes.CommitHash)
	}
}
