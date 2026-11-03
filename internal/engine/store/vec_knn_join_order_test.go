package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/internal/sqlite"
	"knomit/internal/engine/store/vocab"
)

// vecKNNCase is one vec0 KNN read under test: its exact production SQL, the
// alias of its vec0 table, the arguments to run it with, and its k window.
// filtered marks a read whose WHERE drops rows from the window (the fixture
// must make it drop some); ordered marks a read with an ORDER BY (its rows are
// compared in that order).
type vecKNNCase struct {
	name, sql, alias  string
	args              []any
	k                 int
	filtered, ordered bool
}

// Every vec0 KNN read that joins its vec0 table to another table keeps the
// vec0 table as the OUTER loop, and the fixed order changes the plan only: the
// rows equal those of the same SQL with each CROSS JOIN written as a plain
// JOIN, in ORDER BY order where the read has one. It holds under three
// planner-statistics states: none, a fresh ANALYZE, and STALE statistics that
// still describe a one-fact store (what PRAGMA optimize leaves before a large
// batch lands; the rows below are the ones dumped from the #725 store before
// its 501-fact commit).
//
// With no statistics and after ANALYZE every inner loop, the QueryByPath
// subquery's included, is also an index SEARCH, so a read costs about k
// lookups. Under the stale statistics the inner loops degrade to full SCANs
// (`SCAN f USING COVERING INDEX …`, `SCAN bf`, the subquery's `SCAN bf2`):
// vec0 still runs once, but each of its k rows scans facts or branch_facts.
// That is a known residual, tracked in #731; in that state only the outer loop
// is asserted.
//
// Written as a plain JOIN, SQLite planned `SCAN f USING COVERING INDEX …`
// outer and re-ran the whole KNN once per facts row (#725): every indexed fact
// write cost (facts rows) × one KNN, and a 501-fact batch took 9 minutes on
// Windows. Which plan a plain JOIN gets depends on the statistics, so the
// search reads flipped between 10 ms and 1.2 s run to run.
//
// Sabotage: write any one of the queries' CROSS JOINs as a plain JOIN (its
// case fails: the outer loop is a real table, not the vec0 table).
func TestVecKNN_JoinOrderKeepsVec0Outer(t *testing.T) {
	for _, stats := range []string{"no statistics", "ANALYZE", "stale one-fact statistics"} {
		t.Run(stats, func(t *testing.T) {
			db, cases := vecKNNFixture(t)
			if stats != "no statistics" {
				_, err := db.Exec(`ANALYZE`)
				require.NoError(t, err)
			}
			stale := stats == "stale one-fact statistics"
			if stale {
				plantOneFactStatistics(t, db)
			}
			for _, c := range cases {
				loops, sub := vecKNNPlan(t, db, c.sql, c.args)
				plan := strings.Join(append(append([]string{}, loops...), sub...), "\n")
				require.NotEmpty(t, loops, "%s: no loops in the plan", c.name)
				require.True(t, strings.HasPrefix(loops[0], "SCAN "+c.alias+" VIRTUAL TABLE"),
					"%s: the outer loop must be the vec0 table %q, got plan:\n%s", c.name, c.alias, plan)
				if !stale { // stale: the inner SCANs are #731
					requireEverySearch(t, c.name, plan, append(loops[1:], sub...))
				}

				plain := strings.ReplaceAll(c.sql, "CROSS JOIN", "JOIN")
				plainLoops, plainSub := vecKNNPlan(t, db, plain, c.args)
				t.Logf("%s\n  pinned: %s\n  plain:  %s", c.name, strings.Join(append(loops, sub...), " | "),
					strings.Join(append(plainLoops, plainSub...), " | "))
				got := vecKNNRows(t, db, c.sql, c.args, c.ordered)
				require.NotEmpty(t, got, "%s: the fixture must give the read rows to compare", c.name)
				if c.filtered {
					require.Less(t, len(got), c.k,
						"%s: the k window must straddle rows the filter drops, or the order is not tested against it", c.name)
				}
				require.Equal(t, vecKNNRows(t, db, plain, c.args, c.ordered), got,
					"%s: pinning the join order must not change the rows", c.name)
			}
		})
	}
}

