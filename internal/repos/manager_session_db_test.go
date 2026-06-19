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
// startup *.db glob used to treat that sidecar as a repo named
// "<repo>.sessions", which fails isValidRepoName (repo names can't contain a
// '.'), producing a spurious warning on every boot. Start must skip session
// sidecars silently.
func TestStart_skipsSessionSidecarDB(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&buf))
	t.Cleanup(func() { log.Logger = orig })

	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())

	// Precondition: opening the default repo really did create the session
	// sidecar in the scanned directory, so the glob actually encountered it.
	// (It is ephemeral and removed on Close, so stat it while the manager is
	// still open.)
	sidecar := filepath.Join(home, "repos", config.DefaultRepoName+store.SessionDBSuffix)
	_, statErr := os.Stat(sidecar)
	require.NoError(t, statErr, "session sidecar DB should exist after boot")

	// The session sidecar must never be registered as a repo.
	require.Nil(t, m.Get(config.DefaultRepoName+".sessions"),
		"session sidecar must not be registered as a repo")

	// Close before reading the buffer so background loggers (reaper, cluster
	// checker) can't write concurrently with the read.
	require.NoError(t, m.Close())

	out := buf.String()
	require.NotContains(t, out, "invalid repo name",
		"session sidecar DB must be skipped silently, got log: %s", out)
	require.NotContains(t, out, config.DefaultRepoName+".sessions")
}
