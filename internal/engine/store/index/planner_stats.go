package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	storegit "knomit/internal/engine/store/git"

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
// one-row statistics (`SCAN edges` per merged edge, inner SCANs under the vec0
// KNN reads: #731). PRAGMA
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

// refreshPlannerStats is RefreshPlannerStats for this index's database; a
// failure is logged at debug (best-effort: it never fails the caller).
func (si *Index) refreshPlannerStats(ctx context.Context, stmt string) {
	if err := RefreshPlannerStats(ctx, si.rh.DB(), stmt); err != nil {
		log.Debug().Err(err).Msg("planner stats: refresh failed (query plans may drift)")
	}
}

// RefreshPlannerStats runs stmt (an optimize or ANALYZE) and, when it changed
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
// Must run outside any open transaction: under _txlock=immediate an open tx is
// the process-wide write lock, so the statements, which run on db's own pooled
// connections, would wait on the caller's own lock (measured: 10.6 s, then
// "database is locked"; see kb/invariants/store/transactions/212e7e44.md). A
// ctx that carries a tx is therefore refused: nothing runs. Outside a tx the
// statements may still wait up to busy_timeout behind another writer.
//
// The returned error is the statement's own failure only; a failed digest or
// bump is logged and skipped. Callers treat it as best-effort.
func RefreshPlannerStats(ctx context.Context, db *sql.DB, stmt string) error {
	if storegit.TxFromContext(ctx) != nil {
		log.Debug().Msg("planner stats: refresh skipped, ctx carries an open transaction")
		return nil
	}
	before, errBefore := statsDigest(ctx, db)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return err
	}
	after, errAfter := statsDigest(ctx, db)
	if errBefore != nil || errAfter != nil {
		// Cannot tell whether the statistics changed: skip the write-lock bump.
		log.Debug().AnErr("before", errBefore).AnErr("after", errAfter).
			Msg("planner stats: digest failed, schema bump skipped (other connections may keep old plans)")
		return nil
	}
	if before == after {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS planner_stats_epoch(x); DROP TABLE planner_stats_epoch`); err != nil {
		log.Debug().Err(err).Msg("planner stats: schema bump failed (other connections keep old plans)")
	}
	return nil
}

// statsDigest is a digest of every sqlite_stat1 row. A store with no
// sqlite_stat1 table yet has the digest "none", so the table's creation counts
// as a change. Any other error is returned.
func statsDigest(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT tbl, coalesce(idx, ''), stat FROM sqlite_stat1 ORDER BY tbl, idx, stat`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return "none", nil
		}
		return "", err
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
