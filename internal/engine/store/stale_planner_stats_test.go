package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/internal/sqlite"
	"knomit/internal/engine/store/vocab"
)

// plannerStatsBatch is n fact files, distinct enough to embed differently.
func plannerStatsBatch(n int) map[string]string {
	files := make(map[string]string, n)
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("kb/b/%04d.md", i)] = fmt.Sprintf(
			"---\ntype: observation\n---\n# Batch fact %d\n\nbody %s", i, strings.Repeat("x", i%97))
	}
	return files
}

// plannerPlanProblems EXPLAINs the five vec0 KNN reads and the two hot graph
// write statements on every held connection and returns one line per plan that
// falls back to a scan: an inner loop of a vec0 read that is not an index
// SEARCH or vec0 point read, `SCAN edges` in graphMergeEdge, or
// graphNodeIDByProps driven by the label alone. It returns problems rather
// than failing so a hook running inside Sync can report them to the test.
func plannerPlanProblems(ctx context.Context, conns []*sql.Conn) ([]string, error) {
	var problems []string
	for ci, c := range conns {
		var branchID int64
		if err := c.QueryRowContext(ctx, `SELECT id FROM branches WHERE name = 'agent/test'`).Scan(&branchID); err != nil {
			return nil, err
		}
		v := make([]float32, 768)
		v[0] = 1
		vec := sqlite.Float32SliceToBytes(v)
		a := index.KNNArgsForTest{Vec: vec, K: 11, BranchID: branchID, Path: "kb/b/0001.md"}
		type read struct {
			name, sql string
			args      []any
		}
		var reads []read
		for _, q := range index.VecKNNQueriesForTest() {
			reads = append(reads, read{q.Name, q.SQL, q.Args(a)})
		}
		reads = append(reads, read{"title", vocab.TitleKNNSQLForTest(), []any{vec, 24, "agent/test", int64(1)}})
		for _, r := range reads {
			loops, err := plannerLoops(ctx, c, r.sql, r.args)
			if err != nil {
				return nil, err
			}
			for _, l := range loops[1:] {
				if !strings.HasPrefix(l, "SEARCH ") && !vec0PointLookup.MatchString(l) {
					problems = append(problems, fmt.Sprintf("conn %d: %s: inner loop %q (plan: %s)",
						ci, r.name, l, strings.Join(loops, " | ")))
					break
				}
			}
		}
		for _, g := range index.GraphWriteStatementsForTest() {
			loops, err := plannerLoops(ctx, c, g.SQL, g.Args)
			if err != nil {
				return nil, err
			}
			plan := strings.Join(loops, " | ")
			switch g.Name {
			case "graphMergeEdge":
				for _, l := range loops {
					if strings.HasPrefix(l, "SCAN ") && !strings.HasPrefix(l, "SCAN CONSTANT ROW") {
						problems = append(problems, fmt.Sprintf("conn %d: %s: %q (plan: %s)", ci, g.Name, l, plan))
						break
					}
				}
			case "graphNodeIDByProps":
				// Fresh statistics start from the property lookup. With node_labels
				// statistics describing a much smaller table (alone: other tables'
				// statistics fresh) the lookup walks node_labels instead: either
				// a full `SCAN nl` or every node of the label through
				// idx_node_labels_label (label=?). That state is planted by
				// TestGraphNodeIDByProps_NodeLabelsOnlyStale; the natural
				// one-fact state does not produce it (there the stale graph cost
				// is graphMergeEdge's SCAN edges).
				if strings.HasPrefix(loops[0], "SCAN ") || strings.Contains(plan, "idx_node_labels_label (label=?)") {
					problems = append(problems, fmt.Sprintf("conn %d: %s walks node_labels (plan: %s)", ci, g.Name, plan))
				}
			}
		}
	}
	return problems, nil
}

// plannerLoops is the SCAN/SEARCH rows of q's plan, outermost first.
func plannerLoops(ctx context.Context, c *sql.Conn, q string, args []any) ([]string, error) {
	rows, err := c.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var loops []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			return nil, err
		}
		if strings.HasPrefix(detail, "SCAN ") || strings.HasPrefix(detail, "SEARCH ") {
			loops = append(loops, detail)
		}
	}
	if len(loops) == 0 {
		return nil, fmt.Errorf("no loops in the plan of %q", q)
	}
	return loops, rows.Err()
}

