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
func (idx *Index) GetPipelineWatermark(ctx context.Context, tool, branch string) (string, error) {
	var hash string
	err := conn(ctx, idx.db).QueryRowContext(ctx,
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
func (idx *Index) SetPipelineWatermark(ctx context.Context, tool, branch, hash string) error {
	_, err := conn(ctx, idx.db).ExecContext(ctx,
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
func (idx *Index) CreatePipelineSession(ctx context.Context, tool, branch string) (*PipelineSession, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Abandon any active session for this tool+branch.
	_, err := conn(ctx, idx.db).ExecContext(ctx,
		`UPDATE pipeline_sessions SET status = 'abandoned', updated_at = ? WHERE tool = ? AND branch = ? AND status = 'active'`,
		now, tool, branch,
	)
	if err != nil {
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

	_, err = conn(ctx, idx.db).ExecContext(ctx,
		`INSERT INTO pipeline_sessions(id, tool, branch, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		s.ID, s.Tool, s.Branch, s.Status, s.CreatedAt, s.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("CreatePipelineSession insert: %w", err)
	}
	return s, nil
}

// GetPipelineSession returns the session with the given ID, or nil if not found.
func (idx *Index) GetPipelineSession(ctx context.Context, id string) (*PipelineSession, error) {
	var s PipelineSession
	err := conn(ctx, idx.db).QueryRowContext(ctx,
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
func (idx *Index) CompletePipelineSession(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := conn(ctx, idx.db).ExecContext(ctx,
		`UPDATE pipeline_sessions SET status = 'completed', updated_at = ? WHERE id = ?`,
		now, id,
	)
	if err != nil {
		return fmt.Errorf("CompletePipelineSession: %w", err)
	}
	return nil
}

// InsertPipelineWorkItem inserts a new work item into the pipeline_work_items table.
func (idx *Index) InsertPipelineWorkItem(ctx context.Context, item PipelineWorkItem) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := conn(ctx, idx.db).ExecContext(ctx,
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
func (idx *Index) NextPipelineWorkItem(ctx context.Context, sessionID string) (*PipelineWorkItem, error) {
	var item PipelineWorkItem
	err := conn(ctx, idx.db).QueryRowContext(ctx,
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
func (idx *Index) SetPipelineWorkItemResponse(ctx context.Context, id int64, response string) error {
	_, err := conn(ctx, idx.db).ExecContext(ctx,
		`UPDATE pipeline_work_items SET response = ? WHERE id = ?`,
		response, id,
	)
	if err != nil {
		return fmt.Errorf("SetPipelineWorkItemResponse: %w", err)
	}
	return nil
}

// GCPipelineSessions deletes all but the most recent `keep` sessions for a tool+branch.
// Work items are cascaded via the foreign key constraint.
func (idx *Index) GCPipelineSessions(ctx context.Context, tool, branch string, keep int) error {
	_, err := conn(ctx, idx.db).ExecContext(ctx,
		`DELETE FROM pipeline_sessions
		 WHERE tool = ? AND branch = ? AND id NOT IN (
		     SELECT id FROM pipeline_sessions
		     WHERE tool = ? AND branch = ?
		     ORDER BY rowid DESC
		     LIMIT ?
		 )`,
		tool, branch, tool, branch, keep,
	)
	if err != nil {
		return fmt.Errorf("GCPipelineSessions: %w", err)
	}
	return nil
}

// PipelineWorkItemStats returns the count of completed and remaining work items for a session.
func (idx *Index) PipelineWorkItemStats(ctx context.Context, sessionID string) (completed, remaining int, err error) {
	err = conn(ctx, idx.db).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipeline_work_items WHERE session_id = ? AND response IS NOT NULL`,
		sessionID,
	).Scan(&completed)
	if err != nil {
		return 0, 0, fmt.Errorf("PipelineWorkItemStats completed: %w", err)
	}
	err = conn(ctx, idx.db).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipeline_work_items WHERE session_id = ? AND response IS NULL`,
		sessionID,
	).Scan(&remaining)
	if err != nil {
		return 0, 0, fmt.Errorf("PipelineWorkItemStats remaining: %w", err)
	}
	return completed, remaining, nil
}
