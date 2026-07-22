package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWriteFact_CancelledContext_KeepsGitAndIndexConsistent: a caller whose
// context is already cancelled must not be able to leave git ahead of the SQL
// index. writeFile advances the branch ref via SetReference (not ctx-bound) and
// then calls notifyCommit (AppendCommitLog + im.Sync, both ctx-bound SQL); if
// cancellation reached notifyCommit the commit would exist in git while
// commit_log / branch_facts never learned about it, and the caller would be
// told the write failed.
//
// resolveRef ignores ctx, so a pre-cancelled context reaches SetReference
// deterministically — no timing window needed to reproduce.
func TestWriteFact_CancelledContext_KeepsGitAndIndexConsistent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := svc.Facts().WriteFact(ctx, "main", "kb/cancelled.md",
		testFactBody("cancelled", 0.9, nil), "write under cancelled ctx", "update")
	require.NoError(t, err, "write must complete atomically despite caller cancellation")
	require.NotEmpty(t, res.CommitHash)

	// The commit git accepted must also be visible to the SQL index: read the
	// fact back at HEAD (branch_facts) and confirm the commit is in commit_log.
	got, err := svc.Facts().ReadFact(context.Background(), "main", "kb/cancelled.md", nil)
	require.NoError(t, err, "fact written to git must be readable through the index")
	require.NotEmpty(t, got.Content)

	var n int
	require.NoError(t, svc.rh.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM commit_log WHERE commit_hash = ?`, res.CommitHash).Scan(&n))
	require.Equal(t, 1, n, "commit %s advanced the git ref but never reached commit_log", res.CommitHash)
}

// TestDeleteFact_CancelledContext_KeepsGitAndIndexConsistent: same invariant on
// the delete path, which has the identical SetReference-then-notifyCommit
// sequence.
func TestDeleteFact_CancelledContext_KeepsGitAndIndexConsistent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	bg := context.Background()
	_, err = svc.Facts().WriteFact(bg, "main", "kb/doomed.md",
		testFactBody("doomed", 0.9, nil), "init", "update")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(bg)
	cancel()

	hash, err := svc.Facts().DeleteFact(ctx, "main", "kb/doomed.md", "retract under cancelled ctx")
	require.NoError(t, err, "delete must complete atomically despite caller cancellation")
	require.NotEmpty(t, hash)

	// git dropped the path, so the index must have dropped it too.
	_, err = svc.Facts().ReadFact(bg, "main", "kb/doomed.md", nil)
	require.Error(t, err, "deleted fact must not still be readable through the index")
}