// plannerProblemsOnPool holds the whole pool, collects the problems and
// releases it again.
func plannerProblemsOnPool(t *testing.T, db *sql.DB) []string {
	t.Helper()
	conns := holdPool(t, db)
	defer releasePool(conns)
	problems, err := plannerPlanProblems(context.Background(), conns)
	require.NoError(t, err)
	return problems
}

// A store that holds one fact has the one-row planner statistics the first
// Sync wrote, and SQLite broadcast them to every pooled connection (creating
// sqlite_stat1 is a schema change). A batch landing on it must not plan its
// writes, or the reads that follow, with those statistics: every pooled
// connection, not only the one that refreshed them, plans the five vec0 KNN
// reads and the hot graph writes (graphMergeEdge, graphNodeIDByProps) with
// index SEARCHes (#731).
//
// Mid-pass: Sync refreshes the statistics when the number of facts it has
// indexed doubles, so the pass does not plan its own tail with one-row
// statistics. After the pass: the refresh reaches every pooled connection, not
// just the one that ran it (ANALYZE reloads only its own connection).
//
// Sabotage (each run and seen to fail): drop the statsCheckpoint call from
// Index.Sync's loops, and the mid-pass assertion fails; drop the schema bump
// from refreshPlannerStats, and the assertions fail on the connections that
// did not run the optimize, mid-pass and after the batch.
func TestVecKNN_StalePlannerStatsRefreshed(t *testing.T) {
	ctx := context.Background()
	svc := openAbstractionTestService(t)
	db := svc.si.RepoForTest().DB()
	writeTestFact(t, svc, "kb/a/one.md", "One", "first body")

	// The premise: the natural state after one write is the stale one.
	require.NotEmpty(t, plannerProblemsOnPool(t, db), "premise: SQLite must plan a store holding one fact with SCANs under "+
		"the one-row statistics a first write leaves; if it no longer does, this guard proves nothing and needs rethinking")

	var midPass []string
	checkpoints := map[int]bool{}
	svc.si.SetAfterStatsCheckpointHookForTest(func(n int) {
		checkpoints[n] = true
		// optimize re-analyzes a table only once it is 10x the size its
		// statistics describe, so the one-row statistics of the first
		// checkpoints are replaced at the 16-fact one (a fixture choice: any
		// smaller table is too small for the scans to cost anything).
		if n < 16 {
			return
		}
		// Holding the whole pool would block forever if Sync ever kept a pooled
		// connection across a checkpoint: give up with a failure instead.
		holdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		conns, err := holdPoolContext(holdCtx, db)
		defer releasePool(conns)
		if err != nil {
			midPass = append(midPass, "cannot hold the pool inside the hook: "+err.Error())
			return
		}
		problems, err := plannerPlanProblems(ctx, conns)
		if err != nil {
			midPass = append(midPass, err.Error())
		}
		for _, p := range problems {
			midPass = append(midPass, fmt.Sprintf("after %d facts indexed: %s", n, p))
		}
	})
	t.Cleanup(func() { svc.si.SetAfterStatsCheckpointHookForTest(nil) })

	_, _, err := svc.Facts().BatchWriteFacts(ctx, "agent/test", plannerStatsBatch(40), nil, "batch", "test")
	require.NoError(t, err)

	for _, n := range []int{1, 2, 4, 8, 16, 32} {
		require.True(t, checkpoints[n], "Sync must checkpoint when it has indexed %d facts (saw %v)", n, checkpoints)
	}
	assert.Empty(t, midPass, "mid-pass, every pooled connection must plan with SEARCH")
	assert.Empty(t, plannerProblemsOnPool(t, db), "after the batch, every pooled connection must plan with SEARCH")
}

