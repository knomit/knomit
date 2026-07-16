package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

func TestManager_Registry_NilBeforeStart(t *testing.T) {
	m := New(context.Background(), Deps{Cfg: config.Config{}, AgentBranch: "agent/test"})
	require.Nil(t, m.Registry())
}

func TestManager_Registry_OpenAfterStartAndUsable(t *testing.T) {
	m := newLifecycleManager(t)
	reg := m.Registry()
	require.NotNil(t, reg)

	// control.db lives at <home>/control.db (NOT under repos/).
	// newLifecycleManager sets Cfg.Home; recover it from the manager's deps.
	_, err := os.Stat(filepath.Join(m.deps.Cfg.Home, "control.db"))
	require.NoError(t, err)

	stored, err := reg.Create(Lens{Name: "eng", Write: "core", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	got, ok, err := reg.Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, stored, got)
}

func TestArchive_BlockedWhileLensReferencesRepo(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)

	_, err = m.Registry().Create(Lens{Name: "eng", Write: "work", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)

	_, err = m.Archive("work")
	require.ErrorIs(t, err, ErrRepoInUseByLens)
	require.NotNil(t, m.Get("work"), "repo must stay registered when the guard blocks")

	// Deleting the lens unblocks archiving.
	require.NoError(t, m.Registry().Delete("eng"))
	info, err := m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, "work", info.Name)
}
