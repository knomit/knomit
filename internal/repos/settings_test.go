package repos

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

func TestRepoSettings_DefaultAndRoundTrip(t *testing.T) {
	s, err := OpenRepoSettings(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	p, err := s.Profile("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	require.NoError(t, err)
	require.Equal(t, "code", p, "absent row defaults to code")

	require.NoError(t, s.SetProfile("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "chat"))
	p, err = s.Profile("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	require.NoError(t, err)
	require.Equal(t, "chat", p)

	// Upsert overwrites.
	require.NoError(t, s.SetProfile("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "generic"))
	p, _ = s.Profile("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	require.Equal(t, "generic", p)
}

func TestRepoSettings_Validation(t *testing.T) {
	s, err := OpenRepoSettings(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	require.Error(t, s.SetProfile("id1", "bogus"), "unknown profile rejected")
	require.Error(t, s.SetProfile("", "code"), "empty repo id rejected")

	p, err := s.Profile("")
	require.NoError(t, err)
	require.Equal(t, "code", p, "empty id reads as default, never errors")
}

// The tenant shares control.db with the lens registry: both open the same
// file without clobbering each other's tables.
func TestRepoSettings_CoexistsWithLensRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	reg, err := OpenLensRegistry(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })
	s, err := OpenRepoSettings(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = reg.Create(Lens{Name: "eng", Write: "core", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	require.NoError(t, s.SetProfile("someid", "chat"))

	_, ok, err := reg.Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	p, err := s.Profile("someid")
	require.NoError(t, err)
	require.Equal(t, "chat", p)
}

func TestManager_Settings_LifecycleAndNilBeforeStart(t *testing.T) {
	m := New(context.Background(), Deps{Cfg: config.Config{}, AgentBranch: "agent/test"})
	require.Nil(t, m.Settings())

	started := newLifecycleManager(t)
	require.NotNil(t, started.Settings())
	require.NoError(t, started.Settings().SetProfile("abc123", "chat"))
	p, err := started.Settings().Profile("abc123")
	require.NoError(t, err)
	require.Equal(t, "chat", p)
}
