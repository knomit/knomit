package repos_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/app"
	"knomit/internal/config"
	"knomit/internal/repos"
)

// startManager boots a Manager rooted at a t.TempDir() with the default
// "knomit" repo created. Returns the manager and the home directory so
// tests can drop additional .db files into <home>/repos/.
func startManager(t *testing.T) (*repos.Manager, string) {
	t.Helper()
	home := t.TempDir()
	m := repos.New(t.Context(), repos.Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	return m, home
}

// initRepoFile uses the production app.InitRepo path to create a new
// repo .db file under <home>/repos/<name>.db. The manager isn't told
// about it — that's what Rescan should do.
func initRepoFile(t *testing.T, home, name string) {
	t.Helper()
	cfg := config.Config{Home: home}
	require.NoError(t, app.InitRepo(cfg, name, "", ""))
}

func TestManager_Rescan_AddsNewRepo(t *testing.T) {
	m, home := startManager(t)
	require.Nil(t, m.Get("work"), "precondition: work not yet registered")
	initRepoFile(t, home, "work")

	result, err := m.Rescan()
	require.NoError(t, err)

	require.Equal(t, []string{"work"}, result.Added)
	require.Equal(t, []string{"knomit"}, result.Skipped)
	require.Empty(t, result.Errors)
	require.NotNil(t, m.Get("work"), "work must be registered after Rescan")
}

func TestManager_Rescan_SkipsAlreadyOpen(t *testing.T) {
	m, home := startManager(t)
	initRepoFile(t, home, "work")
	_, err := m.Rescan()
	require.NoError(t, err)

	// Second call: nothing new on disk, both repos must show up in Skipped.
	result, err := m.Rescan()
	require.NoError(t, err)

	require.Empty(t, result.Added)
	require.ElementsMatch(t, []string{"knomit", "work"}, result.Skipped)
	require.Empty(t, result.Errors)
}

func TestManager_Rescan_IgnoresInvalidNames(t *testing.T) {
	m, home := startManager(t)

	// Drop a file with an uppercase name — fails isValidRepoName.
	bogus := filepath.Join(home, "repos", "Foo.db")
	require.NoError(t, writeEmptyFile(bogus))

	result, err := m.Rescan()
	require.NoError(t, err)

	require.NotContains(t, result.Added, "Foo")
	require.NotContains(t, result.Skipped, "Foo")
	for _, e := range result.Errors {
		require.NotEqual(t, "Foo", e.Repo)
	}
	require.Nil(t, m.Get("Foo"))
}

func TestManager_Rescan_EmptyDirReturnsKnomitOnly(t *testing.T) {
	m, _ := startManager(t)

	result, err := m.Rescan()
	require.NoError(t, err)

	require.Empty(t, result.Added)
	require.Equal(t, []string{"knomit"}, result.Skipped)
	require.Empty(t, result.Errors)
}

func TestManager_Rescan_ConcurrentSafe(t *testing.T) {
	m, home := startManager(t)
	initRepoFile(t, home, "work")

	type rescanReport struct {
		result repos.RescanResult
		err    error
	}

	const n = 8
	var wg sync.WaitGroup
	reports := make(chan rescanReport, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.Rescan()
			reports <- rescanReport{r, err}
		}()
	}
	wg.Wait()
	close(reports)

	totalAdded := 0
	for r := range reports {
		require.NoError(t, r.err)
		for _, name := range r.result.Added {
			if name == "work" {
				totalAdded++
			}
		}
	}
	// The spec requires rescanMu to serialize entire Rescan calls (not just
	// the Add step), so exactly one call must observe "work" in Added and
	// all others must report it in Skipped. A different sync strategy that
	// allowed parallel calls could legitimately yield totalAdded == 0 here;
	// this assertion intentionally locks in the spec's serialization design.
	require.Equal(t, 1, totalAdded, "exactly one Rescan must report 'work' in Added")
	require.NotNil(t, m.Get("work"))
}

func writeEmptyFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return f.Close()
}