// vec0PointLookup matches a vec0 loop planned as a rowid point read
// (sqlite-vec's idxStr: plan '2' = VEC0_QUERY_PLAN_POINT, '!' = rowid equals),
// which SQLite prints as SCAN although it reads one row.
var vec0PointLookup = regexp.MustCompile(`^SCAN \w+ VIRTUAL TABLE INDEX \d+:2!`)

// requireEverySearch fails unless every loop is an index SEARCH or a vec0
// rowid point read.
func requireEverySearch(t *testing.T, name, plan string, loops []string) {
	t.Helper()
	for _, l := range loops {
		require.True(t, strings.HasPrefix(l, "SEARCH ") || vec0PointLookup.MatchString(l),
			"%s: every inner loop must be an index SEARCH, got %q in plan:\n%s", name, l, plan)
	}
}

// vecKNNFixture is a store whose facts and both vec tables hold live and
// superseded rows with distinct vectors, and every vec0 KNN read of the store
// with arguments whose k windows straddle both.
func vecKNNFixture(t *testing.T) (*sql.DB, []vecKNNCase) {
	t.Helper()
	ctx := context.Background()
	svc := openAbstractionTestService(t)
	db := svc.si.RepoForTest().DB()
	const branch = "agent/test"

	// 30 observations and 3 methodology facts, then 10 observations rewritten,
	// so facts and both vec tables also hold superseded rows the branch filter
	// must drop.
	for i := 0; i < 30; i++ {
		writeTestFact(t, svc, fmt.Sprintf("kb/f/%02d.md", i), fmt.Sprintf("Fact %02d", i), "body")
	}
	for i := 0; i < 3; i++ {
		_, err := svc.Facts().WriteFact(ctx, branch, fmt.Sprintf("kb/m/%d.md", i),
			fmt.Sprintf("---\ntype: methodology\n---\n# Method %d\n\nbody", i), "add", "test")
		require.NoError(t, err)
	}
	for i := 0; i < 10; i++ {
		writeTestFact(t, svc, fmt.Sprintf("kb/f/%02d.md", i), fmt.Sprintf("Fact %02d v2", i), "body two")
	}

	// Row i of facts (by id) sits at position i: rows 0-9 are the superseded
	// first versions of kb/f/00-09, rows 10-29 live kb/f/10-29.
	var ids []int64
	rows, err := db.QueryContext(ctx, `SELECT id FROM facts ORDER BY id`)
	require.NoError(t, err)
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Greater(t, len(ids), 33, "the rewrites must leave superseded facts rows")
	for i, id := range ids {
		blob := sqlite.Float32SliceToBytes(vecKNNVector(float64(i), len(ids)))
		for _, table := range []string{"facts_vec", "fact_titles_vec"} {
			_, err := db.ExecContext(ctx, `DELETE FROM `+table+` WHERE rowid = ?`, id)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `INSERT INTO `+table+`(rowid, embedding) VALUES (?, ?)`, id, blob)
			require.NoError(t, err)
		}
	}

	var branchID, liveID int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT id FROM branches WHERE name = ?`, branch).Scan(&branchID))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT fact_id FROM branch_facts WHERE branch_id = ? AND path = 'kb/f/20.md'`, branchID).Scan(&liveID))
	// Position 12.5's neighbourhood, and that of kb/f/11.md (QueryByPath's
	// source, at position 11), hold superseded rows 4-9 and live rows: each
	// filtered window comes back short.
	probe := sqlite.Float32SliceToBytes(vecKNNVector(12.5, len(ids)))

	const k = 15
	a := index.KNNArgsForTest{Vec: probe, K: k, BranchID: branchID, Path: "kb/f/11.md", MinVec: 0}
	var cases []vecKNNCase
	for _, q := range index.VecKNNQueriesForTest() {
		args := a
		c := vecKNNCase{name: q.Name, sql: q.SQL, alias: q.VecAlias, k: k, filtered: true, ordered: true}
		switch {
		case strings.HasPrefix(q.Name, "similarity"):
			c.filtered = false // no WHERE beyond the KNN
		case q.Name == "methodology":
			args.K = len(ids) // the whole table, as RelevantMethodologyForFact asks
			c.k, c.ordered = args.K, false
		}
		c.args = q.Args(args)
		cases = append(cases, c)
	}
	cases = append(cases, vecKNNCase{name: "title (TopTitleNeighbours)", sql: vocab.TitleKNNSQLForTest(), alias: "tv",
		args: []any{probe, 8, branch, liveID}, k: 8, filtered: true, ordered: true})
	return db, cases
}

