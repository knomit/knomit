package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/rs/zerolog/log"
)

// OptimizeStatsSQL is the statement every planner-statistics refresh runs.
// analysis_limit is per-connection and must ride the same Exec: without it,
// 0x10000 (consider ALL tables) can trigger an unbounded ANALYZE on a large
// corpus. 0x10002 = "all tables" + "run it" (0x1 alone would only print the
// plan). store.Open uses it too, to repair statistics that went stale while no
// process was running.
const OptimizeStatsSQL = `PRAGMA analysis_limit=1000; PRAGMA optimize=0x10002`

// isPow2 reports whether n is 1, 2, 4, 8, ...
func isPow2(n int) bool { return n > 0 && n&(n-1) == 0 }

// statsCheckpoint refreshes planner statistics when n, the number of facts
// this Sync pass has indexed so far, reaches 1, 2, 4, 8, ...
//
// Why checkpoints: SQLite plans every statement with the sqlite_stat1 rows it
// loaded, and the first Sync into a new store ANALYZEs one-row tables. A batch
// that then lands in the same pass would plan every write against those
// one-row statistics (full SCAN edges per merged edge, graphNodeIDByProps
// driven by label=?, inner SCANs under the vec0 KNN reads: #731). PRAGMA
// optimize re-analyzes a table only when its size drifted 10x or more, so
// calling it whenever the pass has doubled keeps every table inside that rule
// without a corpus-size constant: the doubling is a schedule, the 10x is
// SQLite's own.
func (si *Index) statsCheckpoint(ctx context.Context, n int) {
	if !isPow2(n) {
		return
	}
	si.refreshPlannerStats(ctx, OptimizeStatsSQL)
	if si.afterStatsCheckpoint != nil {
		si.afterStatsCheckpoint(n)
	}
}

// refreshPlannerStats runs stmt (an optimize or ANALYZE) and, when it changed
// sqlite_stat1, makes every pooled connection reload the statistics.
//
// ANALYZE reloads statistics only on the connection that ran it and does not
// touch the schema cookie, so without the bump the other pooled connections
// keep whatever they last loaded (for the life of the process) and keep
// planning with stale statistics. A schema change makes each of them reload at
// its next statement, so the bump is a no-op DDL on a dedicated table. It takes
// the write lock briefly and invalidates prepared statements, hence only when
// the sqlite_stat1 content actually changed.
//
// Must run outside any open transaction (under _txlock=immediate an open tx is
// the process-wide write lock, and the optimize may wait up to busy_timeout
// behind another writer). Best-effort: a failure is logged and never fails the
// caller.
func (si *Index) refreshPlannerStats(ctx context.Context, stmt string) {
	db := si.rh.DB()
	before, errBefore := statsDigest(ctx, db)
	if _, err := conn(ctx, db).ExecContext(ctx, stmt); err != nil {
		log.Debug().Err(err).Msg("planner stats: refresh failed (query plans may drift)")
		return
	}
	after, errAfter := statsDigest(ctx, db)
	if errBefore == nil && errAfter == nil && before == after {
		return
	}
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS planner_stats_epoch(x); DROP TABLE planner_stats_epoch`); err != nil {
		log.Debug().Err(err).Msg("planner stats: schema bump failed (other connections keep old plans)")
	}
}

// statsDigest is a digest of every sqlite_stat1 row. A store with no
// sqlite_stat1 table yet has the digest "none", so the table's creation counts
// as a change.
func statsDigest(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := conn(ctx, db).QueryContext(ctx,
		`SELECT tbl, coalesce(idx, ''), stat FROM sqlite_stat1 ORDER BY tbl, idx, stat`)
	if err != nil {
		return "none", nil // no such table: nothing analyzed yet
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var a, b, c string
		if err := rows.Scan(&a, &b, &c); err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\n", a, b, c)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
