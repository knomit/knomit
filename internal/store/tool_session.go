// Tool session CRUD for progressive knowledge-base exploration and other paginated tools.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ToolSession represents a paginated tool session for a branch.
type ToolSession struct {
	ID         string
	Tool       string
	Branch     string
	PathPrefix string
	LastCommit string
	Status     string // "active", "completed", "abandoned"
	CreatedAt  string
	UpdatedAt  string
}

// QueueItem represents a single item in a tool session's work queue.
type QueueItem struct {
	Path       string
	CommitHash string
	Depth      int
}

// CreateToolSession creates a new tool session for the given tool, branch, and path prefix.
func (idx *Index) CreateToolSession(ctx context.Context, tool, branch, pathPrefix string) (*ToolSession, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	s := &ToolSession{
		ID:         uuid.New().String(),
		Tool:       tool,
		Branch:     branch,
		PathPrefix: pathPrefix,
		LastCommit: "",
		Status:     "active",
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	_, err := conn(ctx, idx.db).ExecContext(ctx,
		`INSERT INTO tool_sessions(id, tool, branch, path_prefix, last_commit, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Tool, s.Branch, s.PathPrefix, s.LastCommit, s.Status, s.CreatedAt, s.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateToolSession: %w", err)
	}
	return s, nil
}

// GetToolSession returns the session with the given ID, or nil if not found.
func (idx *Index) GetToolSession(ctx context.Context, id string) (*ToolSession, error) {
	var s ToolSession
	err := conn(ctx, idx.db).QueryRowContext(ctx,
		`SELECT id, tool, branch, path_prefix, last_commit, status, created_at, updated_at FROM tool_sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.Tool, &s.Branch, &s.PathPrefix, &s.LastCommit, &s.Status, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetToolSession: %w", err)
	}
	return &s, nil
}

// UpdateToolSession updates the last_commit, status, and updated_at for a session.
func (idx *Index) UpdateToolSession(ctx context.Context, id, lastCommit, status string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := conn(ctx, idx.db).ExecContext(ctx,
		`UPDATE tool_sessions SET last_commit = ?, status = ?, updated_at = ? WHERE id = ?`,
		lastCommit, status, now, id,
	)
	if err != nil {
		return fmt.Errorf("UpdateToolSession: %w", err)
	}
	return nil
}

// GetSeenPaths returns all seen paths for the given session as a set.
func (idx *Index) GetSeenPaths(ctx context.Context, sessionID string) (map[string]bool, error) {
	rows, err := conn(ctx, idx.db).QueryContext(ctx,
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
func (idx *Index) AddSeenPaths(ctx context.Context, sessionID string, paths []string) error {
	tx, err := idx.db.BeginTx(ctx, nil)
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
func (idx *Index) EnqueuePaths(ctx context.Context, sessionID string, items []QueueItem) error {
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("EnqueuePaths begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO tool_queue(session_id, path, commit_hash, depth) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("EnqueuePaths prepare: %w", err)
	}
	defer stmt.Close()

	for _, item := range items {
		if _, err := stmt.ExecContext(ctx, sessionID, item.Path, item.CommitHash, item.Depth); err != nil {
			return fmt.Errorf("EnqueuePaths exec: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("EnqueuePaths commit: %w", err)
	}
	return nil
}

// DequeuePaths atomically selects and deletes up to `limit` items from the queue,
// ordered by depth ASC then rowid ASC (breadth-first).
func (idx *Index) DequeuePaths(ctx context.Context, sessionID string, limit int) ([]QueueItem, error) {
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("DequeuePaths begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT rowid, path, commit_hash, depth FROM tool_queue
		 WHERE session_id = ?
		 ORDER BY depth ASC, rowid ASC
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
		if err := rows.Scan(&rowID, &item.Path, &item.CommitHash, &item.Depth); err != nil {
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

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("DequeuePaths commit: %w", err)
	}
	return items, nil
}

// QueueSize returns the number of items in the queue for a session.
func (idx *Index) QueueSize(ctx context.Context, sessionID string) (int, error) {
	var count int
	err := conn(ctx, idx.db).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tool_queue WHERE session_id = ?`, sessionID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("QueueSize: %w", err)
	}
	return count, nil
}
