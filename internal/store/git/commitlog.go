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

// CommitLogPayload produces the rows for one commit: its ordered parent hashes
// and its commit_log entries.
//
// It is deliberately a thunk rather than a value. CommitLogSync calls it ONLY
// for a commit that is not yet recorded on the branch, because computing it is
// expensive — in the production caller it is an object.DiffTree of the commit
// against its first parent, ~300 SQLite object loads and ~2 ms per commit. A
// warm repo open re-walks a fully-populated DAG, so nearly every commit is a
// dedup hit; computing the payload eagerly made repo open cost ~2 ms per commit
// (4.2 s for a 1831-commit repo) to produce rows that were immediately thrown
// away. See CommitLogSync's dedup step.
//
// `parents` is the ordered list of parent commit hashes for this commit
// (parents[0] is the first parent, etc.). Used by resolveActiveCommitForPath's
// recursive-CTE walk and replaces the retired first_parent_chain virtual
// table whose Go cursor callback could re-enter the *sql.DB pool mid-scan
// and deadlock.
type CommitLogPayload func() (parents []string, entries []CommitLogEntry, err error)

// CommitLogSyncOptions tunes CommitLogSyncWith.
type CommitLogSyncOptions struct {
	// Derive, when set, runs INSIDE the transaction that records hash — after
	// its commit_log, branch_commits and commit_parents rows are written — so
	// whatever it derives becomes visible atomically with the commit. It is
	// called for every commit newly recorded on the branch, in iteration order;
	// the caller iterates parents before children when Derive needs them.
	Derive func(ctx context.Context, tx *sql.Tx, hash string) error
	// Batch is how many commits one transaction records (default 1). Larger
	// batches amortise the commit (one WAL fsync) over many commits for bulk
	// population; they also hold the write lock for longer.
	Batch int
}

// CommitLogSync is CommitLogSyncWith with no options: one transaction per
// commit, no derivation.
func (s *Storer) CommitLogSync(branchName string, iter func() (hash string, payload CommitLogPayload, err error)) error {
	return s.CommitLogSyncWith(context.Background(), branchName, iter, CommitLogSyncOptions{})
}

// CommitLogSyncWith is the core write method for commit_log.
// It calls iter() repeatedly until it returns ("", nil, nil) (sentinel for done).
// For each non-empty hash: if the commit is already recorded as visible on this
// branch it is skipped WITHOUT calling its payload, and the walk continues. All
// rows for a hash — commit_log entries, branch_commits visibility,
// commit_parents edges and whatever opts.Derive writes — are inserted in one
// transaction (shared by up to opts.Batch commits).
//
// When ctx carries a transaction (TxFromContext), everything runs inside it and
// nothing is committed here: the caller owns atomicity (a purge or rebuild
// that must not expose an empty branch between delete and repopulate).
//
// iter must return the hash cheaply; all per-commit work belongs in the
// returned CommitLogPayload so the dedup check can gate it.
func (s *Storer) CommitLogSyncWith(ctx context.Context, branchName string, iter func() (hash string, payload CommitLogPayload, err error), opts CommitLogSyncOptions) error {
	if !s.CommitLogAvailable() {
		return nil
	}
	if branchName == "" {
		return fmt.Errorf("CommitLogSync: branchName is empty")
	}
	batch := max(opts.Batch, 1)
	outer := TxFromContext(ctx)
	q := Conn(ctx, s.db)

	// Require branch to exist. Callers must EnsureBranch before this runs.
	var branchID int64
	if err := q.QueryRowContext(ctx, `SELECT id FROM branches WHERE name = ?`, branchName).Scan(&branchID); err != nil {
		return fmt.Errorf("CommitLogSync: branch %q not registered in branches table: %w", branchName, err)
	}

	var tx *sql.Tx
	pending := 0
	commit := func() error {
		if tx == nil || tx == outer {
			tx, pending = nil, 0
			return nil
		}
		err := tx.Commit()
		tx, pending = nil, 0
		if err != nil {
			return fmt.Errorf("CommitLogSync: commit tx: %w", err)
		}
		return nil
	}
	fail := func(err error) error {
		if tx != nil && tx != outer {
			tx.Rollback()
		}
		return err
	}

	for {
		hash, payload, err := iter()
		if err != nil {
			return fail(fmt.Errorf("CommitLogSync: iter: %w", err))
		}
		if hash == "" {
			if err := commit(); err != nil {
				return err
			}
			s.commitLog.Store(true)
			return nil
		}

		// Dedup: is this commit already recorded as visible on this branch?
		// For merge commits the iterator walks a DAG — hitting a known commit on
		// one parent's line says nothing about the other parent's ancestry — so
		// skip and continue rather than short-circuiting. Checked through the
		// open transaction when there is one, so rows written earlier in the
		// same batch, or deleted by the caller's transaction, are seen.
		dq := q
		if tx != nil {
			dq = tx
		}
		var cnt int
		if err := dq.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM branch_commits WHERE branch_id = ? AND commit_hash = ?`,
			branchID, hash).Scan(&cnt); err != nil {
			return fail(fmt.Errorf("CommitLogSync: dedup check: %w", err))
		}
		if cnt > 0 {
			s.commitLog.Store(true)
			continue
		}

		var parents []string
		var entries []CommitLogEntry
		if payload != nil {
			if parents, entries, err = payload(); err != nil {
				return fail(fmt.Errorf("CommitLogSync: payload for %s: %w", hash, err))
			}
		}

		if tx == nil {
			if outer != nil {
				tx = outer
			} else if tx, err = s.db.BeginTx(ctx, nil); err != nil {
				return fmt.Errorf("CommitLogSync: begin tx: %w", err)
			}
		}

		for _, e := range entries {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO commit_log (commit_hash, path, message, operation, author_name, author_email, action, committed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				e.Hash, e.Path, e.Message, e.Operation, e.AuthorName, e.AuthorEmail, e.Action, e.CommittedAt); err != nil {
				return fail(fmt.Errorf("CommitLogSync: insert commit_log: %w", err))
			}
		}
		// Record visibility for this commit on this branch.
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO branch_commits (branch_id, commit_hash) VALUES (?, ?)`,
			branchID, hash); err != nil {
			return fail(fmt.Errorf("CommitLogSync: insert branch_commits: %w", err))
		}
		// Record parent edges. INSERT OR IGNORE keeps this idempotent across
		// branches and re-syncs: every branch that walks the same DAG
		// converges on the same commit_parents rows.
		for i, p := range parents {
			if p == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO commit_parents (commit_hash, parent_order, parent_hash) VALUES (?, ?, ?)`,
				hash, i, p); err != nil {
				return fail(fmt.Errorf("CommitLogSync: insert commit_parents: %w", err))
			}
		}
		if opts.Derive != nil {
			if err := opts.Derive(WithTx(ctx, tx), tx, hash); err != nil {
				return fail(fmt.Errorf("CommitLogSync: derive %s: %w", hash, err))
			}
		}
		s.commitLog.Store(true)
		if pending++; pending >= batch {
			if err := commit(); err != nil {
				return err
			}
		}
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
