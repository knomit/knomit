package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// The tests here all assert one property from different angles: a swap that
// cannot complete must leave the repo serving the database it was serving
// before, and must resume replication against THAT file.
//
// The failure they exist for is not hypothetical. The old code copied the
// replacement straight onto the live path — and any copy onto a path truncates
// it before the first byte is written — while taking its .bak on a best-effort
// basis. A disk full between the two produced: no .bak, a 0-byte .db, a
// SUCCESSFUL reopen (0 bytes is a valid empty SQLite database), an empty store
// reattached, and then the deferred resume() re-anchoring litestream against
// that empty file and making it the replica head. Three copies destroyed by one
// ENOSPC, and nothing in the process raised anything louder than a WARN.

// headOf reads the repo's agent-branch head through the live handle. It is how
// these tests tell WHICH database is in place, which raw bytes cannot: closing a
// store checkpoints its WAL, so the file differs after an aborted swap even
// though nothing was replaced.
func headOf(t *testing.T, ri *RepoInstance) string {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err, "the repo must still be serving")
	defer release()
	head, err := svc.Branches().HeadCommit(context.Background(), "machine/test")
	require.NoError(t, err)
	require.NotEmpty(t, head)
	return head
}

// headOfFile reads the agent-branch head straight from a database file, without
// going through the repo instance.
func headOfFile(t *testing.T, dbPath string) string {
	t.Helper()
	svc, err := store.Open(dbPath)
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.OpenRepo())
	head, err := svc.Branches().HeadCommit(context.Background(), "machine/test")
	require.NoError(t, err)
	require.NotEmpty(t, head)
	return head
}

// blockStagedPath makes the sibling file the replacement is staged into
// impossible to create. A non-empty directory is the reliable way to do it: an
// EMPTY one os.Remove would simply delete, which is precisely what the staging
// step does to a leftover from an interrupted swap.
func blockStagedPath(t *testing.T, ri *RepoInstance) {
	t.Helper()
	staged := ri.dbPath + ".knomit-swap"
	require.NoError(t, os.Mkdir(staged, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staged, "blocker"), nil, 0o644))
}

// TestSwapStoreRefusesWhenTheBackupCopyFails is the direct mutation test for the
// "best-effort .bak" decision. With the backup unwritable the old code logged a
// warning and went on to overwrite the live database anyway — the one step that
// has no way back. It must refuse instead, before touching anything.
func TestSwapStoreRefusesWhenTheBackupCopyFails(t *testing.T) {
	m, ri, tracker, tempDBPath := swapStoreFixture(t)
	before := headOf(t, ri)

	// A directory where the .bak has to go: os.Create fails deterministically,
	// with no need to fill a disk.
	require.NoError(t, os.Mkdir(ri.dbPath+".bak", 0o755))

	err := m.SwapStore(ri, tempDBPath)
	require.Error(t, err, "a swap that cannot take a backup first must fail, not proceed unprotected")
	require.Contains(t, err.Error(), "back up")

	require.Equal(t, before, headOf(t, ri),
		"the repo must still be serving the ORIGINAL database when the backup could not be taken")

	paused, resumed := tracker.counts()
	require.Equal(t, 1, paused)
	require.Equal(t, 1, resumed, "replication must be resumed even when the swap never started")
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	require.True(t, tracker.resumedSnaps[0].isDatabase(),
		"resume was handed %+v; litestream re-anchors on resume, so this becomes the replica head",
		tracker.resumedSnaps[0])
}

// TestSwapStoreLeavesTheLiveDatabaseIntactWhenStagingFails covers the other half
// of the guarantee: the replacement is assembled somewhere it can be thrown
// away, so failing to write it costs nothing. The old code had no staging step
// at all — the replacement WAS the live file — so this scenario truncated the
// database and only then failed.
func TestSwapStoreLeavesTheLiveDatabaseIntactWhenStagingFails(t *testing.T) {
	m, ri, tracker, tempDBPath := swapStoreFixture(t)
	before := headOf(t, ri)

	// Blocked one step later: the .bak is taken successfully and it is the
	// staging of the REPLACEMENT that fails. A non-empty directory in the staged
	// path's place is unremovable and uncreatable, so the swap cannot get past
	// it however it tries.
	blockStagedPath(t, ri)

	require.Error(t, m.SwapStore(ri, tempDBPath))
	require.Equal(t, before, headOf(t, ri),
		"the live database must never be truncated before a complete replacement exists")

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	require.True(t, tracker.resumedSnaps[0].isDatabase(),
		"resume was handed %+v after a failed staging", tracker.resumedSnaps[0])
}