// Stores whose on-disk sqlite_stat1 is already stale at Open (a crash mid-pass,
// statistics written by another binary) are repaired by Open: it runs
// `PRAGMA optimize=0x10002` after the migrations. A plain `PRAGMA optimize`
// before them, as Open used to run, checks no size drift: it has used no table
// yet on its connection and only analyzes never-analyzed indexes.
//
// Sabotage (run and seen to fail): put Open's optimize back before migrate.All
// as a plain `PRAGMA optimize`.
func TestOpen_RepairsStaleOnDiskStatistics(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "k.db")
	svc := openAbstractionTestServiceAt(t, path, &stub768Embedder{})
	db := svc.si.RepoForTest().DB()
	writeTestFact(t, svc, "kb/a/one.md", "One", "first body")
	_, _, err := svc.Facts().BatchWriteFacts(ctx, "agent/test", plannerStatsBatch(40), nil, "batch", "test")
	require.NoError(t, err)

	// Plant statistics describing a one-fact store, on disk and on every
	// connection, then close the store (every connection released first).
	conns := holdPool(t, db)
	plantOneFactStatistics(t, conns)
	problems, err := plannerPlanProblems(ctx, conns)
	require.NoError(t, err)
	require.NotEmpty(t, problems, "premise: planted one-row statistics must make SQLite plan scans")
	releasePool(conns)
	require.NoError(t, svc.Close())

	svc2 := openAbstractionTestServiceAt(t, path, &stub768Embedder{})
	require.Empty(t, plannerProblemsOnPool(t, svc2.si.RepoForTest().DB()),
		"after Open, every pooled connection must plan with SEARCH")
}

// holdPoolContext is holdPool for code that cannot use require (a hook inside
// Sync): it returns what it got and the error.
func holdPoolContext(ctx context.Context, db *sql.DB) ([]*sql.Conn, error) {
	var conns []*sql.Conn
	for i := 0; i < db.Stats().MaxOpenConnections; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			return conns, err
		}
		conns = append(conns, c)
	}
	return conns, nil
}

// plantNodeLabelsStatistics makes sqlite_stat1 describe node_labels as a
// one-row table (every other table keeps its statistics) and reloads them on
// every held connection.
func plantNodeLabelsStatistics(t *testing.T, conns []*sql.Conn) {
	t.Helper()
	for _, st := range [][2]string{
		{"idx_node_labels_label", "1 1 1"},
		{"sqlite_autoindex_node_labels_1", "1 1 1"},
	} {
		res, err := conns[0].ExecContext(context.Background(),
			`UPDATE sqlite_stat1 SET stat = ? WHERE tbl = 'node_labels' AND idx = ?`, st[1], st[0])
		require.NoError(t, err)
		n, err := res.RowsAffected()
		require.NoError(t, err)
		require.EqualValues(t, 1, n, "sqlite_stat1 must hold a row for node_labels/%s", st[0])
	}
	reloadStatistics(t, conns)
}

// graphNodeIDByProps must not walk node_labels. Statistics that describe
// node_labels as a one-row table while every other table's are fresh (the
// state the #731 RCA found slowest: "mixed" staleness is not safer than all
// stale) make it do so, on every connection; one refresh must repair every
// pooled connection, not only the one that ran it.
//
// Sabotage (run and seen to fail): drop the schema bump from
// RefreshPlannerStats, and only the connection that ran the optimize is
// repaired.
func TestGraphNodeIDByProps_NodeLabelsOnlyStale(t *testing.T) {
	ctx := context.Background()
	svc := openAbstractionTestService(t)
	db := svc.si.RepoForTest().DB()
	writeTestFact(t, svc, "kb/a/one.md", "One", "first body")
	_, _, err := svc.Facts().BatchWriteFacts(ctx, "agent/test", plannerStatsBatch(300), nil, "batch", "test")
	require.NoError(t, err)

	conns := holdPool(t, db)
	plantNodeLabelsStatistics(t, conns)
	problems, err := plannerPlanProblems(ctx, conns)
	require.NoError(t, err)
	walks := 0
	for _, p := range problems {
		if strings.Contains(p, "graphNodeIDByProps walks node_labels") {
			walks++
		}
	}
	require.Equal(t, len(conns), walks,
		"premise: node_labels-only stale statistics must make graphNodeIDByProps walk node_labels on every connection; problems: %v", problems)
	releasePool(conns)

	require.NoError(t, index.RefreshPlannerStats(ctx, db, index.OptimizeStatsSQL))
	require.Empty(t, plannerProblemsOnPool(t, db), "after the refresh, every pooled connection must plan with SEARCH")
}

