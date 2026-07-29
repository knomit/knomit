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

// TestPurgeUntracksTheRepo: purge is the one permanent deletion in the
// lifecycle, so a tracked entry must not outlive the file.
func TestPurgeUntracksTheRepo(t *testing.T) {
	m, tracker, _ := backupLifecycleFixture(t)
	mustCreateRepo(t, m, "doomed")

	info, err := m.Archive("doomed")
	require.NoError(t, err)
	require.NoError(t, m.Purge(info.ID))

	require.Contains(t, tracker.untrackedNames(), "doomed")
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

	require.NoError(t, m.Purge(info.ID))

	require.Equal(t, want, tracker.trackedPath("recycled"),
		"purging the archive stopped replication for the LIVE repo that reclaimed the name")
	require.NotContains(t, tracker.untrackedNames(), "recycled")
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
