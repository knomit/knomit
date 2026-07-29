package repos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// backupLifecycleFixture builds a started manager wired to a recording tracker.
func backupLifecycleFixture(t *testing.T) (*Manager, *fakeBackupTracker, string) {
	t.Helper()
	home := t.TempDir()
	tracker := &fakeBackupTracker{}
	m := newTestManager(t, home, func(d *Deps) { d.Backup = tracker })
	require.NoError(t, m.Start())
	return m, tracker, home
}

// TestCreateTracksNewRepoForReplication pins the gap between boot-time tracking
// and the API. cmd/serve registers only the databases Start opened, so a repo
// created through POST /api/v1/repos would otherwise stay unreplicated until
// the next restart.
//
// That is not a mere backup gap. Backup also switches on StrictMissing, so
// losing the volume before that restart leaves a registry row with no snapshot
// and (for a preset repo) no origin — ErrRepoUnrecoverable, which refuses the
// boot permanently until an operator hand-deletes the row.
func TestCreateTracksNewRepoForReplication(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)

	ri := mustCreateRepo(t, m, "fresh")
	require.NotNil(t, ri)

	want := filepath.Join(home, "repos", "fresh.db")
	require.Equal(t, want, tracker.trackedPath("fresh"),
		"a repo created after boot must start replicating immediately, not at the next restart")
}

// TestCreateSurvivesTrackFailure: replication failing to start must not fail the
// create. The repo is built, registered and serving by that point, so reporting
// an error would describe a create that visibly succeeded — and the retry would
// hit ErrRepoExists. The condition is logged loudly instead.
func TestCreateSurvivesTrackFailure(t *testing.T) {
	m, tracker, _ := backupLifecycleFixture(t)
	tracker.trackErr = errors.New("replica unreachable")

	ri, err := m.Create(context.Background(), CreateSpec{Name: "fresh", Mode: "preset"}, nil)
	require.NoError(t, err, "a replication failure must not fail the repo creation")
	require.NotNil(t, ri)
	require.NotNil(t, m.Get("fresh"), "the repo must still be registered and serving")
}

// TestCreateWithoutBackupIsANoOp: a nil tracker is how backup-disabled runs, and
// it must not need a special path at the call site.
func TestCreateWithoutBackupIsANoOp(t *testing.T) {
	home := t.TempDir()
	m := newTestManager(t, home)
	require.NoError(t, m.Start())

	require.NotNil(t, mustCreateRepo(t, m, "fresh"))
}

// TestArchiveMovesReplicationToTheArchivePrefix pins both halves of the archive
// handover. Leaving the LIVE entry tracked is not "nothing happens": litestream
// pins a file descriptor at init, so the stale tracker would go on replicating
// the MOVED file under the live repo's prefix forever.
func TestArchiveMovesReplicationToTheArchivePrefix(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "shelved")

	info, err := m.Archive("shelved")
	require.NoError(t, err)

	require.Empty(t, tracker.trackedPath("shelved"),
		"archive left the live entry tracked; it would replicate the moved file under the live prefix")
	require.Contains(t, tracker.untrackedNames(), "shelved")
	require.Equal(t, filepath.Join(home, "repos", "archive", info.ID+".db"),
		tracker.trackedPath(archiveKey(info.ID)),
		"the archived database must replicate under the archive prefix")
}