// Rebuild ends with an ANALYZE after which every pooled connection must plan
// with the fresh statistics. (Rebuild's phases 1-3 themselves get no in-pass
// checkpoints: they plan with the statistics held when Rebuild starts.)
//
// Sabotage run: dropping the closing ANALYZE makes this fail. Reverting it to
// a bare ANALYZE with no schema bump does NOT: Rebuild's own DDL moves the
// schema version (by 14 here) so every connection reloads from disk at its next
// statement anyway, and the bump through RefreshPlannerStats is belt and braces
// there. This test therefore guards the ANALYZE, not the propagation.
func TestRebuild_AnalyzeReachesEveryPooledConnection(t *testing.T) {
	ctx := context.Background()
	svc := openAbstractionTestService(t)
	db := svc.si.RepoForTest().DB()
	writeTestFact(t, svc, "kb/a/one.md", "One", "first body")
	_, _, err := svc.Facts().BatchWriteFacts(ctx, "agent/test", plannerStatsBatch(40), nil, "batch", "test")
	require.NoError(t, err)

	conns := holdPool(t, db)
	plantOneFactStatistics(t, conns)
	problems, err := plannerPlanProblems(ctx, conns)
	require.NoError(t, err)
	require.NotEmpty(t, problems, "premise: planted one-row statistics must make SQLite plan scans")
	releasePool(conns)

	require.NoError(t, svc.si.Rebuild(ctx, "agent/test", nil))
	require.Empty(t, plannerProblemsOnPool(t, db), "after Rebuild, every pooled connection must plan with SEARCH")
}

// RefreshPlannerStats must do nothing when ctx carries a transaction: its
// statements run on other pooled connections, which would wait on the write
// lock the caller's own tx holds (measured: 10.6 s, then "database is
// locked"). The statement here would block that way if it ran.
func TestRefreshPlannerStats_RefusesAContextWithATransaction(t *testing.T) {
	svc := openAbstractionTestService(t)
	db := svc.si.RepoForTest().DB()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	ctx, cancel := context.WithTimeout(storegit.WithTx(context.Background(), tx), 3*time.Second)
	defer cancel()

	start := time.Now()
	err = index.RefreshPlannerStats(ctx, db, `INSERT INTO meta(key, value) VALUES ('zz_planner_stats_probe', '1')`)
	require.NoError(t, err)
	require.Less(t, time.Since(start), time.Second, "it must return at once, not wait on the tx's write lock")
	require.NoError(t, tx.Rollback())

	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM meta WHERE key = 'zz_planner_stats_probe'`).Scan(&n))
	require.Zero(t, n, "the statement must not have run")
}

// BenchmarkBatchWriteOntoOneFactStore times #725's shape: 1 existing fact and
// 2000 more in one BatchWriteFacts. Run with -benchtime=1x. Wall time is
// platform-specific; it is a logged measurement, not an assertion.
func BenchmarkBatchWriteOntoOneFactStore(b *testing.B) {
	ctx := context.Background()
	files := plannerStatsBatch(2000)
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		svc, cleanup := openBenchService(b)
		_, err := svc.Facts().WriteFact(ctx, "agent/test", "kb/a/one.md",
			"---\ntype: observation\n---\n# One\n\nfirst body", "write", "test")
		require.NoError(b, err)
		b.StartTimer()
		_, _, err = svc.Facts().BatchWriteFacts(ctx, "agent/test", files, nil, "batch", "test")
		b.StopTimer()
		require.NoError(b, err)
		cleanup()
	}
}

func openBenchService(b *testing.B) (*Service, func()) {
	b.Helper()
	svc, err := Open(filepath.Join(b.TempDir(), "k.db"))
	require.NoError(b, err)
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(b, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	return svc, func() { _ = svc.Close() }
}
