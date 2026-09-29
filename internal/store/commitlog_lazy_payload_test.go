package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	storegit "knomit/internal/store/git"
)

// newCommitLogService opens a fresh single-branch repo for commit-log tests.
func newCommitLogService(t *testing.T) (*Service, string) {
	t.Helper()
	const branch = "main"
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))
	return svc, branch
}

// applySynthetic records synthetic commits (a chain, in order) with
// CommitLogApply and no derivation hook — the storegit-level contract.
func applySynthetic(t *testing.T, s *Service, branch string, hashes []string) {
	t.Helper()
	items := make([]storegit.CommitLogItem, len(hashes))
	for i, h := range hashes {
		var parents []string
		if i > 0 {
			parents = []string{hashes[i-1]}
		}
		items[i] = storegit.CommitLogItem{Hash: h, Parents: parents, Entries: []storegit.CommitLogEntry{{
			Hash: h, Path: fmt.Sprintf("kb/f%s.md", h), Message: "m",
			Operation: "learn", AuthorName: "a", AuthorEmail: "a@b",
			Action: "added", CommittedAt: 1000 + int64(i),
		}}}
	}
	require.NoError(t, s.rh.gits.CommitLogApply(context.Background(), branch, items, storegit.CommitLogApplyOptions{}))
}

// TestPopulateCommitLog_SkipsPayloadForKnownCommits is the regression anchor
// for the warm-open cost: a commit already recorded on the branch must not
// cost a payload diff (~2 ms each in production, for rows it already has).
func TestPopulateCommitLog_SkipsPayloadForKnownCommits(t *testing.T) {
	svc, branch := newCommitLogService(t)
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c"} {
		_, err := svc.Facts().WriteFact(ctx, branch, "kb/"+name+".md", testFactBody(name, 0.9, nil), "learn "+name, "learn")
		require.NoError(t, err)
	}
	before := payloadsComputed.Load()
	require.NoError(t, svc.rh.populateCommitLog(ctx, branch))
	require.Equal(t, before, payloadsComputed.Load(), "re-walk of fully-recorded commits must compute no payloads")

	_, err := svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.9, nil), "learn d", "learn")
	require.NoError(t, err)
	require.Equal(t, before+1, payloadsComputed.Load(), "a new commit is computed once")
}

// TestCommitLogApply_WalksPastDedupHits guards the DAG invariant: a dedup hit
// mid-batch skips-and-continues, it never ends the batch. On a merge commit, a
// known commit on one parent's line says nothing about the other parent's
// ancestry.
func TestCommitLogApply_WalksPastDedupHits(t *testing.T) {
	svc, branch := newCommitLogService(t)
	a := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	c := "cccccccccccccccccccccccccccccccccccccccc"

	applySynthetic(t, svc, branch, []string{b})
	applySynthetic(t, svc, branch, []string{a, b, c})

	for _, h := range []string{a, b, c} {
		var n int
		require.NoError(t, svc.rh.db.QueryRow(
			`SELECT COUNT(*) FROM branch_commits bc JOIN branches br ON br.id = bc.branch_id
			 WHERE br.name = ? AND bc.commit_hash = ?`, branch, h).Scan(&n))
		require.Equalf(t, 1, n, "branch_commits row for %s", h)
	}
	var edges int
	require.NoError(t, svc.rh.db.QueryRow(
		`SELECT COUNT(*) FROM commit_parents WHERE commit_hash = ? AND parent_hash = ?`,
		c, b).Scan(&edges))
	require.Equal(t, 1, edges, "commit_parents edge past the dedup hit")
}

// TestPopulateCommitLog_NoTreeReadsForKnownCommits is the end-to-end anchor. It
// deletes the tree objects of already-recorded commits, which makes any attempt
// to diff them fail outright. A re-walk must still succeed, proving the diff is
// never attempted for a commit the branch already has.
func TestPopulateCommitLog_NoTreeReadsForKnownCommits(t *testing.T) {
	svc, branch := newCommitLogService(t)
	ctx := context.Background()

	for _, name := range []string{"a", "b", "c"} {
		_, err := svc.Facts().WriteFact(ctx, branch, "kb/"+name+".md",
			testFactBody(name, 0.9, nil), "learn "+name, "learn")
		require.NoError(t, err)
	}

	// Collect every commit reachable from the tip, then drop its tree object.
	head, err := svc.rh.resolveRef(ctx, branch)
	require.NoError(t, err)
	iter, err := svc.rh.repo.Log(&gogit.LogOptions{From: head, Order: gogit.LogOrderDefault})
	require.NoError(t, err)
	var trees []plumbing.Hash
	require.NoError(t, iter.ForEach(func(c *object.Commit) error {
		trees = append(trees, c.TreeHash)
		return nil
	}))
	iter.Close()
	require.NotEmpty(t, trees)

	for _, h := range trees {
		require.NoError(t, svc.rh.gits.DeleteObjectForTest(h))
	}

	// Every commit is already recorded, so no payload — and therefore no tree
	// read — should be attempted. Before the lazy payload this errored with
	// "changedFilesInCommit: tree: object not found".
	require.NoError(t, svc.rh.populateCommitLog(ctx, branch),
		"re-walk of a populated branch must not read commit trees")
}