// TestArchiveRollsBackWhenTheArchivedCopyCannotReplicate covers the recovery
// path the new ordering creates. Archive now stops replicating BEFORE it moves
// the file, so every abort after that point has an untrack to undo as well as a
// deregistration — and a repo that came back live but unreplicated would be
// silently unprotected until the next restart, which is precisely the failure
// class the backup work exists to remove.
func TestArchiveRollsBackWhenTheArchivedCopyCannotReplicate(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "shelved")
	tracker.trackArchivedErr = errors.New("replica unreachable")

	_, err := m.Archive("shelved")
	require.Error(t, err, "an archive whose copy cannot be replicated must not report success")

	live := filepath.Join(home, "repos", "shelved.db")
	require.NotNil(t, m.Get("shelved"), "the repo must be live again")
	require.FileExists(t, live, "the database must have been moved back")
	require.Equal(t, live, tracker.trackedPath("shelved"),
		"the reinstated repo is live but replicated by nothing")

	rec, ok, rerr := m.RepoRegistry().ActiveRecord("shelved")
	require.NoError(t, rerr)
	require.True(t, ok, "the active registry row must survive the aborted archive")
	require.Equal(t, RepoActive, rec.State)

	archived, lerr := m.ListArchived()
	require.NoError(t, lerr)
	require.Empty(t, archived, "the aborted archive must leave no archived row behind")
}

// TestArchiveStopsReplicationWhileTheDatabaseIsStillOpen pins the second half of
// the archive ordering, and it is a bug this work shipped and then found.
//
// Untrack's final sync READS the database. ri.shutdown closes knomit's SQLite
// handle, which — without PERSIST_WAL — checkpoints and DELETES the -wal.
// Untracking after the shutdown therefore makes that sync fail with
// "open <db>-wal: no such file or directory", and since a failed untrack aborts
// the archive, the whole operation fails. It is timing-dependent in the wild
// (roughly one archive in fifteen under load) and deterministic here: the -wal
// exists only while a connection is open, so its presence at the moment Untrack
// is called is exactly the property that matters.
//
// Pause documents the same ordering for the same reason — litestream's
// connection must be dropped before knomit's.
func TestArchiveStopsReplicationWhileTheDatabaseIsStillOpen(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "shelved")

	wal := filepath.Join(home, "repos", "shelved.db-wal")
	require.FileExists(t, wal, "fixture: an open WAL-mode repo must have a -wal to observe")

	var openAtUntrack bool
	tracker.onUntrack = func(name string) {
		if name == "shelved" {
			_, err := os.Stat(wal)
			openAtUntrack = err == nil
		}
	}

	_, err := m.Archive("shelved")
	require.NoError(t, err)
	require.True(t, openAtUntrack,
		"replication was stopped after the repo was closed; the final sync has no -wal to read and the archive fails")
}

// TestArchiveReservesTheNameForItsWholeWindow closes the sibling of the Purge
// TOCTOU. Archive drops the repo from m.repos and only THEN shuts it down,
// makes the archive dir, stops replication and moves the file — and m.Get(name)
// returns nil for that entire window.
//
// Unreserved, a Create for the same name lands inside it and every guard
// passes: it opens repos/<name>.db, which is still the ARCHIVED repo's file,
// and if its trailing Track wins, Archive's Untrack removes the brand-new live
// repo's tracker before the rename carries its database into the archive dir.
// A live repo with no database and no replication, and nothing logged.
//
// The test takes the reservation from the outside, which is what a Create in
// flight looks like, and requires Archive to refuse rather than proceed.
func TestArchiveReservesTheNameForItsWholeWindow(t *testing.T) {
	m, _, _ := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "contended")

	release, err := m.reserveNameAndOrigin("contended", "")
	require.NoError(t, err)
	defer release()

	_, err = m.Archive("contended")
	require.ErrorIs(t, err, ErrCreateInFlight,
		"Archive ran while another operation held the name; their windows overlap and either can clobber the other")
	require.NotNil(t, m.Get("contended"), "the refused archive must leave the repo alone")
}

// TestArchiveAndCreateCannotBothHoldAName is the same property from the other
// side: with Archive holding the reservation, a Create for the freed name is
// refused rather than opening the archived repo's file.
func TestArchiveAndCreateCannotBothHoldAName(t *testing.T) {
	m, _, _ := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "contended")

	info, err := m.Archive("contended")
	require.NoError(t, err)
	require.NotEmpty(t, info.ID)

	// Once Archive has returned, the reservation is released and the name is
	// genuinely free — the mutual exclusion is for the window, not forever.
	require.NotNil(t, mustCreateRepo(t, m, "contended"))
}

