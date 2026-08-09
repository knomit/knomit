package repos

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Start opens what the registry says exists, keyed by uid — not what the
// directory happens to contain.
func TestStart_OpensRegisteredRepos(t *testing.T) {
	t.Skip("registry rows are written in Task 6")
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	require.NoError(t, m.Close())

	m2 := newTestManager(t)
	m2.deps.Cfg.Home = m.deps.Cfg.Home
	require.NoError(t, m2.Start())
	got := m2.Get("core")
	require.NotNil(t, got)
	require.Equal(t, uid, got.UID())
}

// A registered repo whose file is gone stays VISIBLE as unavailable rather
// than vanishing from the API — the whole point of a registry that outlives
// the file.
func TestStart_MissingFileReportsUnavailable(t *testing.T) {
	t.Skip("registry rows are written in Task 6")
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	home := m.deps.Cfg.Home
	require.NoError(t, m.Close())

	require.NoError(t, os.Remove(m.RepoPath(uid)))

	m2 := newTestManager(t)
	m2.deps.Cfg.Home = home
	require.NoError(t, m2.Start())

	require.Nil(t, m2.Get("core"), "no live instance for a missing file")
	un := m2.Unavailable()
	require.Len(t, un, 1)
	require.Equal(t, "core", un[0].Record.Name)
	require.Equal(t, "missing", un[0].Reason)
}

// A .db with no registry row is inert. Dropping a file into repos/ is no
// longer a way to register anything.
func TestStart_OrphanFileIsIgnored(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	require.NoError(t, os.WriteFile(m.RepoPath("orphan"), []byte("not a db"), 0o644))
	require.NoError(t, m.Close())

	m2 := newTestManager(t)
	m2.deps.Cfg.Home = m.deps.Cfg.Home
	require.NoError(t, m2.Start())
	require.Empty(t, m2.Names())
	require.Empty(t, m2.Unavailable())
}
