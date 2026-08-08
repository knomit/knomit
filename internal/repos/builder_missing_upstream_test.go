package repos

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestOpen_StoredUpstreamWithNoLocalRef regresses PR #73 review finding #1: a
// repo whose stored upstream names a branch that does not exist locally must not
// re-index itself on every boot, forever, while reporting a healthy index.
//
// The shape is reachable in production: a repo created locally (its agent branch
// comes off "main"), later pointed at a master-convention origin. Rebuild fails
// at HeadCommit for "master" permanently — nothing about that heals on retry.
//
// The test forces the heal onto its REBUILD path (by clearing the persisted
// index schema version, exactly as a GraphSchemaVersion bump or an
// embedding-identity change does) because that is the only path where the old
// code re-armed anything. Then it boots twice more:
//
//   - Boot 2 must rebuild the agent branch and report ready, with the
//     unresolvable upstream logged and skipped rather than failing the repo.
//   - Boot 3 must find the agent branch already current. Under the old GLOBAL
//     schema version this is what broke: the upstream's permanent failure
//     cleared the key the agent branch's rebuild had just written, so boot 3 —
//     and every boot after it — re-indexed the whole repo from scratch while the
//     UI reported a healthy index.
func TestOpen_StoredUpstreamWithNoLocalRef(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cfg := config.Config{Home: home}
	ctx := context.Background()

	boot := func() (*Manager, func()) {
		m := New(context.Background(), Deps{
			Cfg:                   cfg,
			AgentBranch:           "agent/test-abc",
			DisableBackgroundSync: true,
		})
		require.NoError(t, m.Start())
		return m, func() { _ = m.Close() }
	}

	// First boot: a plain local repo, then point it at an origin whose default
	// branch this repo does not have.
	m, done := boot()
	ri := bootRepo(t, m)
	agentBranch := ri.AgentBranch()
	require.NoError(t, testService(t, ri).Remote().SetRemote(
		"origin", "file://"+filepath.Join(dir, "nowhere.git"), "master", agentBranch, 300, 300, "", ""))
	done()

	dbPath := filepath.Join(home, "repos", testRepoName+".db")

	// Simulate the upgrade that puts the heal on its rebuild path: every branch's
	// persisted schema version is behind.
	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	// The GLOB has no colon so it clears the version however it is keyed — the
	// point of this step is "the persisted schema version is behind", not which
	// row holds it.
	_, err = raw.Exec(`DELETE FROM meta WHERE key GLOB 'graph_schema_version*'`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// Second boot: the builder rehydrates upstreamMain="master", finds it
	// unresolvable, and heals what it can.
	m2, done2 := boot()
	ri2 := m2.Get(testRepoName)
	require.NotNil(t, ri2)

	state, _, _ := ri2.IndexStatus()
	require.Equal(t, "ready", state, "an unreachable upstream must not fail the repo's index")

	stale, err := testService(t, ri2).IndexManager().NeedsRebuild(ctx, agentBranch)
	require.NoError(t, err)
	require.False(t, stale, "the agent branch rebuilt on this boot and must read current")
	done2()

	// Third boot: the agent branch must STILL be current — the previous boot's
	// unusable upstream did not re-arm a repo-wide rebuild.
	m3, done3 := boot()
	defer done3()
	ri3 := m3.Get(testRepoName)
	require.NotNil(t, ri3)

	stale, err = testService(t, ri3).IndexManager().NeedsRebuild(ctx, agentBranch)
	require.NoError(t, err)
	require.False(t, stale,
		"a permanently unusable upstream must not force the agent branch to rebuild every boot")

	state, _, _ = ri3.IndexStatus()
	require.Equal(t, "ready", state)
}
