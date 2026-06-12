package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

func newLifecycleManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "machine/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestCreate_PresetMode_RegistersRepo(t *testing.T) {
	m := newLifecycleManager(t)
	var steps []string
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, func(e Event) { steps = append(steps, e.Step) })
	require.NoError(t, err)
	require.NotNil(t, ri)
	require.Equal(t, "work", ri.Name())
	require.NotNil(t, m.Get("work"))
	require.Contains(t, steps, "done")
}

func TestCreate_RejectsExistingActiveName(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: config.DefaultRepoName, Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.ErrorIs(t, err, ErrRepoExists)
}

func TestCreate_RejectsInvalidName(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "Bad Name", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.ErrorIs(t, err, ErrInvalidName)
}

func TestCreate_CustomOntology(t *testing.T) {
	m := newLifecycleManager(t)
	y, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "cust", Mode: "custom", OntologyYAML: string(y),
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, ri)
}

func TestActiveRepoWithOrigin_EmptyWhenNone(t *testing.T) {
	m := newLifecycleManager(t)
	require.Equal(t, "", m.ActiveRepoWithOrigin("https://example.com/x.git"))
}

func TestArchive_MovesFileAndUnregisters(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)

	info, err := m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, "work", info.Name)
	require.NotEmpty(t, info.ID)
	require.Nil(t, m.Get("work"), "archived repo must be unregistered")

	archived, err := m.ListArchived()
	require.NoError(t, err)
	require.Len(t, archived, 1)
	require.Equal(t, "work", archived[0].Name)
	require.Equal(t, info.ID, archived[0].ID)
}

func TestArchive_BlocksDefaultRepo(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Archive(config.DefaultRepoName)
	require.ErrorIs(t, err, ErrCannotArchiveDefault)
}

func TestArchive_NotFound(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Archive("nope")
	require.ErrorIs(t, err, ErrRepoNotFound)
}

func TestRestore_BringsBackAndUnarchives(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	info, err := m.Archive("work")
	require.NoError(t, err)

	ri, err := m.Restore(info.ID, "")
	require.NoError(t, err)
	require.Equal(t, "work", ri.Name())
	require.NotNil(t, m.Get("work"))

	left, _ := m.ListArchived()
	require.Empty(t, left)
}

func TestRestore_RenameOnCollision(t *testing.T) {
	m := newLifecycleManager(t)
	_, _ = m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	info, _ := m.Archive("work")
	_, _ = m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)

	_, err := m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrRepoExists)

	ri, err := m.Restore(info.ID, "work2")
	require.NoError(t, err)
	require.Equal(t, "work2", ri.Name())
}

func TestPurge_RemovesArchive(t *testing.T) {
	m := newLifecycleManager(t)
	_, _ = m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	info, _ := m.Archive("work")
	require.NoError(t, m.Purge(info.ID))
	left, _ := m.ListArchived()
	require.Empty(t, left)
	require.ErrorIs(t, m.Purge(info.ID), ErrArchiveNotFound)
}

// TestArchive_DefaultStillBlocked documents that with only trunk present,
// Archive(trunk) returns ErrCannotArchiveDefault — the default-repo check
// fires before the last-repo guard, so ErrCannotArchiveLast is never reached
// via the trunk path. This is the realistic reachable behavior.
func TestArchive_DefaultStillBlocked(t *testing.T) {
	m := newLifecycleManager(t)
	require.Len(t, m.Names(), 1) // only trunk
	_, err := m.Archive(config.DefaultRepoName)
	require.ErrorIs(t, err, ErrCannotArchiveDefault)
}

// TestArchive_LastNonDefault_StillSucceeds documents that archiving the last
// NON-default repo succeeds, leaving only trunk. ErrCannotArchiveLast does not
// fire here because len(m.repos)==2 (trunk + work) at the time of the check.
func TestArchive_LastNonDefault_StillSucceeds(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	require.Len(t, m.Names(), 2)

	_, err = m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, []string{config.DefaultRepoName}, m.Names())
}

