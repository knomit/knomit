// Commit log SQL operations.
// The commit_log table is a denormalized index of (commit_hash, path, committed_at, message)
// that enables O(1) activity aggregates and efficient path-history queries.
package git

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CommitLogEntry is one row inserted into commit_log.
type CommitLogEntry struct {
	Hash, Path, Message, Operation, AuthorName, AuthorEmail, Action string
	CommittedAt                                                     int64
}

// CommitLogRow is one result row from CommitLogQuery.
type CommitLogRow struct {
	Hash, Message, Operation, AuthorName, AuthorEmail string
	Timestamp                                         int64
}

// CommitLogActivityResult holds aggregate activity metrics.
type CommitLogActivityResult struct {
	LastCommit                               sql.NullInt64
	Total, Changes7d, Changes30d, Changes90d int
}

// CommitLogCursorType is the pagination direction.
type CommitLogCursorType uint8

const (
	CommitLogCursorNone CommitLogCursorType = iota
	CommitLogCursorAfter
	CommitLogCursorFrom
	CommitLogCursorBefore
)

// CommitLogCursor identifies an anchor commit for paginated queries.
type CommitLogCursor struct {
	Type CommitLogCursorType
	Hash string
}

// CommitLogAvailable returns true if the commit_log table is confirmed populated.
// On first call it probes sqlite_master; if the table exists the atomic is set.
func (s *Storer) CommitLogAvailable() bool {
	if s.commitLog.Load() {
		return true
	}
	if s.db == nil {
		return false
	}
	if s.commitLogTableExists() {
		s.commitLog.Store(true)
		return true
	}
	return false
}

