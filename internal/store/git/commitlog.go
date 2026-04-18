// Commit log SQL operations.
// The commit_log table is a denormalized index of (commit_hash, path, committed_at, message)
// that enables O(1) activity aggregates and efficient path-history queries.
package git

import (
	"database/sql"
	"fmt"
	"strings"
)

// CommitLogEntry is one row inserted into commit_log.
type CommitLogEntry struct {
	Hash, Path, Message, Operation, AuthorEmail, Action string
	CommittedAt                                         int64
}

// CommitLogRow is one result row from CommitLogQuery.
type CommitLogRow struct {
	Hash, Message, Operation string
	Timestamp                int64
}

// CommitLogActivityResult holds aggregate activity metrics.
type CommitLogActivityResult struct {
	LastCommit                             sql.NullInt64
	Total, Changes7d, Changes30d, Changes90d int
}

// CommitLogFileRecency is a file path + timestamp of its last commit.
type CommitLogFileRecency struct {
	Path      string
	UpdatedAt int64
}

// CommitLogCursorType is the pagination direction.
type CommitLogCursorType uint8

const (
	CommitLogCursorNone   CommitLogCursorType = iota
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

// CommitLogSync is the core write method for commit_log.
// It calls iter() repeatedly until it returns ("", nil, nil) (sentinel for done).
// For each non-empty hash: if the hash already exists in commit_log, iteration stops
// (backfill dedup). All entries for a hash are inserted in a single transaction.
// The commitLog atomic is marked true once the table is confirmed to exist
// and has been previously written to (either via a new insert or confirmed
// via an existing indexed commit).
func (s *Storer) CommitLogSync(branchName string, iter func() (hash string, entries []CommitLogEntry, err error)) error {
	if !s.CommitLogAvailable() {
		return nil
	}

	// Require branch to exist. Callers must EnsureBranch before this runs.
	if branchName == "" {
		return fmt.Errorf("CommitLogSync: branchName is empty")
	}
	var branchID int64
	err := s.db.QueryRow(`SELECT id FROM branches WHERE name = ?`, branchName).Scan(&branchID)
	if err != nil {
		return fmt.Errorf("CommitLogSync: branch %q not registered in branches table: %w", branchName, err)
	}

	for {
		hash, entries, err := iter()
		if err != nil {
			return fmt.Errorf("CommitLogSync: iter: %w", err)
		}
		if hash == "" {
			// Done.
			s.commitLog.Store(true)
			return nil
		}

		// Dedup: is this commit already recorded as visible on this branch?
		// For linear history an existing row means all ancestors are already
		// recorded too, so we could stop. But for merge commits the iterator
		// is walking a DAG — hitting a known commit on one parent's line says
		// nothing about the other parent's ancestry. Skip this commit and
		// continue walking rather than short-circuiting.
		var cnt int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM branch_commits WHERE branch_id = ? AND commit_hash = ?`,
			branchID, hash).Scan(&cnt); err != nil {
			return fmt.Errorf("CommitLogSync: dedup check: %w", err)
		}
		if cnt > 0 {
			s.commitLog.Store(true)
			continue
		}

		if len(entries) == 0 {
			// Still record visibility even if no path entries (edge case).
			if _, err := s.db.Exec(
				`INSERT OR IGNORE INTO branch_commits (branch_id, commit_hash) VALUES (?, ?)`,
				branchID, hash); err != nil {
				return fmt.Errorf("CommitLogSync: record visibility: %w", err)
			}
			continue
		}

		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("CommitLogSync: begin tx: %w", err)
		}

		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO commit_log (commit_hash, path, message, operation, author_email, action, committed_at) VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("CommitLogSync: prepare commit_log: %w", err)
		}

		for _, e := range entries {
			if _, err := stmt.Exec(e.Hash, e.Path, e.Message, e.Operation, e.AuthorEmail, e.Action, e.CommittedAt); err != nil {
				stmt.Close()
				tx.Rollback()
				return fmt.Errorf("CommitLogSync: insert commit_log: %w", err)
			}
		}
		stmt.Close()

		// Record visibility for this commit on this branch.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO branch_commits (branch_id, commit_hash) VALUES (?, ?)`,
			branchID, hash); err != nil {
			tx.Rollback()
			return fmt.Errorf("CommitLogSync: insert branch_commits: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("CommitLogSync: commit tx: %w", err)
		}
		s.commitLog.Store(true)
	}
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
SELECT commit_hash, ts, message, operation
FROM (
    SELECT cl.commit_hash, MIN(cl.committed_at) AS ts, MIN(cl.message) AS message, MIN(cl.operation) AS operation, MAX(cl.rowid) AS max_rid
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
		if err := rows.Scan(&r.Hash, &r.Timestamp, &r.Message, &r.Operation); err != nil {
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

// CommitLogWalkChanged returns file paths + timestamps ordered by most recently changed,
// excluding paths in seen, up to limit results, scoped to branchID (0 = no filter).
func (s *Storer) CommitLogWalkChanged(branchID int64, prefix string, seen map[string]bool, limit int) ([]CommitLogFileRecency, error) {
	branchJoin, branchWhere, branchArgs := branchCommitsJoin(branchID)
	var whereParts []string
	var args []any

	whereParts = append(whereParts, branchWhere)
	args = append(args, branchArgs...)

	if prefix != "" {
		whereParts = append(whereParts, "cl.path GLOB ?")
		args = append(args, prefix+"/*")
	}
	if len(seen) > 0 {
		placeholders := make([]string, 0, len(seen))
		for p := range seen {
			placeholders = append(placeholders, "?")
			args = append(args, p)
		}
		whereParts = append(whereParts, "cl.path NOT IN ("+strings.Join(placeholders, ",")+")")
	}

	where := strings.Join(whereParts, " AND ")

	q := fmt.Sprintf(`
		SELECT cl.path, MAX(cl.committed_at) AS ts, MAX(cl.rowid) AS last_rowid
		FROM commit_log cl
		%s
		WHERE %s
		GROUP BY cl.path
		ORDER BY ts DESC, last_rowid DESC
		LIMIT ?`, branchJoin, where)
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("CommitLogWalkChanged: query: %w", err)
	}
	defer rows.Close()

	var results []CommitLogFileRecency
	for rows.Next() {
		var path string
		var ts, lastRowid int64
		if err := rows.Scan(&path, &ts, &lastRowid); err != nil {
			return nil, fmt.Errorf("CommitLogWalkChanged: scan: %w", err)
		}
		results = append(results, CommitLogFileRecency{
			Path:      path,
			UpdatedAt: ts,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("CommitLogWalkChanged: rows: %w", err)
	}
	return results, nil
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
