package repos

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// fakeBackupTracker records the pause/resume calls SwapStore makes and, at each
// one, the CONTENT of the database file. Recording the bytes (rather than only
// the call order) is what proves the placement: pause must observe the old file
// and resume the new one, which no ordering-only assertion can show.
// It also records Track/Untrack so the lifecycle tests can assert that a repo
// created after boot starts replicating immediately, rather than waiting for a
// restart it may not survive.
type fakeBackupTracker struct {
	dbPath string

	mu        sync.Mutex
	paused    []string // db content seen at each Pause
	resumed   []string // db content seen at each resume
	tracked   map[string]string
	untracked []string
	trackErr  error
	pauseErr  error
	resumeErr error
}

func (f *fakeBackupTracker) Track(name, dbPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.trackErr != nil {
		return f.trackErr
	}
	if f.tracked == nil {
		f.tracked = map[string]string{}
	}
	f.tracked[name] = dbPath
	return nil
}

func (f *fakeBackupTracker) Untrack(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.untracked = append(f.untracked, name)
	delete(f.tracked, name)
	return nil
}

// trackedPath returns the path the named repo is replicating from, or "".
func (f *fakeBackupTracker) trackedPath(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tracked[name]
}

func (f *fakeBackupTracker) untrackedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.untracked...)
}

func (f *fakeBackupTracker) Pause(string) (func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pauseErr != nil {
		return func() error { return nil }, f.pauseErr
	}
	f.paused = append(f.paused, f.content())
	return func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.resumed = append(f.resumed, f.content())
		return f.resumeErr
	}, nil
}

// content fingerprints the database file as it stands right now.
func (f *fakeBackupTracker) content() string {
	b, err := os.ReadFile(f.dbPath)
	if err != nil {
		return "missing: " + err.Error()
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func (f *fakeBackupTracker) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paused), len(f.resumed)
}

// swapStoreFixture builds a manager with one real repo plus a fake backup
// tracker, and returns the repo instance and a valid replacement database.
func swapStoreFixture(t *testing.T) (*Manager, *RepoInstance, *fakeBackupTracker, string) {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	writeTestKey(t, keyPath)

	tracker := &fakeBackupTracker{}
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "machine/test",
		KeyPath:     keyPath,
		Backup:      tracker,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)
	tracker.dbPath = ri.dbPath
	require.NotEmpty(t, tracker.dbPath, "fixture needs a file-backed repo")

	tempDBPath := filepath.Join(t.TempDir(), "clone.db")
	clone, err := store.Open(tempDBPath)
	require.NoError(t, err)
	require.NoError(t, clone.InitRepo(map[string]string{}, "machine/test"))
	require.NoError(t, clone.Checkpoint())
	require.NoError(t, clone.Close())

	return m, ri, tracker, tempDBPath
}

// TestSwapStorePausesBeforeAndResumesAfterTheSwap pins the placement: the
// replica must be paused while the file still holds the OLD database and
// resumed only once the NEW one is in place. Resuming too early re-registers a
// database whose file is mid-swap.
func TestSwapStorePausesBeforeAndResumesAfterTheSwap(t *testing.T) {
	m, ri, tracker, tempDBPath := swapStoreFixture(t)

	before := tracker.content()
	require.NoError(t, m.SwapStore(ri, tempDBPath))
	after := tracker.content()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	require.Equal(t, []string{before}, tracker.paused, "Pause must run before the file is replaced")
	require.Equal(t, []string{after}, tracker.resumed, "resume must run after the reopen, on the new file")
	require.NotEqual(t, before, after, "fixture precondition: the swap must change the file")
}

// TestSwapStoreResumesOnFailurePath is the one that matters operationally. A
// swap that fails after the pause must still resume replication: otherwise the
// repo is left silently unreplicated, and nothing in the returned error even
// mentions backup.
func TestSwapStoreResumesOnFailurePath(t *testing.T) {
	m, ri, tracker, _ := swapStoreFixture(t)

	err := m.SwapStore(ri, filepath.Join(t.TempDir(), "does-not-exist.db"))
	require.Error(t, err, "swapping in a missing temp DB must fail")

	paused, resumed := tracker.counts()
	require.Equal(t, 1, paused)
	require.Equal(t, 1, resumed, "resume must run on the error path too, or the repo stops being backed up")
}

// TestSwapStoreFailsWhenPauseFails: if replication cannot be paused, the file
// must not be swapped underneath a live replica.
func TestSwapStoreFailsWhenPauseFails(t *testing.T) {
	m, ri, tracker, tempDBPath := swapStoreFixture(t)
	tracker.pauseErr = errors.New("boom")

	before := tracker.content()
	err := m.SwapStore(ri, tempDBPath)
	require.Error(t, err)
	require.Equal(t, before, tracker.content(), "the database file must be untouched when the pause fails")
}

// TestSwapStoreSurfacesResumeFailure: a swap that succeeds but leaves the repo
// unreplicated is not a success.
func TestSwapStoreSurfacesResumeFailure(t *testing.T) {
	m, ri, tracker, tempDBPath := swapStoreFixture(t)
	tracker.resumeErr = errors.New("boom")

	err := m.SwapStore(ri, tempDBPath)
	require.ErrorContains(t, err, "resume replication")
}

// TestSwapStoreWithoutBackupNeedsNoNilChecks: a nil Deps.Backup means backup is
// disabled, and SwapStore behaves exactly as before.
func TestSwapStoreWithoutBackupNeedsNoNilChecks(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	writeTestKey(t, keyPath)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "machine/test",
		KeyPath:     keyPath,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)

	tempDBPath := filepath.Join(t.TempDir(), "clone.db")
	clone, err := store.Open(tempDBPath)
	require.NoError(t, err)
	require.NoError(t, clone.InitRepo(map[string]string{}, "machine/test"))
	require.NoError(t, clone.Checkpoint())
	require.NoError(t, clone.Close())

	require.NoError(t, m.SwapStore(ri, tempDBPath))
}
