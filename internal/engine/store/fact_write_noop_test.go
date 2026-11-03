package store

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/facts"
)

// A write whose content is byte-identical to the blob already at the path is
// a no-op (#432): no commit, the branch tip does not move, the commit
// notification does not fire, and the result says Unchanged with the tip as
// CommitHash. Both doors that share writeFileExact are covered: WriteFact and
// the compare-and-swap WriteFactIfUnchanged. A changed write still commits.
//
// The assertion is on the BRANCH TIP, never on `git log -- <path>`: a
// path-limited log hides a commit that rewrites a file with identical bytes
// (kb/gotchas/testing/git/commit-counting).
//
// SABOTAGE: drop the blob-hash comparison from writeFileExact -> the identical
// writes commit, the tip moves, onCommit fires, Unchanged is false -> red.
func TestWriteFact_IdenticalContentMakesNoCommit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	ctx := context.Background()
	require.NoError(t, svc.InitRepo(ctx, map[string]string{}, "main"))
	fi := svc.Facts()
	var notified atomic.Int32
	svc.SetOnCommit(func(string, string) { notified.Add(1) })

	path := "kb/technology/software/abcdef12.md"
	body := testFactBody("abcdef12", 0.9, nil)
	first, err := fi.WriteFact(ctx, "main", path, body, "first", "learn")
	require.NoError(t, err)
	require.False(t, first.Unchanged, "the first write creates the file")
	tip, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, first.CommitHash, tip)
	notifiedAfterFirst := notified.Load()
	require.Positive(t, notifiedAfterFirst, "a real commit notifies")

	// WriteFact with the same bytes.
	again, err := fi.WriteFact(ctx, "main", path, body, "again", "update")
	require.NoError(t, err)
	require.True(t, again.Unchanged, "identical bytes: nothing to write")
	require.Equal(t, tip, again.CommitHash, "CommitHash is the tip compared against")
	require.Equal(t, first.BlobHash, again.BlobHash)
	now, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, tip, now, "the branch tip did not move")
	require.Equal(t, notifiedAfterFirst, notified.Load(), "no commit notification")

	// WriteFactIfUnchanged with the same bytes and the right blob.
	cas, err := fi.WriteFactIfUnchanged(ctx, "main", path, body, "cas", "update", first.BlobHash)
	require.NoError(t, err)
	require.True(t, cas.Unchanged)
	require.Equal(t, tip, cas.CommitHash)
	now, err = svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, tip, now, "the compare-and-swap door is a no-op too")
	require.Equal(t, notifiedAfterFirst, notified.Load())

	// The precondition still wins over the no-op: a stale blob is refused
	// even when the bytes match.
	_, err = fi.WriteFactIfUnchanged(ctx, "main", path, body, "stale", "update", "0000000000000000000000000000000000000000")
	require.ErrorIs(t, err, facts.ErrFactChanged)

	// A changed write is a real commit on top of the tip.
	changed, err := fi.WriteFact(ctx, "main", path, testFactBody("abcdef12", 0.5, nil), "changed", "update")
	require.NoError(t, err)
	require.False(t, changed.Unchanged)
	require.NotEqual(t, tip, changed.CommitHash)
	now, err = svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, changed.CommitHash, now, "the changed write moved the tip")
	require.Greater(t, notified.Load(), notifiedAfterFirst, "and notified")
}

// The root-file door goes through the same core: re-writing README.md with
// the same bytes makes no commit either.
func TestWriteRootFile_IdenticalContentMakesNoCommit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	ctx := context.Background()
	require.NoError(t, svc.InitRepo(ctx, map[string]string{}, "main"))
	fi := svc.Facts()

	_, err = fi.WriteRootFile(ctx, "main", "README.md", "# hello\n", "readme", "update")
	require.NoError(t, err)
	tip, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	res, err := fi.WriteRootFile(ctx, "main", "README.md", "# hello\n", "readme", "update")
	require.NoError(t, err)
	require.True(t, res.Unchanged)
	now, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, tip, now)
}
