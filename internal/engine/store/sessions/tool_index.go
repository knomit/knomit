// Tool session CRUD for progressive knowledge-base exploration and other paginated tools.
package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"knomit/internal/engine/store/model"

	"github.com/google/uuid"
	sqlite3 "github.com/mattn/go-sqlite3"
)

// ToolIndex persists tool-session paging state. It targets the ephemeral
// session DB (db), NOT the main git-derived DB — so every method uses the db
// handle directly and never conn(ctx, …): the context may carry a *sql.Tx bound
// to the MAIN db, and conn would hand that back, executing session SQL against
// the wrong database. It deliberately holds no *repoHandler, so there is no main
// DB handle to reach for by mistake.
type ToolIndex struct {
	db *sql.DB
	// waits records the book writes' lock waits (book_index.go).
	waits bookLockWaits
	// booksDir holds the book files (book_file.go); "" disables books.
	booksDir string
	// booksMu guards openBooks: how many calls hold each book file open
	// right now. The sweeper never deletes a file with a holder.
	booksMu   sync.Mutex
	openBooks map[string]int
}

// CreateToolSession creates a new tool session for the given tool, branch, path
// prefix, binding, and read-set fingerprint. The binding pins the cursor to one
// binding's identity and readSet pins its read set (each mount at its branch);
// resume through another binding, or against a re-pinned read set, is rejected
// (lenses RFC §7.3). owner is who may resume it (the MCP endpoint plus the
// caller's principal); the resuming handler compares it.
func (ti *ToolIndex) CreateToolSession(ctx context.Context, tool, branch, pathPrefix, binding, readSet, owner string) (*model.ToolSession, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	s := &model.ToolSession{
		ID:         uuid.New().String(),
		Tool:       tool,
		Branch:     branch,
		PathPrefix: pathPrefix,
		Binding:    binding,
		ReadSet:    readSet,
		Owner:      owner,
		LastCommit: "",
		Status:     "active",
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	_, err := ti.db.ExecContext(ctx,
		`INSERT INTO tool_sessions(id, tool, branch, path_prefix, binding, read_set, owner, last_commit, status, created_at, updated_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Tool, s.Branch, s.PathPrefix, s.Binding, s.ReadSet, s.Owner, s.LastCommit, s.Status, s.CreatedAt, s.UpdatedAt, now,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateToolSession: %w", err)
	}
	return s, nil
}

// GetToolSession returns the session with the given ID, or nil if not found.
func (ti *ToolIndex) GetToolSession(ctx context.Context, id string) (*model.ToolSession, error) {
	var s model.ToolSession
	err := ti.db.QueryRowContext(ctx,
		`SELECT id, tool, branch, path_prefix, binding, read_set, owner, last_commit, status, created_at, updated_at FROM tool_sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.Tool, &s.Branch, &s.PathPrefix, &s.Binding, &s.ReadSet, &s.Owner, &s.LastCommit, &s.Status, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetToolSession: %w", err)
	}
	return &s, nil
}

// UpdateToolSession updates the last_commit, status, and updated_at for a session.
func (ti *ToolIndex) UpdateToolSession(ctx context.Context, id, lastCommit, status string) error {
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
func (ti *ToolIndex) GetSeenPaths(ctx context.Context, sessionID string) (map[string]bool, error) {
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
func (ti *ToolIndex) AddSeenPaths(ctx context.Context, sessionID string, paths []string) error {
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
func (ti *ToolIndex) EnqueuePaths(ctx context.Context, sessionID string, items []model.QueueItem) error {
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
//
// BEGIN IMMEDIATE, on a connection of its own (as createPipelineSession): the
// transaction reads the queue and then deletes from it, and a DEFERRED
// transaction whose read began before another cursor call committed fails its
// first write with SQLITE_BUSY ("database is locked") at once, which the busy
// timeout cannot wait out. The session DB is one file for every cursor, so two
// walks on DIFFERENT cursors collide this way too. Taking the write lock up
// front makes a concurrent dequeue wait its turn, then read the queue as the
// winner left it, so no item is ever handed to two callers.
func (ti *ToolIndex) DequeuePaths(ctx context.Context, sessionID string, limit int) ([]model.QueueItem, error) {
	tx, err := ti.db.Conn(ctx)
	if err != nil {
		return nil, busy(fmt.Errorf("DequeuePaths conn: %w", err))
	}
	defer tx.Close()
	if _, err := tx.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, busy(fmt.Errorf("DequeuePaths begin: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = tx.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	rows, err := tx.QueryContext(ctx,
		`SELECT rowid, path, commit_hash, sort_key, state FROM tool_queue
		 WHERE session_id = ?
		 ORDER BY sort_key ASC, rowid ASC
		 LIMIT ?`,
		sessionID, limit,
	)
	if err != nil {
		return nil, busy(fmt.Errorf("DequeuePaths select: %w", err))
	}

	var items []model.QueueItem
	var rowIDs []int64
	for rows.Next() {
		var rowID int64
		var item model.QueueItem
		if err := rows.Scan(&rowID, &item.Path, &item.CommitHash, &item.SortKey, &item.State); err != nil {
			rows.Close()
			return nil, busy(fmt.Errorf("DequeuePaths scan: %w", err))
		}
		items = append(items, item)
		rowIDs = append(rowIDs, rowID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, busy(fmt.Errorf("DequeuePaths rows: %w", err))
	}

	// Delete the dequeued rows.
	for _, rowID := range rowIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tool_queue WHERE rowid = ?`, rowID); err != nil {
			return nil, busy(fmt.Errorf("DequeuePaths delete: %w", err))
		}
	}

	// Heartbeat: keep the session alive against the idle reaper.
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`UPDATE tool_sessions SET last_used_at = ? WHERE id = ?`, now, sessionID,
	); err != nil {
		return nil, busy(fmt.Errorf("DequeuePaths touch: %w", err))
	}

	if _, err := tx.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, busy(fmt.Errorf("DequeuePaths commit: %w", err))
	}
	committed = true
	return items, nil
}

// busy marks err with model.ErrSessionBusy when SQLite reports the session DB
// busy or locked (the busy timeout ran out). DequeuePaths alone uses it: its
// transaction is rolled back on every error, so the caller may retry.
func busy(err error) error {
	var se sqlite3.Error
	if errors.As(err, &se) && (se.Code == sqlite3.ErrBusy || se.Code == sqlite3.ErrLocked) {
		return fmt.Errorf("%w: %w", model.ErrSessionBusy, err)
	}
	return err
}

// QueueSize returns the number of items in the queue for a session.
func (ti *ToolIndex) QueueSize(ctx context.Context, sessionID string) (int, error) {
	var count int
	err := ti.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tool_queue WHERE session_id = ?`, sessionID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("QueueSize: %w", err)
	}
	return count, nil
}
