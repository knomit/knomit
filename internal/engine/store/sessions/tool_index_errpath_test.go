package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"knomit/internal/engine/store/model"

	"github.com/stretchr/testify/require"
)

// newToolIndexWithDB is newToolIndexForTest that also hands back the session
// DB and its path, for the error-path tests.
func newToolIndexWithDB(t *testing.T) (*ToolIndex, *sql.DB, string) {
	t.Helper()
	db, path, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewToolIndex(db, BooksDirFor(path)), db, path
}

// A dequeue that fails after BEGIN IMMEDIATE must give its connection back
// with no transaction open: a forced failure (a trigger aborts the DELETE)
// leaves no connection in use, the queue intact, and the next transactions on
// the session DB succeed.
//
// Sabotage: drop the deferred ROLLBACK and the connection goes back to the
// pool inside the open transaction (the next transaction on it fails with
// "cannot start a transaction within a transaction"); drop the deferred
// Close and InUse stays 1.
func TestDequeuePaths_FailureLeavesNoTransactionOrConnectionBehind(t *testing.T) {
	ti, db, _ := newToolIndexWithDB(t)
	ctx := context.Background()
	s, err := ti.CreateToolSession(ctx, "materialize:human:3", "main", "kb/root.md", "repo:pin", "rs", "owner")
	require.NoError(t, err)
	require.NoError(t, ti.EnqueuePaths(ctx, s.ID, queueItems(5)))

	_, err = db.ExecContext(ctx, `CREATE TRIGGER probe_abort BEFORE DELETE ON tool_queue
		BEGIN SELECT RAISE(ABORT, 'probe: dequeue forced to fail'); END`)
	require.NoError(t, err)

	_, err = ti.DequeuePaths(ctx, s.ID, 3)
	require.ErrorContains(t, err, "probe: dequeue forced to fail", "the failure is the trigger's, at the DELETE")
	require.NotErrorIs(t, err, model.ErrSessionBusy, "an abort is not a busy store")
	require.Equal(t, 0, db.Stats().InUse, "the dequeue's connection went back to the pool")

	// The next transactions on the session DB must start cleanly on whatever
	// connection the pool hands out (the failed dequeue's comes back first).
	for i := 0; i < 4; i++ {
		require.NoError(t, ti.EnqueuePaths(ctx, s.ID, []model.QueueItem{
			{Path: fmt.Sprintf("kb/y/%d.md", i), CommitHash: "c", SortKey: 100 + i, State: "{}"},
		}), "transaction %d after the failed dequeue", i)
	}
	n, err := ti.QueueSize(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, 9, n, "the failed dequeue consumed nothing")

	_, err = db.ExecContext(ctx, `DROP TRIGGER probe_abort`)
	require.NoError(t, err)
	items, err := ti.DequeuePaths(ctx, s.ID, 3)
	require.NoError(t, err)
	require.Len(t, items, 3)
	require.Equal(t, 0, items[0].SortKey, "the head of the queue is still the first item")
	require.Equal(t, 0, db.Stats().InUse)
}

// A dequeue that cannot get the write lock within the busy timeout is marked
// model.ErrSessionBusy (so the edge can answer it as retryable) and consumes
// nothing. Takes the busy timeout (5 s) to run.
//
// Sabotage: return the error unmarked (busy() returning err as is) and
// errors.Is fails.
func TestDequeuePaths_LockTimeoutIsSessionBusyAndConsumesNothing(t *testing.T) {
	ti, _, path := newToolIndexWithDB(t)
	ctx := context.Background()
	s, err := ti.CreateToolSession(ctx, "materialize:human:3", "main", "kb/root.md", "repo:pin", "rs", "owner")
	require.NoError(t, err)
	require.NoError(t, ti.EnqueuePaths(ctx, s.ID, queueItems(5)))

	release := holdWriteLock(t, path)
	_, err = ti.DequeuePaths(ctx, s.ID, 3)
	release()
	require.ErrorIs(t, err, model.ErrSessionBusy)

	n, err := ti.QueueSize(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, 5, n, "a busy dequeue consumed nothing")
}

// holdWriteLock takes the session DB's write lock on a separate handle and
// returns the function that releases it.
func holdWriteLock(t *testing.T, path string) func() {
	t.Helper()
	ctx := context.Background()
	other, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	require.NoError(t, err)
	conn, err := other.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
	require.NoError(t, err)
	return func() {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		_ = conn.Close()
		_ = other.Close()
	}
}