// TestArchive_BlocksLastActiveRepo constructs the ErrCannotArchiveLast
// condition directly. In normal operation it is unreachable because trunk is
// always present and is rejected by the default-repo guard first (see
// TestArchive_DefaultStillBlocked). To exercise the defensive guard we use
// in-package access to make the map contain exactly one non-default repo, then
// confirm Archive refuses to remove it.
func TestArchive_BlocksLastActiveRepo(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)

	// Drop trunk from the map so "work" is the only (non-default) repo. This is
	// in-package access; it is the only way to reach len(m.repos)<=1 with a
	// non-default name, since the default-repo check would otherwise fire first.
	// Capture the trunk instance so we can re-register it for clean teardown.
	m.mu.Lock()
	trunk := m.repos[config.DefaultRepoName]
	delete(m.repos, config.DefaultRepoName)
	m.mu.Unlock()
	require.Equal(t, []string{"work"}, m.Names())

	_, err = m.Archive("work")
	require.ErrorIs(t, err, ErrCannotArchiveLast)
	// Guard must NOT have removed the repo from the map.
	require.NotNil(t, m.Get("work"))

	// Re-register trunk so the manager's Close cleanup tears it down too.
	m.Set(config.DefaultRepoName, trunk)
}

// TestRestore_RefusesExistingDestFile guards against the leftover-db case: a
// db file already at the destination path (from a prior failed restore) must
// not be clobbered. Restore should refuse with ErrRepoExists.
func TestRestore_RefusesExistingDestFile(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	info, err := m.Archive("work")
	require.NoError(t, err)

	// Plant a stray db file at the destination path and remove the active
	// registration so the m.Get(target) check passes but the on-disk guard
	// trips.
	dstDB := filepath.Join(m.deps.Cfg.Home, "repos", "work.db")
	require.NoError(t, os.WriteFile(dstDB, []byte("stray"), 0o644))

	_, err = m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrRepoExists)

	// The archive manifest must be untouched (not deleted) so the repo stays
	// recoverable.
	left, _ := m.ListArchived()
	require.Len(t, left, 1)
}

// TestArchive_PersistsOrigin verifies the origin captured at archive time is
// the one persisted in the store's remote record — exercising the WithRead +
// SetRemote round-trip that ActiveRepoWithOrigin and Archive both rely on.
func TestArchive_PersistsOrigin(t *testing.T) {
	m := newLifecycleManager(t)
	ri, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)

	const originURL = "https://example.com/origin.git"
	ri.WithRead(func(svc *store.Service) {
		require.NotNil(t, svc)
		require.NoError(t, svc.Remote().SetRemote(
			"origin", originURL, "main", m.deps.AgentBranch, 300, 300, "", "",
		))
	})

	// ActiveRepoWithOrigin should now find "work" by its origin URL.
	require.Equal(t, "work", m.ActiveRepoWithOrigin(originURL))

	info, err := m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, originURL, info.Origin)
}

// TestRestore_OriginInUse verifies a restore is refused when the archived
// repo's origin matches an active repo's origin. Persists an origin on a live
// active repo, archives a second repo carrying the same origin, then asserts
// Restore returns ErrOriginInUse.
func TestRestore_OriginInUse(t *testing.T) {
	m := newLifecycleManager(t)
	const originURL = "https://example.com/shared.git"

	// Active repo "keeper" holds the origin.
	keeper, err := m.Create(context.Background(), CreateSpec{Name: "keeper", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	keeper.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Remote().SetRemote(
			"origin", originURL, "main", m.deps.AgentBranch, 300, 300, "", "",
		))
	})

	// Second repo "work" also carries the same origin, then gets archived.
	work, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	work.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Remote().SetRemote(
			"origin", originURL, "main", m.deps.AgentBranch, 300, 300, "", "",
		))
	})
	info, err := m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, originURL, info.Origin)

	_, err = m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrOriginInUse)
}