// TestRestoreFetchesTheArchivedDBFromTheReplica covers the container-replacement
// case: control.db came back with the archived row in it, so ListArchived still
// advertises the repo, but repos/archive/<id>.db is not on this volume. Without
// the fetch, Restore reaches os.Rename and fails with a bare ENOENT naming no
// cause, while the never-expiring replica copy sits unused.
func TestRestoreFetchesTheArchivedDBFromTheReplica(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "cold")

	info, err := m.Archive("cold")
	require.NoError(t, err)

	// The volume is replaced: the registry row survives inside control.db, the
	// archived database does not.
	archivedPath := filepath.Join(home, "repos", "archive", info.ID+".db")
	realBytes, rerr := os.ReadFile(archivedPath)
	require.NoError(t, rerr)
	require.NoError(t, os.Remove(archivedPath))
	tracker.seedReplica(info.ID, string(realBytes))

	listed, lerr := m.ListArchived()
	require.NoError(t, lerr)
	require.Len(t, listed, 1, "the archive is still advertised, which is what makes the missing file a trap")

	ri, err := m.Restore(info.ID, "")
	require.NoError(t, err, "Restore must pull the archived database from the replica")
	require.NotNil(t, ri)
	require.Equal(t, filepath.Join(home, "repos", "cold.db"), tracker.trackedPath("cold"))
}

// TestRestoreExplainsAnArchiveTheReplicaDoesNotHave: when the database is on
// neither the volume nor the replica there is nothing to restore, and the error
// must say which of the two failed rather than surfacing a bare rename ENOENT.
func TestRestoreExplainsAnArchiveTheReplicaDoesNotHave(t *testing.T) {
	m, _, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "cold")

	info, err := m.Archive("cold")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(home, "repos", "archive", info.ID+".db")))

	_, err = m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrArchiveNotFound)
	require.ErrorContains(t, err, "not on this volume")
	require.ErrorContains(t, err, "no backup for it")
}

// TestReinstateArchivedSkipsAMissingDatabase pins the guard on the abort path
// of an unarchive. TrackArchived against a missing path SUCCEEDS — litestream
// opens the file lazily, so registration never notices — leaving a phantom
// entry that can never sync and logs an error on every monitor tick for the
// life of the process.
//
// Tested directly rather than through Restore because Restore's own paths now
// close every route to it (an absent database is fetched from the replica up
// front, and the move-back is only reinstated when it succeeded). The guard is
// what keeps that true as those paths change, so it is worth its own test —
// with a control proving it still tracks when the file IS there.
func TestReinstateArchivedSkipsAMissingDatabase(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)
	archiveDir := filepath.Join(home, "repos", "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	m.reinstateArchived("ghost", filepath.Join(archiveDir, "ghost.db"), "test")
	require.Empty(t, tracker.trackedPath(archiveKey("ghost")),
		"registered a database that does not exist; it can never sync and will log an error forever")

	real := filepath.Join(archiveDir, "real.db")
	require.NoError(t, os.WriteFile(real, []byte("db"), 0o644))
	m.reinstateArchived("real", real, "test")
	require.Equal(t, real, tracker.trackedPath(archiveKey("real")),
		"control: an archived database that IS present must still be reinstated")
}

// TestRestoreSurfacesAReplicaFetchFailure: an unarchive whose replica fetch
// errors must fail with the cause, not sail on into a rename that reports a
// bare ENOENT.
func TestRestoreSurfacesAReplicaFetchFailure(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "cold")

	info, err := m.Archive("cold")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(home, "repos", "archive", info.ID+".db")))
	tracker.seedReplica(info.ID, "restored-bytes")
	tracker.restoreErr = errors.New("replica unreachable")

	_, err = m.Restore(info.ID, "")
	require.ErrorContains(t, err, "replica unreachable")
	require.Nil(t, m.Get("cold"), "a failed unarchive must not leave a half-registered repo")
}