// TestSwapStoreResumeNeverSeesATruncatedDatabase names the consequence rather
// than the mechanism, across every way the swap can fail.
//
// resume() runs reset_local_state and re-anchors, so whatever it finds becomes
// the new replica head. An empty file is a perfectly valid SQLite database, so
// there is no error anywhere on the path from "the swap broke" to "the backup is
// empty too" — which is why the assertion has to be made here, on the file the
// resume actually observed.
func TestSwapStoreResumeNeverSeesATruncatedDatabase(t *testing.T) {
	cases := map[string]func(t *testing.T, ri *RepoInstance) string{
		"backup unwritable": func(t *testing.T, ri *RepoInstance) string {
			require.NoError(t, os.Mkdir(ri.dbPath+".bak", 0o755))
			return ""
		},
		"staging blocked": func(t *testing.T, ri *RepoInstance) string {
			blockStagedPath(t, ri)
			return ""
		},
		"replacement missing": func(t *testing.T, ri *RepoInstance) string {
			return filepath.Join(t.TempDir(), "does-not-exist.db")
		},
	}
	for name, sabotage := range cases {
		t.Run(name, func(t *testing.T) {
			m, ri, tracker, tempDBPath := swapStoreFixture(t)
			before := headOf(t, ri)
			if override := sabotage(t, ri); override != "" {
				tempDBPath = override
			}

			require.Error(t, m.SwapStore(ri, tempDBPath))
			require.Equal(t, before, headOf(t, ri), "the original database must still be the one served")

			tracker.mu.Lock()
			defer tracker.mu.Unlock()
			require.Len(t, tracker.resumedSnaps, 1, "replication must be resumed on every failure path")
			require.True(t, tracker.resumedSnaps[0].isDatabase(),
				"resume observed %+v — litestream would snapshot that as the new replica head",
				tracker.resumedSnaps[0])
		})
	}
}

// TestSwapStoreSucceedsAndClearsItsScratchFiles pins the happy path against the
// new machinery: the replacement really is installed, and neither the staged
// file nor the backup is left behind for a later swap to adopt.
func TestSwapStoreSucceedsAndClearsItsScratchFiles(t *testing.T) {
	m, ri, tracker, tempDBPath := swapStoreFixture(t)
	before := headOf(t, ri)

	require.NoError(t, m.SwapStore(ri, tempDBPath))
	require.NotEqual(t, before, headOf(t, ri), "the replacement database must be the one now served")

	for _, scratch := range []string{ri.dbPath + ".bak", ri.dbPath + ".knomit-swap"} {
		_, err := os.Stat(scratch)
		require.True(t, os.IsNotExist(err), "%s survived a successful swap", scratch)
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	require.True(t, tracker.resumedSnaps[0].isDatabase())
}

// TestSwapStoreClearsAStaleStagedFile: a swap killed between staging and rename
// leaves a .knomit-swap behind. Renaming that into place on the next attempt
// would install a database nobody asked for, so it is cleared rather than
// trusted.
func TestSwapStoreClearsAStaleStagedFile(t *testing.T) {
	m, ri, _, tempDBPath := swapStoreFixture(t)

	require.NoError(t, os.WriteFile(ri.dbPath+".knomit-swap",
		[]byte("leftover from an interrupted swap"), 0o644))

	require.NoError(t, m.SwapStore(ri, tempDBPath))
	// The repo opened, which the 33-byte leftover would not have allowed.
	require.NotEmpty(t, headOf(t, ri))
}

// TestSwapStoreAbortsWhenASidecarCannotBeCleared pins the ORDER, not just the
// removal. A companion file left beside the old database is replayed into
// whatever file later appears under that name — a WAL header and a rollback
// journal both carry the frames and neither carries a database identity — so a
// sidecar that cannot be removed has to stop the swap BEFORE the rename, not be
// worked around after it.
//
// -journal rather than -wal because knomit opens in WAL mode: the live handle
// owns its -wal, so only the journal path can be blocked without racing it.
func TestSwapStoreAbortsWhenASidecarCannotBeCleared(t *testing.T) {
	m, ri, tracker, tempDBPath := swapStoreFixture(t)
	before := headOf(t, ri)

	journal := ri.dbPath + "-journal"
	require.NoError(t, os.Mkdir(journal, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(journal, "blocker"), nil, 0o644))

	require.Error(t, m.SwapStore(ri, tempDBPath),
		"a sidecar that cannot be removed must abort the swap, not be installed around")

	// Read the file directly rather than through the instance: SQLite refuses to
	// open a database whose journal path is unreadable, so the recovery reopen
	// failed too and the handle is (truthfully) detached. What matters is which
	// database is on disk.
	require.NoError(t, os.RemoveAll(journal))
	require.Equal(t, before, headOfFile(t, ri.dbPath), "the original database must still be the one on disk")

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	require.True(t, tracker.resumedSnaps[0].isDatabase())
}

// TestClearSQLiteSidecarsIsAllOrError pins the helper's contract directly: every
// companion file goes, an absent one is not an error, and one that cannot be
// removed is — because installing a database into a path that still holds
// foreign frames is the outcome the function exists to prevent.
func TestClearSQLiteSidecarsIsAllOrError(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "core.db")
	require.NoError(t, os.WriteFile(db, []byte("db"), 0o644))
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		require.NoError(t, os.WriteFile(db+suffix, []byte("x"), 0o644))
	}
	require.NoError(t, clearSQLiteSidecars(db))
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Stat(db + suffix)
		require.True(t, os.IsNotExist(err), "%s survived", suffix)
	}
	require.NoError(t, clearSQLiteSidecars(db), "absent sidecars are not an error")
	require.FileExists(t, db, "the database itself must not be touched")

	// A non-empty directory cannot be removed, so this stands in for the
	// unremovable sidecar.
	require.NoError(t, os.Mkdir(db+"-wal", 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(db+"-wal", "blocker"), nil, 0o644))
	require.Error(t, clearSQLiteSidecars(db))
}
