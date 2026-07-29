package repos

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// TestStart_skipsSessionSidecarDB is a regression test for the boot-time
// warning "skipping db with invalid repo name file=<repo>.sessions.db".
//
// Each repo's ephemeral session database is written as a sibling
// "<repo>.sessions.db" in the same repos/ directory as the repo DBs. The
// filesystem adoption scan used to treat that sidecar as a repo named
// "<repo>.sessions", which fails isValidRepoName (repo names can't contain a
// '.'), producing a spurious warning and a junk registry row. Adoption must
// skip session sidecars silently.
//
// The scan only runs when the registry is empty — the one-time migration from
// filesystem-as-registry — so the fixture is exactly that: repo databases on
// disk (one with a leftover sidecar next to it) and no control.db yet.
func TestStart_skipsSessionSidecarDB(t *testing.T) {
	home := t.TempDir()

	// Build the pre-registry on-disk state through the production path, then
	// discard control.db so the next boot must adopt from the filesystem.
	seed := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	require.NoError(t, seed.Start())
	mustCreateRepo(t, seed, testRepoName)
	require.NoError(t, seed.Close())
	require.NoError(t, os.Remove(filepath.Join(home, "control.db")))

	// The live sidecar is removed on Close, so re-create one: a crashed process
	// leaves it behind, and that is the case the scan has to tolerate.
	sidecar := filepath.Join(home, "repos", testRepoName+store.SessionDBSuffix)
	require.NoError(t, os.WriteFile(sidecar, []byte{}, 0o644))

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&buf))
	t.Cleanup(func() { log.Logger = orig })

	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())

	require.NotNil(t, m.Get(testRepoName), "the real repo must be adopted")
	require.Nil(t, m.Get(testRepoName+".sessions"),
		"session sidecar must not be registered as a repo")

	// Close before reading the buffer so background loggers (reaper, cluster
	// checker) can't write concurrently with the read.
	require.NoError(t, m.Close())

	out := buf.String()
	require.NotContains(t, out, "invalid repo name",
		"session sidecar DB must be skipped silently, got log: %s", out)
	require.NotContains(t, out, testRepoName+".sessions")
}