// commitLogTableExists checks whether the commit_log table exists in SQLite.
func (s *Storer) commitLogTableExists() bool {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='commit_log'`).Scan(&n); err != nil || n == 0 {
		return false
	}
	return true
}

// CommitLogItem is one commit to record: its ordered parents and commit_log
// entries, computed by the caller before CommitLogApply opens its transaction.
type CommitLogItem struct {
	Hash    string
	Parents []string
	Entries []CommitLogEntry
}

// CommitLogApplyOptions tunes CommitLogApply.
type CommitLogApplyOptions struct {
	// Before runs first inside the transaction — a purge or clear that must
	// become visible atomically with the re-recording that follows.
	Before func(ctx context.Context, tx *sql.Tx) error
	// Derive (REQUIRED) runs inside the transaction for every item newly
	// recorded on the branch, right after its rows, in item order (callers
	// order parents first). Whatever it writes becomes visible atomically with
	// the commit, and an error rolls the whole transaction back.
	Derive func(ctx context.Context, tx *sql.Tx, i int) error
}

// CommitLogApply is the ONLY writer of branch_commits visibility for new
// commits (with CreateBranch's copy of an already-indexed branch). It records
// items on branchName in ONE transaction — the one
// ctx carries, if any (then nothing is committed here), else its own:
// Before, then for each item not yet visible on the branch its commit_log,
// branch_commits and commit_parents rows and Derive.
//
// Everything that reads git objects belongs in the caller, BEFORE this call:
// the repo DB takes the write lock at BEGIN (_txlock=immediate) from a pool
// of 4 connections, so a transaction that waits for a pool connection (an
// object read) while other writers hold the rest waiting for its lock
// starves until the busy timeout. Inside, only SQL runs, on tx.
func (s *Storer) CommitLogApply(ctx context.Context, branchName string, items []CommitLogItem, opts CommitLogApplyOptions) error {
	if !s.CommitLogAvailable() {
		return nil
	}
	if branchName == "" {
		return fmt.Errorf("CommitLogApply: branchName is empty")
	}
	if opts.Derive == nil {
		// Recording a commit without the hook would make it visible without
		// its derived history (see internal/store/path_changes.go).
		return fmt.Errorf("CommitLogApply: Derive is required")
	}
	ctx, tx, own, err := BeginTxIfNeeded(ctx, s.db)
	if err != nil {
		return fmt.Errorf("CommitLogApply: begin tx: %w", err)
	}
	if own {
		defer tx.Rollback()
	}
	var branchID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM branches WHERE name = ?`, branchName).Scan(&branchID); err != nil {
		return fmt.Errorf("CommitLogApply: branch %q not registered in branches table: %w", branchName, err)
	}
	if opts.Before != nil {
		if err := opts.Before(ctx, tx); err != nil {
			return err
		}
	}
	for i, it := range items {
		var cnt int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM branch_commits WHERE branch_id = ? AND commit_hash = ?`,
			branchID, it.Hash).Scan(&cnt); err != nil {
			return fmt.Errorf("CommitLogApply: dedup check: %w", err)
		}
		if cnt > 0 {
			continue
		}
		for _, e := range it.Entries {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO commit_log (commit_hash, path, message, operation, author_name, author_email, action, committed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				e.Hash, e.Path, e.Message, e.Operation, e.AuthorName, e.AuthorEmail, e.Action, e.CommittedAt); err != nil {
				return fmt.Errorf("CommitLogApply: insert commit_log: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO branch_commits (branch_id, commit_hash) VALUES (?, ?)`,
			branchID, it.Hash); err != nil {
			return fmt.Errorf("CommitLogApply: insert branch_commits: %w", err)
		}
		for j, p := range it.Parents {
			if p == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO commit_parents (commit_hash, parent_order, parent_hash) VALUES (?, ?, ?)`,
				it.Hash, j, p); err != nil {
				return fmt.Errorf("CommitLogApply: insert commit_parents: %w", err)
			}
		}
		if err := opts.Derive(ctx, tx, i); err != nil {
			return fmt.Errorf("CommitLogApply: derive %s: %w", it.Hash, err)
		}
	}
	if own {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("CommitLogApply: commit tx: %w", err)
		}
	}
	s.commitLog.Store(true)
	return nil
}

// CommitLogQuery performs a paginated query on commit_log scoped to branchID.
// Returns rows, hasMore, error. Fetches limit+1 rows and returns limit.
// branchID == 0 means no branch filter (not used in normal operation).
func (s *Storer) CommitLogQuery(branchID int64, path string, cursor CommitLogCursor, limit int) ([]CommitLogRow, bool, error) {
	pathCond, pathArgs := commitLogPathCondPrefixed(path, "cl.")
	branchJoin, branchWhere, branchArgs := branchCommitsJoin(branchID)

	lookupTS := func(hash string) (int64, error) {
		var ts int64
		err := s.db.QueryRow(
			`SELECT MIN(committed_at) FROM commit_log WHERE commit_hash = ?`, hash,
		).Scan(&ts)
		return ts, err
	}

	lookupMaxRowid := func(hash string) (int64, error) {
		var rid int64
		err := s.db.QueryRow(
			`SELECT MAX(rowid) FROM commit_log WHERE commit_hash = ?`, hash,
		).Scan(&rid)
		return rid, err
	}

	var cursorCond string
	var cursorArgs []any
	switch cursor.Type {
	case CommitLogCursorBefore:
		ts, err := lookupTS(cursor.Hash)
		if err != nil {
			return nil, false, fmt.Errorf("CommitLogQuery: before lookup: %w", err)
		}
		rid, err := lookupMaxRowid(cursor.Hash)
		if err != nil {
			return nil, false, fmt.Errorf("CommitLogQuery: before rowid lookup: %w", err)
		}
		cursorCond = "(ts > ? OR (ts = ? AND max_rid > ?))"
		cursorArgs = []any{ts, ts, rid}
	case CommitLogCursorFrom:
		ts, err := lookupTS(cursor.Hash)
		if err != nil {
			return nil, false, fmt.Errorf("CommitLogQuery: from lookup: %w", err)
		}
		rid, err := lookupMaxRowid(cursor.Hash)
		if err != nil {
			return nil, false, fmt.Errorf("CommitLogQuery: from rowid lookup: %w", err)
		}
		cursorCond = "(ts < ? OR (ts = ? AND max_rid <= ?))"
		cursorArgs = []any{ts, ts, rid}
	case CommitLogCursorAfter:
		ts, err := lookupTS(cursor.Hash)
		if err != nil {
			return nil, false, fmt.Errorf("CommitLogQuery: after lookup: %w", err)
		}
		rid, err := lookupMaxRowid(cursor.Hash)
		if err != nil {
			return nil, false, fmt.Errorf("CommitLogQuery: after rowid lookup: %w", err)
		}
		cursorCond = "(ts < ? OR (ts = ? AND max_rid < ?))"
		cursorArgs = []any{ts, ts, rid}
	default:
		cursorCond = "1=1"
	}

	query := `
SELECT commit_hash, ts, message, operation, author_name, author_email
FROM (
    SELECT cl.commit_hash, MIN(cl.committed_at) AS ts, MIN(cl.message) AS message, MIN(cl.operation) AS operation, MIN(cl.author_name) AS author_name, MIN(cl.author_email) AS author_email, MAX(cl.rowid) AS max_rid
    FROM commit_log cl
    ` + branchJoin + `
    WHERE ` + branchWhere + ` AND ` + pathCond + `
    GROUP BY cl.commit_hash
)
WHERE ` + cursorCond + `
ORDER BY ts DESC, max_rid DESC
LIMIT ?`

	args := append(branchArgs, pathArgs...)
	args = append(args, cursorArgs...)
	args = append(args, limit+1)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("CommitLogQuery: query: %w", err)
	}
	defer rows.Close()

	var results []CommitLogRow
	for rows.Next() {
		var r CommitLogRow
		if err := rows.Scan(&r.Hash, &r.Timestamp, &r.Message, &r.Operation, &r.AuthorName, &r.AuthorEmail); err != nil {
			return nil, false, fmt.Errorf("CommitLogQuery: scan: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("CommitLogQuery: rows: %w", err)
	}

	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}
	return results, hasMore, nil
}

// CommitLogFileCounts returns map[commitHash]map[action]count for the given hashes.
func (s *Storer) CommitLogFileCounts(hashes []string) (map[string]map[string]int, error) {
	if len(hashes) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(hashes))
	args := make([]any, len(hashes))
	for i, h := range hashes {
		placeholders[i] = "?"
		args[i] = h
	}

	query := `SELECT commit_hash, action, COUNT(*) FROM commit_log WHERE commit_hash IN (` +
		strings.Join(placeholders, ",") +
		`) GROUP BY commit_hash, action`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("CommitLogFileCounts: query: %w", err)
	}
	defer rows.Close()

	result := make(map[string]map[string]int)
	for rows.Next() {
		var hash, action string
		var count int
		if err := rows.Scan(&hash, &action, &count); err != nil {
			return nil, fmt.Errorf("CommitLogFileCounts: scan: %w", err)
		}
		if result[hash] == nil {
			result[hash] = make(map[string]int)
		}
		result[hash][action] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("CommitLogFileCounts: rows: %w", err)
	}
	return result, nil
}

// CommitLogActivity returns aggregate activity metrics for the given path,
// scoped to branchID (0 = no filter).
func (s *Storer) CommitLogActivity(branchID int64, path string, cutoff7, cutoff30, cutoff90 int64) (CommitLogActivityResult, error) {
	branchJoin, branchWhere, branchArgs := branchCommitsJoin(branchID)
	pathCond, pathArgs := commitLogPathCondPrefixed(path, "cl.")
	args := append([]any{cutoff7, cutoff30, cutoff90}, branchArgs...)
	args = append(args, pathArgs...)

	q := fmt.Sprintf(`
		SELECT MAX(cl.committed_at),
		       COUNT(DISTINCT cl.commit_hash),
		       COUNT(DISTINCT CASE WHEN cl.committed_at > ? THEN cl.commit_hash END),
		       COUNT(DISTINCT CASE WHEN cl.committed_at > ? THEN cl.commit_hash END),
		       COUNT(DISTINCT CASE WHEN cl.committed_at > ? THEN cl.commit_hash END)
		FROM commit_log cl %s WHERE %s AND %s`, branchJoin, branchWhere, pathCond)

	var r CommitLogActivityResult
	if err := s.db.QueryRow(q, args...).Scan(&r.LastCommit, &r.Total, &r.Changes7d, &r.Changes30d, &r.Changes90d); err != nil {
		return CommitLogActivityResult{}, fmt.Errorf("CommitLogActivity: %w", err)
	}
	return r, nil
}

// commitLogPathCond returns the SQL WHERE fragment and bind args for filtering
// commit_log rows by path. Empty path matches all rows.
func commitLogPathCond(path string) (cond string, args []any) {
	return commitLogPathCondPrefixed(path, "")
}

// commitLogPathCondPrefixed is like commitLogPathCond but prepends the given
// table alias (e.g. "cl.") to the path column. Used when the commit_log table
// is joined with branch_commits and path must be disambiguated.
func commitLogPathCondPrefixed(path, prefix string) (cond string, args []any) {
	col := prefix + "path"
	if path == "" {
		return "1=1", nil
	}
	if strings.HasSuffix(path, ".md") {
		return col + " = ?", []any{path}
	}
	return col + " GLOB ?", []any{path + "/*"}
}

// branchCommitsJoin returns a SQL JOIN fragment scoping commit_log (aliased as
// cl) to a branch via branch_commits. If branchID == 0, returns an empty JOIN
// and a tautology WHERE predicate so callers can unconditionally concatenate.
func branchCommitsJoin(branchID int64) (join, where string, args []any) {
	if branchID == 0 {
		return "", "1=1", nil
	}
	return "JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash",
		"bc.branch_id = ?",
		[]any{branchID}
}