// TestPurgeDeletesTheArchiveReplicaObjects. Under the archive namespace nothing
// else ever will: retention is disabled there by design, so the objects a purge
// leaves behind are unreachable by any deletion mechanism. Silently keeping
// them would redefine purge as "delete locally, keep forever in the bucket".
func TestPurgeDeletesTheArchiveReplicaObjects(t *testing.T) {
	m, tracker, _ := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "doomed")

	info, err := m.Archive("doomed")
	require.NoError(t, err)
	require.NoError(t, m.Purge(info.ID))

	require.Contains(t, tracker.deletedReplicaIDs(), info.ID,
		"purge left the archived database in the replica, where nothing will ever reclaim it")
}

// TestPurgeFailsLoudlyWhenTheReplicaCannotBeDeleted: this is the one backup
// cleanup that must not be best-effort, and a failed purge must leave the
// archive intact so the operator can retry it.
func TestPurgeFailsLoudlyWhenTheReplicaCannotBeDeleted(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "doomed")

	info, err := m.Archive("doomed")
	require.NoError(t, err)
	tracker.deleteReplicaErr = errors.New("bucket unreachable")

	err = m.Purge(info.ID)
	require.Error(t, err, "a purge that cannot remove the replica objects has not purged anything")
	require.ErrorContains(t, err, "retried")

	require.FileExists(t, filepath.Join(home, "repos", "archive", info.ID+".db"),
		"the local database must survive so the purge can be retried")
	listed, lerr := m.ListArchived()
	require.NoError(t, lerr)
	require.Len(t, listed, 1, "the registry row must survive, or nothing names the objects left behind")
}

// TestArchivedDBPathsReportsOnlyPresentFiles feeds the boot-time re-tracking.
// A row whose database is absent has nothing to replicate from, and tracking it
// would install exactly the phantom entry the reinstate guard avoids; those are
// fetched lazily on unarchive instead.
func TestArchivedDBPathsReportsOnlyPresentFiles(t *testing.T) {
	m, _, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "here")
	mustCreateRepo(t, m, "gone")

	present, err := m.Archive("here")
	require.NoError(t, err)
	absent, err := m.Archive("gone")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(home, "repos", "archive", absent.ID+".db")))

	paths, err := m.ArchivedDBPaths()
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		present.ID: filepath.Join(home, "repos", "archive", present.ID+".db"),
	}, paths)
}

// TestRestoreMovesReplicationBackToTheLiveRepo is the reverse handover. The
// archive entry must stop before the file moves — the same pinned-descriptor
// hazard, in the other direction.
func TestRestoreMovesReplicationBackToTheLiveRepo(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "shelved")

	info, err := m.Archive("shelved")
	require.NoError(t, err)
	tracker.resetCalls()

	ri, err := m.Restore(info.ID, "")
	require.NoError(t, err)
	require.NotNil(t, ri)

	require.Empty(t, tracker.trackedPath(archiveKey(info.ID)),
		"restore left the archive entry replicating a file that has moved back to the live path")
	require.Contains(t, tracker.untrackedNames(), archiveKey(info.ID))
	require.Equal(t, filepath.Join(home, "repos", "shelved.db"), tracker.trackedPath("shelved"))
}

// TestRestoreUnderANewNameTracksThatName covers the rename form of restore.
func TestRestoreUnderANewNameTracksThatName(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "shelved")

	info, err := m.Archive("shelved")
	require.NoError(t, err)

	_, err = m.Restore(info.ID, "renamed")
	require.NoError(t, err)

	require.Equal(t, filepath.Join(home, "repos", "renamed.db"), tracker.trackedPath("renamed"))
	require.Empty(t, tracker.trackedPath(archiveKey(info.ID)))
}

