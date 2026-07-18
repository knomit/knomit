// Tool session CRUD for progressive knowledge-base exploration and other paginated tools.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// toolIndex persists tool-session paging state. It targets the ephemeral
// session DB (db), NOT the main git-derived DB — so every method uses the db
// handle directly and never conn(ctx, …): the context may carry a *sql.Tx bound
// to the MAIN db, and conn would hand that back, executing session SQL against
// the wrong database. It deliberately holds no *repoHandler, so there is no main
// DB handle to reach for by mistake.
type toolIndex struct {
	db *sql.DB
}

var _ ToolSessionIndex = (*toolIndex)(nil)

// ToolSession represents a paginated tool session for a branch.
type ToolSession struct {
	ID         string
	Tool       string
	Branch     string
	PathPrefix string
	Binding    string
	ReadSet    string
	LastCommit string
	Status     string // "active", "completed", "abandoned"
	CreatedAt  string
	UpdatedAt  string
}

// QueueItem represents a single item in a tool session's work queue. SortKey is
// the SQL-orderable consume order (breadth-first depth for explain/explore;
// rank index for query). State is an optional per-item JSON payload (query
// stores the frozen rank score here; the fact body is re-read from path+commit
// on resume, so the snapshot carries no body text).
type QueueItem struct {
	Path       string
	CommitHash string
	SortKey    int
	State      string
}

// CreateToolSession creates a new tool session for the given tool, branch, path
// prefix, binding, and read-set fingerprint. The binding pins the cursor to one
// binding's identity and readSet pins its read set (each mount at its branch);
// resume through another binding, or against a re-pinned read set, is rejected
// (lenses RFC §7.3).
func (ti *toolIndex) CreateToolSession(ctx context.Context, tool, branch, pathPrefix, binding, readSet string) (*ToolSession, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	s := &ToolSession{
		ID:         uuid.New().String(),
		Tool:       tool,
		Branch:     branch,
		PathPrefix: pathPrefix,
		Binding:    binding,
		ReadSet:    readSet,
		LastCommit: "",
		Status:     "active",
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	_, err := ti.db.ExecContext(ctx,
		`INSERT INTO tool_sessions(id, tool, branch, path_prefix, binding, read_set, last_commit, status, created_at, updated_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Tool, s.Branch, s.PathPrefix, s.Binding, s.ReadSet, s.LastCommit, s.Status, s.CreatedAt, s.UpdatedAt, now,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateToolSession: %w", err)
	}
	return s, nil
}

// GetToolSession returns the session with the given ID, or nil if not found.
func (ti *toolIndex) GetToolSession(ctx context.Context, id string) (*ToolSession, error) {
	var s ToolSession
	err := ti.db.QueryRowContext(ctx,
		`SELECT id, tool, branch, path_prefix, binding, read_set, last_commit, status, created_at, updated_at FROM tool_sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.Tool, &s.Branch, &s.PathPrefix, &s.Binding, &s.ReadSet, &s.LastCommit, &s.Status, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetToolSession: %w", err)
	}
	return &s, nil
}

// UpdateToolSession updates the last_commit, status, and updated_at for a session.
func (ti *toolIndex) UpdateToolSession(ctx context.Context, id, lastCommit, status string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := ti.db.ExecContext(ctx,
		`UPDATE tool_sessions SET last_commit = ?, status = ?, updated_at = ? WHERE id = ?`,
		lastCommit, status, now, id,
	)
	if err != nil {
		return fmt.Errorf("UpdateToolSession: %w", err)
	}
	return nil
}

// GetSeenPaths returns all seen paths for the given session as a set.
func (ti *toolIndex) GetSeenPaths(ctx context.Context, sessionID string) (map[string]bool, error) {
	rows, err := ti.db.QueryContext(ctx,
		`SELECT path FROM tool_seen_paths WHERE session_id = ?`, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("GetSeenPaths: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("GetSeenPaths scan: %w", err)
		}
		seen[p] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetSeenPaths rows: %w", err)
	}
	return seen, nil
}

// AddSeenPaths batch-inserts seen paths for a session, ignoring duplicates.
func (ti *toolIndex) AddSeenPaths(ctx context.Context, sessionID string, paths []string) error {
	tx, err := ti.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("AddSeenPaths begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO tool_seen_paths(session_id, path) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("AddSeenPaths prepare: %w", err)
	}
	defer stmt.Close()

	for _, p := range paths {
		if _, err := stmt.ExecContext(ctx, sessionID, p); err != nil {
			return fmt.Errorf("AddSeenPaths exec: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("AddSeenPaths commit: %w", err)
	}
	return nil
}

// EnqueuePaths batch-inserts items into the tool_queue for a session, ignoring duplicates.
func (ti *toolIndex) EnqueuePaths(ctx context.Context, sessionID string, items []QueueItem) error {
	tx, err := ti.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("EnqueuePaths begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO tool_queue(session_id, path, commit_hash, sort_key, state) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("EnqueuePaths prepare: %w", err)
	}
	defer stmt.Close()

	for _, item := range items {
		if _, err := stmt.ExecContext(ctx, sessionID, item.Path, item.CommitHash, item.SortKey, item.State); err != nil {
			return fmt.Errorf("EnqueuePaths exec: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("EnqueuePaths commit: %w", err)
	}
	return nil
}

// DequeuePaths atomically selects and deletes up to `limit` items from the
// queue, ordered by sort_key ASC then rowid ASC (breadth-first for explain;
// rank order for query). It also bumps the session's last_used_at in the same
// transaction so an actively-paged session is never reaped as idle.
func (ti *toolIndex) DequeuePaths(ctx context.Context, sessionID string, limit int) ([]QueueItem, error) {
	tx, err := ti.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("DequeuePaths begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT rowid, path, commit_hash, sort_key, state FROM tool_queue
		 WHERE session_id = ?
		 ORDER BY sort_key ASC, rowid ASC
		 LIMIT ?`,
		sessionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("DequeuePaths select: %w", err)
	}

	var items []QueueItem
	var rowIDs []int64
	for rows.Next() {
		var rowID int64
		var item QueueItem
		if err := rows.Scan(&rowID, &item.Path, &item.CommitHash, &item.SortKey, &item.State); err != nil {
			rows.Close()
			return nil, fmt.Errorf("DequeuePaths scan: %w", err)
		}
		items = append(items, item)
		rowIDs = append(rowIDs, rowID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DequeuePaths rows: %w", err)
	}

	// Delete the dequeued rows.
	for _, rowID := range rowIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tool_queue WHERE rowid = ?`, rowID); err != nil {
			return nil, fmt.Errorf("DequeuePaths delete: %w", err)
		}
	}

	// Heartbeat: keep the session alive against the idle reaper.
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`UPDATE tool_sessions SET last_used_at = ? WHERE id = ?`, now, sessionID,
	); err != nil {
		return nil, fmt.Errorf("DequeuePaths touch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("DequeuePaths commit: %w", err)
	}
	return items, nil
}

// QueueSize returns the number of items in the queue for a session.
func (ti *toolIndex) QueueSize(ctx context.Context, sessionID string) (int, error) {
	var count int
	err := ti.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tool_queue WHERE session_id = ?`, sessionID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("QueueSize: %w", err)
	}
	return count, nil
}
