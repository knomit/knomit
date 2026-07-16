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
