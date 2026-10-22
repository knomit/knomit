package source

import (
	"strconv"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"
)

// newFixtureRepo builds an in-memory git repo, so these tests exercise the
// same code path the CLI uses against a real clone.
func newFixtureRepo(t *testing.T) *git.Repository {
	t.Helper()
	r, err := git.Init(memory.NewStorage(), memfs.New())
	require.NoError(t, err)
	return r
}

// commitAt writes files, deletes paths, and commits, returning the commit hash.
// The time is explicit so tests can pin chronology instead of racing the clock.
func commitAt(t *testing.T, r *git.Repository, msg, email string, when time.Time, files map[string]string, remove ...string) plumbing.Hash {
	t.Helper()
	wt, err := r.Worktree()
	require.NoError(t, err)
	for p, body := range files {
		require.NoError(t, util.WriteFile(wt.Filesystem, p, []byte(body), 0o644))
		_, err = wt.Add(p)
		require.NoError(t, err)
	}
	for _, p := range remove {
		_, err = wt.Remove(p)
		require.NoError(t, err)
	}
	sig := &object.Signature{Name: "t", Email: email, When: when}
	h, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig, AllowEmptyCommits: true})
	require.NoError(t, err)
	return h
}

// commitFiles is commitAt at a fixed default time, for tests that do not care.
func commitFiles(t *testing.T, r *git.Repository, msg, email string, files map[string]string) plumbing.Hash {
	t.Helper()
	return commitAt(t, r, msg, email, baseTime, files)
}

var baseTime = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

func factBody(title string, conf float64) string {
	return "---\nkind: epistemic\ntype: observation\ndomain: [x]\nconfidence: " +
		strconv.FormatFloat(conf, 'g', -1, 64) + "\n---\n# " + title + "\n\nBody.\n"
}

func TestLoad_ReadsFactsAndIdentity(t *testing.T) {
	r := newFixtureRepo(t)
	h := commitFiles(t, r, "seed", "a+learn@agents.knomit.io", map[string]string{
		"kb/decisions/x/aaaaaaaa.md": factBody("Alpha", 0.9),
		"kb/decisions/x/bbbbbbbb.md": factBody("Beta", 0.8),
	})
	snap, err := Load(r.Storer, h)
	require.NoError(t, err)
	require.Len(t, snap.Facts, 2)
	require.Len(t, snap.RepoID, 12, "repo id is the 12-hex root commit prefix")
	require.Equal(t, h, snap.SourceSHA)
}