// vecKNNVector places position x on a quarter circle with spacing that grows
// with x, so no two rows are equidistant from any probe: a read's ORDER BY
// distance then has one answer, which two plans must both give.
func vecKNNVector(x float64, n int) []float32 {
	v := make([]float32, 768)
	angle := (x + 0.02*x*x) * (math.Pi / 2) / (float64(n) * (1 + 0.02*float64(n)))
	v[0] = float32(math.Cos(angle))
	v[1] = float32(math.Sin(angle))
	return v
}

// plantOneFactStatistics replaces the planner statistics of the tables the
// KNN reads join with those of a one-fact store, as sqlite_stat1 held them in
// the #725 store before its 501-fact commit, and reloads them.
func plantOneFactStatistics(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, st := range [][3]string{
		{"facts", "facts_origin", "1 1"},
		{"facts", "facts_kind", "1 1"},
		{"facts", "facts_type", "1 1"},
		{"facts", "sqlite_autoindex_facts_1", "1 1 1"},
		{"branch_facts", "branch_facts_fact", "1 1"},
		{"branch_facts", "sqlite_autoindex_branch_facts_1", "1 1 1"},
	} {
		_, err := db.Exec(`DELETE FROM sqlite_stat1 WHERE tbl = ? AND idx = ?`, st[0], st[1])
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES (?, ?, ?)`, st[0], st[1], st[2])
		require.NoError(t, err)
	}
	for _, tbl := range []string{"facts_vec_rowids", "fact_titles_vec_rowids"} {
		_, err := db.Exec(`UPDATE sqlite_stat1 SET stat = '1' WHERE tbl = ? AND idx IS NULL`, tbl)
		require.NoError(t, err)
	}
	_, err := db.Exec(`ANALYZE sqlite_schema`)
	require.NoError(t, err)
}

// vecKNNPlan is the plan's loops: top holds the top-level SCAN/SEARCH rows
// (parent is the root), outermost first; sub holds the SCAN/SEARCH rows nested
// under anything else (a subquery). Temp b-tree rows are left out.
func vecKNNPlan(t *testing.T, db *sql.DB, q string, args []any) (top, sub []string) {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+q, args...)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		if !strings.HasPrefix(detail, "SCAN ") && !strings.HasPrefix(detail, "SEARCH ") {
			continue
		}
		if parent == 0 {
			top = append(top, detail)
		} else {
			sub = append(sub, detail)
		}
	}
	require.NoError(t, rows.Err())
	return top, sub
}

// vecKNNRows runs q and returns its rows, each rendered as text: in the order
// q returns them when ordered, else sorted.
func vecKNNRows(t *testing.T, db *sql.DB, q string, args []any, ordered bool) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	require.NoError(t, err)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		out = append(out, fmt.Sprint(vals...))
	}
	require.NoError(t, rows.Err())
	if !ordered {
		sort.Strings(out)
	}
	return out
}
