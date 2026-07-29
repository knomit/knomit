package repos

import (
	"context"
	"errors"
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