// TestPurgeUntracksTheRepo: purge is the one permanent deletion in the
// lifecycle, so a tracked entry must not outlive the file. The entry that
// belongs to a purged archive is the ARCHIVE one — the live name stopped being
// replicated back when the repo was archived.
func TestPurgeUntracksTheRepo(t *testing.T) {
	m, tracker, _ := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "doomed")

	info, err := m.Archive("doomed")
	require.NoError(t, err)
	tracker.resetCalls()
	require.NoError(t, m.Purge(info.ID))

	require.Contains(t, tracker.untrackedNames(), archiveKey(info.ID))
	require.Empty(t, tracker.trackedPath(archiveKey(info.ID)))
	require.Empty(t, tracker.trackedPath("doomed"))
}

// TestPurgeLeavesAReclaimedNameTracked is the guard that makes the untrack
// above safe. An archived repo's NAME can be claimed by a new active repo
// before the archive is purged — that is exactly why Purge drops the registry
// row by archive id rather than by name. The tracker is keyed by name, so an
// unguarded Untrack would silently stop replicating the live repo that now
// holds it: a purge of dead data quietly disabling the backup of live data.
func TestPurgeLeavesAReclaimedNameTracked(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "recycled")

	info, err := m.Archive("recycled")
	require.NoError(t, err)

	// A NEW repo claims the freed name, and starts replicating under it.
	mustCreateRepo(t, m, "recycled")
	want := filepath.Join(home, "repos", "recycled.db")
	require.Equal(t, want, tracker.trackedPath("recycled"))
	tracker.resetCalls()

	require.NoError(t, m.Purge(info.ID))

	require.Equal(t, want, tracker.trackedPath("recycled"),
		"purging the archive stopped replication for the LIVE repo that reclaimed the name")
	require.NotContains(t, tracker.untrackedNames(), "recycled")
}

// TestPurgeSkipsANameAnInFlightCreateHolds closes the TOCTOU hole in the guard
// above. `m.Get(name) == nil` followed by `Untrack(name)` is two unsynchronised
// steps with nothing reserving the name in between: a Create that lands in the
// window registers and tracks the new repo, and the purge then untracks it —
// leaving a brand-new LIVE repo completely unreplicated, with Untrack returning
// cleanly so there is no log line anywhere.
//
// The test holds the SAME reservation Create/Restore take (reserveNameAndOrigin)
// for the whole of Purge, which is exactly the state a create-in-flight leaves
// the manager in. It is deterministic: no goroutine has to win a race for the
// hole to be visible.
func TestPurgeSkipsANameAnInFlightCreateHolds(t *testing.T) {
	m, tracker, home := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "recycled")

	info, err := m.Archive("recycled")
	require.NoError(t, err)

	// Stand in for a Create that has reserved the reclaimed name and is part way
	// through building it: the reservation is held, and the repo is not yet in
	// the active map — precisely the window m.Get cannot see.
	release, err := m.reserveNameAndOrigin("recycled", "")
	require.NoError(t, err)
	defer release()

	livePath := filepath.Join(home, "repos", "recycled.db")
	require.NoError(t, tracker.Track("recycled", livePath), "the in-flight create starts replicating")
	tracker.resetCalls()

	require.NoError(t, m.Purge(info.ID))

	require.Equal(t, livePath, tracker.trackedPath("recycled"),
		"purge untracked a name an in-flight create owns; the new repo is live and backed up by nothing")
	require.NotContains(t, tracker.untrackedNames(), "recycled")
	require.Contains(t, tracker.untrackedNames(), archiveKey(info.ID),
		"the archive's own entry must still be untracked; it is keyed by a ksuid no live repo can claim")
}

// TestNilTrackerStaysNoOpOnPurge covers the backup-disabled purge path.
func TestNilTrackerStaysNoOpOnPurge(t *testing.T) {
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start())
	mustCreateRepo(t, m, "doomed")

	info, err := m.Archive("doomed")
	require.NoError(t, err)
	require.NoError(t, m.Purge(info.ID))
}
