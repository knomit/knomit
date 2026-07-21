package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// TestMidQueryCancellation_SurfacesAsContextCanceled pins the driver contract
// that the caller-side cancellation checks rely on — notably the ERROR→DEBUG
// downgrade in web's defaultFactReader.Exists, which tests errors.Is(err,
// context.Canceled) and nothing else.
//
// The concern this pins is specific: mattn/go-sqlite3 aborts a running
// statement by calling sqlite3_interrupt, whose native error is
// sqlite3.Error{Code: ErrInterrupt} and does NOT wrap context.Canceled. If that
// error reached callers, a client disconnect landing mid-query would slip past
// every errors.Is(context.Canceled) check in the codebase.
//
// It does not: database/sql runs its own context watcher around the driver call
// and replaces the driver's error with ctx.Err(). The query below is a
// recursive CTE long enough that the cancel is guaranteed to land while the
// statement is executing inside SQLite, so this exercises the interrupt path
// rather than the cheap pre-query check.
//
// If a driver or database/sql upgrade ever lets the bare interrupt escape, this
// test fails — and the cancellation checks must then be broadened to treat a
// sqlite interrupt with ctx.Err() != nil as cancellation.
func TestMidQueryCancellation_SurfacesAsContextCanceled(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	// Counts to 50M — seconds of work, so the cancel below lands mid-statement.
	const slowQuery = `WITH RECURSIVE c(x) AS (
		SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 50000000
	) SELECT count(*) FROM c`

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	var n int
	qerr := svc.rh.db.QueryRowContext(ctx, slowQuery).Scan(&n)
	require.Error(t, qerr, "the cancel must abort the statement, not let it run to completion")

	var se sqlite3.Error
	if errors.As(qerr, &se) && se.Code == sqlite3.ErrInterrupt {
		t.Fatalf("bare sqlite interrupt escaped to the caller (%v); "+
			"errors.Is(err, context.Canceled) checks no longer detect mid-query cancellation", qerr)
	}
	require.ErrorIs(t, qerr, context.Canceled,
		"mid-query cancellation must be observable as context.Canceled")
}
