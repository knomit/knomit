package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestStart_freshHomeHasNoRepos pins the first-run contract: Start succeeds on
// an empty home and registers NOTHING. knomit has no default repo — not "core",
// not any other name — so a fresh install serves zero repos until the user
// creates one, and zero is a healthy state rather than a failure to boot.
func TestStart_freshHomeHasNoRepos(t *testing.T) {
	dir := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "machine/test",
	})
	require.NoError(t, m.Start(), "an empty home must boot, not error")
	t.Cleanup(func() { _ = m.Close() })

	require.Empty(t, m.Names(), "a fresh home must register no repos")
	require.Nil(t, m.Get("core"), `"core" must not be conjured into existence`)

	// The empty manager is fully functional: a repo created now registers
	// normally, and a second Start over the same home re-opens it.
	createRepo(t, m, "work")
	require.Equal(t, []string{"work"}, m.Names())
}

// TestManager_Set_EvictsStaleUID pins the fix for a review finding: replacing
// the instance registered under a name with one carrying a DIFFERENT uid must
// evict the old uid from byUID, or GetByUID(oldUID) keeps returning a dead
// instance forever. This became reachable once SwapStore started re-recording
// identity — a swap can change which uid's data a name's slot logically
// represents in tests that re-Set after swapping.
func TestManager_Set_EvictsStaleUID(t *testing.T) {
	m := New(context.Background(), Deps{})

	first := &RepoInstance{uid: "uid-1"}
	first.setName("core")
	m.Set("core", first)
	require.Same(t, first, m.GetByUID("uid-1"))

	second := &RepoInstance{uid: "uid-2"}
	second.setName("core")
	m.Set("core", second)

	require.Nil(t, m.GetByUID("uid-1"), "stale uid must be evicted from byUID")
	require.Same(t, second, m.GetByUID("uid-2"))
	require.Same(t, second, m.Get("core"))
}

// TestStart_reopensExistingReposOnly pins the other half: Start opens every
// registered repo and still creates none of its own.
func TestStart_reopensExistingReposOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	boot := func() *Manager {
		m := New(context.Background(), Deps{
			Cfg:         config.Config{Home: dir},
			AgentBranch: "machine/test",
			Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
		})
		require.NoError(t, m.Start())
		return m
	}

	m1 := boot()
	createRepo(t, m1, "alpha")
	createRepo(t, m1, "beta")
	require.NoError(t, m1.Close())

	m2 := boot()
	t.Cleanup(func() { _ = m2.Close() })
	require.ElementsMatch(t, []string{"alpha", "beta"}, m2.Names(),
		"reboot must re-open exactly the repos on disk — no more, no fewer")
}
