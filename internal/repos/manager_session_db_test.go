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
// The realistic way Start's glob meets a sidecar is a leftover one: the file is
// removed on a clean Close, so it is still sitting in repos/ at the next boot
// exactly when the previous process did not shut down cleanly.
func TestStart_skipsSessionSidecarDB(t *testing.T) {
	home := t.TempDir()
	newManager := func() *Manager {
		return New(context.Background(), Deps{
			Cfg:                   config.Config{Home: home},
			AgentBranch:           "machine/test",
			DisableBackgroundSync: true,
		})
	}

	// First boot: create a repo and confirm opening it really does produce a
	// sidecar under the scanned directory, so the name below is the product's
	// and not one this test invented.
	m := newManager()
	bootRepo(t, m)
	sidecar := filepath.Join(home, "repos", testRepoName+store.SessionDBSuffix)
	_, statErr := os.Stat(sidecar)
	require.NoError(t, statErr, "session sidecar DB should exist while the repo is open")
	require.NoError(t, m.Close())

	// Leave one behind, as an unclean shutdown would, and boot over it.
	require.NoError(t, os.WriteFile(sidecar, nil, 0o644))

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&buf))
	t.Cleanup(func() { log.Logger = orig })

	m2 := newManager()
	require.NoError(t, m2.Start())
	// Start only opens what the registry says exists, and Create does not
	// register a row until Task 6 — so the reboot must re-open the repo by hand.
	reopenTestRepo(t, m2, home)

	// The session sidecar must never be registered as a repo.
	require.Nil(t, m2.Get(testRepoName+".sessions"),
		"session sidecar must not be registered as a repo")
	require.Equal(t, []string{testRepoName}, m2.Names(),
		"only the real repo may be registered")

	// Close before reading the buffer so background loggers (reaper, cluster
	// checker) can't write concurrently with the read.
	require.NoError(t, m2.Close())

	out := buf.String()
	require.NotContains(t, out, "invalid repo name",
		"session sidecar DB must be skipped silently, got log: %s", out)
	require.NotContains(t, out, testRepoName+".sessions")
}
