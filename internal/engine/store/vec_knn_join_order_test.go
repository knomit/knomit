package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/internal/sqlite"
	"knomit/internal/engine/store/vocab"
)

// vecKNNCase is one vec0 KNN read under test: its exact production SQL, the
// alias of its vec0 table, and the arguments to run it with.
type vecKNNCase struct {
	name, sql, alias string
	args             []any
}

// Every vec0 KNN read that joins its vec0 table to another table keeps the
// vec0 table as the OUTER loop, and the fixed order changes the plan only: the
// rows equal those of the same SQL with each CROSS JOIN written as a plain
// JOIN. It holds under three planner-statistics states: none, a fresh
// ANALYZE, and STALE statistics that still describe a one-fact store (what
// PRAGMA optimize leaves before a large batch lands; the rows below are the
// ones dumped from the #725 store before its 501-fact commit).
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
			if stats == "stale one-fact statistics" {
				plantOneFactStatistics(t, db)
			}
			for _, c := range cases {
				loops := vecKNNLoops(t, db, c.sql, c.args)
				require.NotEmpty(t, loops, "%s: no loops in the plan", c.name)
				require.True(t, strings.HasPrefix(loops[0], "SCAN "+c.alias+" VIRTUAL TABLE"),
					"%s: the outer loop must be the vec0 table %q, got plan:\n%s",
					c.name, c.alias, strings.Join(loops, "\n"))

				plain := strings.ReplaceAll(c.sql, "CROSS JOIN", "JOIN")
				t.Logf("%s\n  pinned: %s\n  plain:  %s", c.name,
					strings.Join(loops, " | "), strings.Join(vecKNNLoops(t, db, plain, c.args), " | "))
				got := vecKNNRows(t, db, c.sql, c.args)
				require.NotEmpty(t, got, "%s: the fixture must give the read rows to compare", c.name)
				require.Equal(t, vecKNNRows(t, db, plain, c.args), got,
					"%s: pinning the join order must not change the rows", c.name)
			}
		})
	}
}

// vecKNNFixture is a store whose facts and both vec tables hold live and
// superseded rows with distinct, ordered vectors, and every vec0 KNN read of
// the store with arguments whose windows straddle both.
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
		blob := sqlite.Float32SliceToBytes(ringVector(i, len(ids), 768))
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
	// Row 12's neighbourhood holds superseded (rows 8, 9) and live rows.
	probe := sqlite.Float32SliceToBytes(ringVector(12, len(ids), 768))

	a := index.KNNArgsForTest{Vec: probe, K: 15, BranchID: branchID, Path: "kb/f/20.md", MinVec: 0}
	var cases []vecKNNCase
	for _, q := range index.VecKNNQueriesForTest() {
		args := a
		if q.Name == "methodology" {
			args.K = len(ids) // the whole table, as RelevantMethodologyForFact asks
		}
		cases = append(cases, vecKNNCase{q.Name, q.SQL, q.VecAlias, q.Args(args)})
	}
	cases = append(cases, vecKNNCase{"title (TopTitleNeighbours)", vocab.TitleKNNSQLForTest(), "tv",
		[]any{probe, 8, branch, liveID}})
	return db, cases
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

// vecKNNLoops is the plan's top-level loops (SCAN/SEARCH rows whose parent is
// the root), outermost first; subquery and temp b-tree rows are left out.
func vecKNNLoops(t *testing.T, db *sql.DB, q string, args []any) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+q, args...)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		if parent == 0 && (strings.HasPrefix(detail, "SCAN ") || strings.HasPrefix(detail, "SEARCH ")) {
			out = append(out, detail)
		}
	}
	require.NoError(t, rows.Err())
	return out
}

// vecKNNRows runs q and returns its rows, each rendered as text, sorted (two
// plans may list equal-distance rows in a different order).
func vecKNNRows(t *testing.T, db *sql.DB, q string, args []any) []string {
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
	sort.Strings(out)
	return out
}
