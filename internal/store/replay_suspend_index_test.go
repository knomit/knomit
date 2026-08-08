package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// countIndexedFacts returns the number of branch_facts rows for branch — i.e.
// how many facts the search index currently has materialized for that branch
// (independent of what the git tree contains).
func countIndexedFacts(t *testing.T, svc *Service, branch string) int {
	t.Helper()
	ctx := context.Background()
	si := svc.si
	bid, err := si.rh.branchID(ctx, branch)
	require.NoError(t, err)
	var n int
	require.NoError(t, si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM branch_facts WHERE branch_id = ?`, bid).Scan(&n))
	return n
}

func mustListAll(t *testing.T, svc *Service, branch string) []string {
	t.Helper()
	paths, err := svc.rh.ListAll(context.Background(), branch)
	require.NoError(t, err)
	return paths
}

// TestReplay_SkipIndexSync verifies the origin-apply optimization: when
// ReplayConfig.SkipIndexSync is set, Replay writes facts into the (throwaway)
// target store's git tree but does NOT maintain its search index per-commit —
// the caller is expected to run a single Rebuild afterward (which the commit
// handler does). With the flag unset, the index is maintained as today.
func TestReplay_SkipIndexSync(t *testing.T) {
	ctx := context.Background()
	const agent = "agent/laptop"

	setupLocal := func(t *testing.T) *Service {
		dir := t.TempDir()
		svc, err := Open(filepath.Join(dir, "local.db"))
		require.NoError(t, err)
		t.Cleanup(func() { svc.Close() })
		require.NoError(t, svc.InitRepo(map[string]string{}, agent))
		_, err = svc.Facts().WriteFact(ctx, agent, "kb/a.md", testFactBody("A", 0.9, nil), "a", "")
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, agent, "kb/b.md", testFactBody("B", 0.8, nil), "b", "")
		require.NoError(t, err)
		return svc
	}

	// A freshly "cloned" target: a main branch and an empty index, mirroring
	// what CloneFrom produces (git refs only, branches table unpopulated).
	setupClone := func(t *testing.T) *Service {
		dir := t.TempDir()
		svc, err := Open(filepath.Join(dir, "clone.db"))
		require.NoError(t, err)
		t.Cleanup(func() { svc.Close() })
		require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
		return svc
	}

	// wantIndexed is the number of indexable facts the replay materializes:
	// the two real facts (kb/a.md, kb/b.md). The InitRepo seed (README.md) is a
	// non-indexable placeholder — it sits in the git tree but never produces a
	// branch_facts row — so it does not count here.
	const wantIndexed = 2

	run := func(t *testing.T, skip bool) *Service {
		local := setupLocal(t)
		clone := setupClone(t)
		iter, err := local.Search().FactsIter(ctx, agent)
		require.NoError(t, err)
		_, err = Replay(ctx, local, agent, iter, clone, ReplayConfig{
			Strategy:      StrategyLocalWins,
			AgentBranch:   agent,
			DefaultBranch: "main",
			SkipIndexSync: skip,
		})
		require.NoError(t, err)

		// The git tree must always contain the replayed facts, suspend or not.
		paths := mustListAll(t, clone, agent)
		require.Contains(t, paths, "kb/a.md", "git tree must contain replayed facts regardless of SkipIndexSync")
		require.Contains(t, paths, "kb/b.md")
		return clone
	}

	t.Run("suspended: index empty until explicit rebuild", func(t *testing.T) {
		clone := run(t, true)
		require.Zero(t, countIndexedFacts(t, clone, agent),
			"SkipIndexSync must not maintain the search index during Replay")

		// The single Rebuild the commit handler runs reconstructs the index from git.
		require.NoError(t, clone.IndexManager().Rebuild(ctx, agent, nil))
		require.Equal(t, wantIndexed, countIndexedFacts(t, clone, agent),
			"Rebuild after a suspended Replay must fully index the replayed facts")
	})

	t.Run("default: index maintained during replay", func(t *testing.T) {
		clone := run(t, false)
		require.Equal(t, wantIndexed, countIndexedFacts(t, clone, agent),
			"without SkipIndexSync, Replay must index facts as it writes (unchanged behavior)")
	})

	// The suspend is per-Service-instance: a separate store written via the
	// normal WriteFact path indexes normally even while a clone is suspended.
	t.Run("suspend does not leak to other stores", func(t *testing.T) {
		_ = run(t, true) // a suspended clone exists in this test process

		other := setupLocal(t) // writes via the regular WriteFact path
		require.Equal(t, wantIndexed, countIndexedFacts(t, other, agent),
			"regular WriteFact on an unrelated store must index normally")
	})
}

// TestSchemaVersionState verifies the three-way classification that lets Sync
// warn only on a genuine version mismatch — not on a fresh/empty DB whose
// graph_schema_version row simply hasn't been written yet.
func TestSchemaVersionState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	si := svc.si

	setVersion := func(v string) {
		_, err := si.rh.db.ExecContext(ctx,
			`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, schemaVersionKey("main"), v)
		require.NoError(t, err)
	}
	clearVersion := func() {
		_, err := si.rh.db.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, schemaVersionKey("main"))
		require.NoError(t, err)
	}

	clearVersion()
	st, err := si.schemaVersionState(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, schemaMissing, st, "no row → missing (fresh DB), not stale")
	nr, err := si.NeedsRebuild(ctx, "main")
	require.NoError(t, err)
	require.True(t, nr, "missing version must still require a rebuild (contract preserved)")

	setVersion(GraphSchemaVersion)
	st, err = si.schemaVersionState(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, schemaCurrent, st)
	nr, err = si.NeedsRebuild(ctx, "main")
	require.NoError(t, err)
	require.False(t, nr, "current version with no embedder must be clean")

	setVersion("ancient")
	st, err = si.schemaVersionState(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, schemaStale, st, "present-but-different → stale (the real warning case)")
	nr, err = si.NeedsRebuild(ctx, "main")
	require.NoError(t, err)
	require.True(t, nr, "stale version must require a rebuild")

	// The key is per-branch, so a current "main" says nothing about a branch
	// that has never been indexed.
	setVersion(GraphSchemaVersion)
	st, err = si.schemaVersionState(ctx, "agent/other")
	require.NoError(t, err)
	require.Equal(t, schemaMissing, st, "another branch's version must not answer for this one")
}
