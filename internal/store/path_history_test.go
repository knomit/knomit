package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func openPathHistoryStore(t *testing.T) (*Service, context.Context) {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc, context.Background()
}

func historyCommits(revs []RevisionMeta) []string {
	out := make([]string, len(revs))
	for i, r := range revs {
		out[i] = r.Commit
	}
	return out
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}

// TestPathHistory_MergeDeliveredWrites is the regression for the explain
// history walk that listed one PR merge per PR and hid every write behind it.
// Every write lands on `agent`, reaches main through a real two-parent merge,
// and `agent` then catches up with main — the topology of an agent branch
// whose work is merged by PR. The history must be exactly the write commits,
// newest first, on both branches, with no merge commit in it.
func TestPathHistory_MergeDeliveredWrites(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	const p = "kb/t.md"

	created, err := svc.Facts().WriteFact(ctx, "main", p, testFactBody("v0", 0.9, nil), "create", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent", "main"))

	writes := []string{created.CommitHash}
	var merges []string
	for k := range 5 {
		for j := range 3 {
			r, err := svc.Facts().WriteFact(ctx, "agent", p,
				testFactBody(fmt.Sprintf("pr%d w%d", k, j), 0.5, nil), fmt.Sprintf("pr%d w%d", k, j), "")
			require.NoError(t, err)
			writes = append(writes, r.CommitHash)
		}
		// Diverge main on another path so the PR merge is a real two-parent commit.
		_, err := svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/other%d.md", k), testFactBody("x", 0.5, nil), "other", "")
		require.NoError(t, err)
		require.NoError(t, svc.Branches().MergeBranch(ctx, "agent", "main", StrategyLocalWins))
		tip, err := svc.Branches().HeadCommit(ctx, "main")
		require.NoError(t, err)
		mc, err := svc.rh.repo.CommitObject(plumbing.NewHash(tip))
		require.NoError(t, err)
		require.Len(t, mc.ParentHashes, 2, "setup must produce a two-parent PR merge")
		merges = append(merges, tip)
		require.NoError(t, svc.Branches().MergeBranch(ctx, "main", "agent", StrategyLocalWins))
	}

	// The first-parent walk this replaces sees only the merges.
	fp, err := svc.Search().RevisionsBefore(ctx, "main", p, merges[len(merges)-1], 100)
	require.NoError(t, err)
	require.Len(t, fp, len(merges)+1, "precondition: first-parent sees one revision per PR plus the creation")

	want := reversed(writes)
	for _, br := range []string{"main", "agent"} {
		anchor, err := svc.Branches().HeadCommit(ctx, br)
		require.NoError(t, err)
		revs, err := svc.Search().PathHistory(ctx, br, p, anchor)
		require.NoError(t, err)
		require.Equal(t, want, historyCommits(revs), "%s: every write, newest first, and nothing else", br)
		require.Equal(t, "added", revs[len(revs)-1].Action, "%s: the oldest entry is the creation", br)
		for _, r := range revs[:len(revs)-1] {
			require.Equal(t, "modified", r.Action)
			require.NotZero(t, r.CommittedAt)
		}
	}
}

// TestPathHistory_LocalWinsLoserExcluded: a feature write that a LocalWins
// merge discarded never reached main's content, so it is not in main's
// history even though its commit is reachable through the merge's second
// parent. Mirrors TestRevisionsBefore_MergeAnomalyPicksFirstParent.
func TestPathHistory_LocalWinsLoserExcluded(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)

	v1, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("v1", 0.9, nil), "v1 on main", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))
	loser, err := svc.Facts().WriteFact(ctx, "feature", "kb/t.md", testFactBody("v2", 0.8, nil), "v2 on feature", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "feature", "kb/other.md", testFactBody("side", 0.5, nil), "side", "")
	require.NoError(t, err)
	v3, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("v3", 0.7, nil), "v3 on main", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().MergeBranch(ctx, "feature", "main", StrategyLocalWins))

	tip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	mc, err := svc.rh.repo.CommitObject(plumbing.NewHash(tip))
	require.NoError(t, err)
	require.Len(t, mc.ParentHashes, 2)

	revs, err := svc.Search().PathHistory(ctx, "main", "kb/t.md", tip)
	require.NoError(t, err)
	require.Equal(t, []string{v3.CommitHash, v1.CommitHash}, historyCommits(revs))
	require.NotContains(t, historyCommits(revs), loser.CommitHash)
}

