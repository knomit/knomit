// Pipeline session and work item CRUD for LLM-driven pipelines (review, hypothesize, etc.).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PipelineSession represents an active pipeline session for a tool on a branch.
type PipelineSession struct {
	ID        string
	Tool      string
	Branch    string
	Status    string // "active", "completed", "abandoned"
	CreatedAt string
	UpdatedAt string
}

// PipelineWorkItem represents a single work item within a pipeline session.
type PipelineWorkItem struct {
	ID         int64
	SessionID  string
	StepType   string // "prune", "distill", "reflect", "hypothesize", etc.
	ClusterKey string
	FactsJSON  string
	Response   *string // nil until answered
	Priority   float64
	Depth      int // RAPTOR depth level (0 = initial)
	CreatedAt  string
}

// GetPipelineWatermark returns the last-processed commit hash for the given tool+branch,
// or "" if no watermark has been set.
func (idx *store) GetPipelineWatermark(ctx context.Context, tool, branch string) (string, error) {
	var hash string
	err := conn(ctx, idx.rh.db).QueryRowContext(ctx,
		`SELECT commit_hash FROM pipeline_watermarks WHERE tool = ? AND branch = ?`, tool, branch,
	).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetPipelineWatermark: %w", err)
	}
	return hash, nil
}

// SetPipelineWatermark upserts the last-processed commit hash for a tool+branch.
func (idx *store) SetPipelineWatermark(ctx context.Context, tool, branch, hash string) error {
	_, err := conn(ctx, idx.rh.db).ExecContext(ctx,
		`INSERT OR REPLACE INTO pipeline_watermarks(tool, branch, commit_hash) VALUES (?, ?, ?)`,
		tool, branch, hash,
	)
	if err != nil {
		return fmt.Errorf("SetPipelineWatermark: %w", err)
	}
	return nil
}

// CreatePipelineSession creates a new session for the given tool+branch.
// Any existing active session for the same tool+branch is abandoned first.
func (idx *store) CreatePipelineSession(ctx context.Context, tool, branch string) (*PipelineSession, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	ctx, tx, ownTx, err := beginTxIfNeeded(ctx, idx.rh.db)
	if err != nil {
		return nil, fmt.Errorf("CreatePipelineSession: begin tx: %w", err)
	}
	if ownTx {
		defer tx.Rollback()
	}
	db := conn(ctx, idx.rh.db)

	// Abandon any active session for this tool+branch.
	if _, err := db.ExecContext(ctx,
		`UPDATE pipeline_sessions SET status = 'abandoned', updated_at = ? WHERE tool = ? AND branch = ? AND status = 'active'`,
		now, tool, branch,
	); err != nil {
		return nil, fmt.Errorf("CreatePipelineSession abandon: %w", err)
	}

	s := &PipelineSession{
		ID:        uuid.New().String(),
		Tool:      tool,
		Branch:    branch,
		Status:    "active",
		CreatedAt: now,
		UpdatedAt: now,
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO pipeline_sessions(id, tool, branch, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		s.ID, s.Tool, s.Branch, s.Status, s.CreatedAt, s.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("CreatePipelineSession insert: %w", err)
	}

	if ownTx {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// GetPipelineSession returns the session with the given ID, or nil if not found.
func (idx *store) GetPipelineSession(ctx context.Context, id string) (*PipelineSession, error) {
	var s PipelineSession
	err := conn(ctx, idx.rh.db).QueryRowContext(ctx,
		`SELECT id, tool, branch, status, created_at, updated_at FROM pipeline_sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.Tool, &s.Branch, &s.Status, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetPipelineSession: %w", err)
	}
	return &s, nil
}

// CompletePipelineSession marks the session as completed.
func (idx *store) CompletePipelineSession(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := conn(ctx, idx.rh.db).ExecContext(ctx,
		`UPDATE pipeline_sessions SET status = 'completed', updated_at = ? WHERE id = ?`,
		now, id,
	)
	if err != nil {
		return fmt.Errorf("CompletePipelineSession: %w", err)
	}
	return nil
}

// InsertPipelineWorkItem inserts a new work item into the pipeline_work_items table.
func (idx *store) InsertPipelineWorkItem(ctx context.Context, item PipelineWorkItem) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := conn(ctx, idx.rh.db).ExecContext(ctx,
		`INSERT INTO pipeline_work_items(session_id, step_type, cluster_key, facts_json, response, priority, depth, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		item.SessionID, item.StepType, item.ClusterKey, item.FactsJSON, item.Response, item.Priority, item.Depth, now,
	)
	if err != nil {
		return fmt.Errorf("InsertPipelineWorkItem: %w", err)
	}
	return nil
}

// NextPipelineWorkItem returns the highest-priority unanswered work item for the given
// session, or nil if all items have been answered.
func (idx *store) NextPipelineWorkItem(ctx context.Context, sessionID string) (*PipelineWorkItem, error) {
	var item PipelineWorkItem
	err := conn(ctx, idx.rh.db).QueryRowContext(ctx,
		`SELECT id, session_id, step_type, cluster_key, facts_json, response, priority, depth, created_at
		 FROM pipeline_work_items
		 WHERE session_id = ? AND response IS NULL
		 ORDER BY priority DESC
		 LIMIT 1`, sessionID,
	).Scan(&item.ID, &item.SessionID, &item.StepType, &item.ClusterKey,
		&item.FactsJSON, &item.Response, &item.Priority, &item.Depth, &item.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("NextPipelineWorkItem: %w", err)
	}
	return &item, nil
}

// SetPipelineWorkItemResponse records the response for a work item.
func (idx *store) SetPipelineWorkItemResponse(ctx context.Context, id int64, response string) error {
	_, err := conn(ctx, idx.rh.db).ExecContext(ctx,
		`UPDATE pipeline_work_items SET response = ? WHERE id = ?`,
		response, id,
	)
	if err != nil {
		return fmt.Errorf("SetPipelineWorkItemResponse: %w", err)
	}
	return nil
}

// PipelineWorkItemStats returns the count of completed and remaining work items for a session.
func (idx *store) PipelineWorkItemStats(ctx context.Context, sessionID string) (completed, remaining int, err error) {
	err = conn(ctx, idx.rh.db).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipeline_work_items WHERE session_id = ? AND response IS NOT NULL`,
		sessionID,
	).Scan(&completed)
	if err != nil {
		return 0, 0, fmt.Errorf("PipelineWorkItemStats completed: %w", err)
	}
	err = conn(ctx, idx.rh.db).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipeline_work_items WHERE session_id = ? AND response IS NULL`,
		sessionID,
	).Scan(&remaining)
	if err != nil {
		return 0, 0, fmt.Errorf("PipelineWorkItemStats remaining: %w", err)
	}
	return completed, remaining, nil
}
