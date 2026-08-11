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

// syncSyntheticHashes feeds hashes to CommitLogSync and reports how many of
// them had their payload thunk invoked.
func syncSyntheticHashes(t *testing.T, s *Service, branch string, hashes []string) (payloads int) {
	t.Helper()
	var i int
	err := s.rh.gits.CommitLogSync(branch, func() (string, storegit.CommitLogPayload, error) {
		if i >= len(hashes) {
			return "", nil, nil
		}
		h := hashes[i]
		i++
		return h, func() ([]string, []storegit.CommitLogEntry, error) {
			payloads++
			var parents []string
			if i > 1 {
				parents = []string{hashes[i-2]}
			}
			return parents, []storegit.CommitLogEntry{{
				Hash: h, Path: fmt.Sprintf("kb/f%s.md", h), Message: "m",
				Operation: "learn", AuthorName: "a", AuthorEmail: "a@b",
				Action: "added", CommittedAt: 1000 + int64(i),
			}}, nil
		}, nil
	})
	require.NoError(t, err)
	return payloads
}

// TestCommitLogSync_SkipsPayloadForKnownCommits is the regression anchor for
// the warm-open cost. A commit already recorded on the branch must not cost a
// payload computation: in production the payload is an object.DiffTree of ~300
// SQLite object loads (~2 ms/commit), and it was previously computed for every
// commit and then discarded by a dedup check that ran only afterwards. That
// made a warm repo open cost ~2 ms per commit for rows it already had.
func TestCommitLogSync_SkipsPayloadForKnownCommits(t *testing.T) {
	svc, branch := newCommitLogService(t)
	hashes := []string{
		"1111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222",
		"3333333333333333333333333333333333333333",
	}

	require.Equal(t, 3, syncSyntheticHashes(t, svc, branch, hashes),
		"first sync must compute every payload")

	require.Equal(t, 0, syncSyntheticHashes(t, svc, branch, hashes),
		"re-walk of fully-recorded commits must compute no payloads")

	fresh := "4444444444444444444444444444444444444444"
	require.Equal(t, 1, syncSyntheticHashes(t, svc, branch, append(append([]string{}, hashes...), fresh)),
		"a new commit after known ones must still be computed")
}

// TestCommitLogSync_WalksPastDedupHits guards the DAG invariant that the lazy
// payload must not weaken: a dedup hit mid-iteration skips-and-continues, it
// never ends the walk. On a merge commit, a known commit on one parent's line
// says nothing about the other parent's ancestry.
func TestCommitLogSync_WalksPastDedupHits(t *testing.T) {
	svc, branch := newCommitLogService(t)
	a := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	c := "cccccccccccccccccccccccccccccccccccccccc"

	syncSyntheticHashes(t, svc, branch, []string{b})

	require.Equal(t, 2, syncSyntheticHashes(t, svc, branch, []string{a, b, c}),
		"the walk must continue past the dedup hit on b and still compute c")

	for _, h := range []string{a, b, c} {
		var n int
		require.NoError(t, svc.rh.db.QueryRow(
			`SELECT COUNT(*) FROM branch_commits bc JOIN branches br ON br.id = bc.branch_id
			 WHERE br.name = ? AND bc.commit_hash = ?`, branch, h).Scan(&n))
		require.Equalf(t, 1, n, "branch_commits row for %s", h)
	}

	// The payload's rows still land: commit_log entries and commit_parents edges.
	var edges int
	require.NoError(t, svc.rh.db.QueryRow(
		`SELECT COUNT(*) FROM commit_parents WHERE commit_hash = ? AND parent_hash = ?`,
		c, b).Scan(&edges))
	require.Equal(t, 1, edges, "commit_parents edge from the computed payload")
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