// TestPathHistory_MergeWithNewContentIsAnEntry: a merge whose result for the
// path matches neither parent introduced new content, so it is an entry — and
// both sides' writes are in its history.
func TestPathHistory_MergeWithNewContentIsAnEntry(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	const p = "kb/t.md"

	v1, err := svc.Facts().WriteFact(ctx, "main", p, testFactBody("v1", 0.9, nil), "v1", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))
	side, err := svc.Facts().WriteFact(ctx, "feature", p, testFactBody("feature side", 0.8, nil), "feature edit", "")
	require.NoError(t, err)
	local, err := svc.Facts().WriteFact(ctx, "main", p, testFactBody("main side", 0.7, nil), "main edit", "")
	require.NoError(t, err)

	_, err = svc.rh.mergeIntoBranchResolved(ctx, "feature", "main", StrategyRefuse, map[string]Resolution{
		p: {Body: []byte(testFactBody("composed", 0.75, nil))},
	})
	require.NoError(t, err)
	tip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	mc, err := svc.rh.repo.CommitObject(plumbing.NewHash(tip))
	require.NoError(t, err)
	require.Len(t, mc.ParentHashes, 2)

	revs, err := svc.Search().PathHistory(ctx, "main", p, tip)
	require.NoError(t, err)
	got := historyCommits(revs)
	require.Len(t, got, 4)
	require.Equal(t, tip, got[0], "the composing merge is the newest change")
	require.Equal(t, "modified", revs[0].Action)
	require.ElementsMatch(t, []string{side.CommitHash, local.CommitHash}, got[1:3])
	require.Equal(t, v1.CommitHash, got[3])
}

// TestPathHistory_RevertIsAChange: returning to an earlier blob is a new
// change — the newest entry must be the content live at the anchor.
func TestPathHistory_RevertIsAChange(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	a := testFactBody("A", 0.9, nil)
	r1, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", a, "A", "")
	require.NoError(t, err)
	r2, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("B", 0.9, nil), "B", "")
	require.NoError(t, err)
	r3, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", a, "back to A", "")
	require.NoError(t, err)

	revs, err := svc.Search().PathHistory(ctx, "main", "kb/t.md", r3.CommitHash)
	require.NoError(t, err)
	require.Equal(t, []string{r3.CommitHash, r2.CommitHash, r1.CommitHash}, historyCommits(revs))
}

// TestPathHistory_LinearMatchesFirstParent: on a linear history the change
// list is exactly the first-parent list.
func TestPathHistory_LinearMatchesFirstParent(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	c1, c2, c3 := writeThreeVersions(t, svc, ctx, "main")

	revs, err := svc.Search().PathHistory(ctx, "main", "kb/t.md", c3)
	require.NoError(t, err)
	require.Equal(t, []string{c3, c2, c1}, historyCommits(revs))
	require.Equal(t, "edit t again", revs[0].Message)
	require.Equal(t, "added", revs[2].Action)

	// Bounded to the anchor.
	revs, err = svc.Search().PathHistory(ctx, "main", "kb/t.md", c2)
	require.NoError(t, err)
	require.Equal(t, []string{c2, c1}, historyCommits(revs))
}

// TestPathHistory_ScopedToBranch: an anchor committed on another branch
// contributes nothing off-branch; its on-branch ancestry still counts.
func TestPathHistory_ScopedToBranch(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	r1, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("v1", 0.9, nil), "create t", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))
	r2, err := svc.Facts().WriteFact(ctx, "feature", "kb/t.md", testFactBody("v2", 0.8, nil), "edit on feature", "")
	require.NoError(t, err)

	revs, err := svc.Search().PathHistory(ctx, "main", "kb/t.md", r2.CommitHash)
	require.NoError(t, err)
	require.Equal(t, []string{r1.CommitHash}, historyCommits(revs))

	revs, err = svc.Search().PathHistory(ctx, "feature", "kb/t.md", r2.CommitHash)
	require.NoError(t, err)
	require.Equal(t, []string{r2.CommitHash, r1.CommitHash}, historyCommits(revs))
}
