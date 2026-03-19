// Review session and work item CRUD for the human-in-the-loop review system.
package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ReviewSession represents a review session for a branch.
type ReviewSession struct {
	ID        string
	Branch    string
	Status    string // "active", "completed", "abandoned"
	CreatedAt string
	UpdatedAt string
}

// ReviewWorkItem represents a single reviewable work item within a session.
type ReviewWorkItem struct {
	ID         int64
	SessionID  string
	StepType   string // "prune" or "distill"
	ClusterKey string
	FactsJSON  string
	Response   *string // nil until answered
	Priority   float64
	Depth      int // RAPTOR depth level (0 = initial)
	CreatedAt  string
}

// GetReviewWatermark returns the last-reviewed commit hash for the given branch,
// or "" if no watermark has been set.
func (idx *Index) GetReviewWatermark(branch string) (string, error) {
	var hash string
	err := idx.db.QueryRow(
		`SELECT commit_hash FROM review_watermarks WHERE branch = ?`, branch,
	).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetReviewWatermark: %w", err)
	}
	return hash, nil
}

// SetReviewWatermark upserts the last-reviewed commit hash for a branch.
func (idx *Index) SetReviewWatermark(branch, hash string) error {
	_, err := idx.db.Exec(
		`INSERT OR REPLACE INTO review_watermarks(branch, commit_hash) VALUES (?, ?)`,
		branch, hash,
	)
	if err != nil {
		return fmt.Errorf("SetReviewWatermark: %w", err)
	}
	return nil
}

// CreateReviewSession creates a new review session for the given branch.
// Any existing active session on the same branch is abandoned first.
func (idx *Index) CreateReviewSession(branch string) (*ReviewSession, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Abandon any active session on this branch.
	_, err := idx.db.Exec(
		`UPDATE review_sessions SET status = 'abandoned', updated_at = ? WHERE branch = ? AND status = 'active'`,
		now, branch,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateReviewSession abandon: %w", err)
	}

	s := &ReviewSession{
		ID:        uuid.New().String(),
		Branch:    branch,
		Status:    "active",
		CreatedAt: now,
		UpdatedAt: now,
	}

	_, err = idx.db.Exec(
		`INSERT INTO review_sessions(id, branch, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		s.ID, s.Branch, s.Status, s.CreatedAt, s.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateReviewSession insert: %w", err)
	}
	return s, nil
}

// GetReviewSession returns the session with the given ID, or nil if not found.
func (idx *Index) GetReviewSession(id string) (*ReviewSession, error) {
	var s ReviewSession
	err := idx.db.QueryRow(
		`SELECT id, branch, status, created_at, updated_at FROM review_sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.Branch, &s.Status, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetReviewSession: %w", err)
	}
	return &s, nil
}

// CompleteReviewSession marks the session as completed.
func (idx *Index) CompleteReviewSession(id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := idx.db.Exec(
		`UPDATE review_sessions SET status = 'completed', updated_at = ? WHERE id = ?`,
		now, id,
	)
	if err != nil {
		return fmt.Errorf("CompleteReviewSession: %w", err)
	}
	return nil
}

// InsertWorkItem inserts a new work item into the review_work_items table.
// The created_at field is set automatically.
func (idx *Index) InsertWorkItem(item ReviewWorkItem) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := idx.db.Exec(
		`INSERT INTO review_work_items(session_id, step_type, cluster_key, facts_json, response, priority, depth, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		item.SessionID, item.StepType, item.ClusterKey, item.FactsJSON, item.Response, item.Priority, item.Depth, now,
	)
	if err != nil {
		return fmt.Errorf("InsertWorkItem: %w", err)
	}
	return nil
}

// NextWorkItem returns the highest-priority unanswered work item for the given
// session, or nil if all items have been answered.
func (idx *Index) NextWorkItem(sessionID string) (*ReviewWorkItem, error) {
	var item ReviewWorkItem
	err := idx.db.QueryRow(
		`SELECT id, session_id, step_type, cluster_key, facts_json, response, priority, depth, created_at
		 FROM review_work_items
		 WHERE session_id = ? AND response IS NULL
		 ORDER BY priority DESC
		 LIMIT 1`, sessionID,
	).Scan(&item.ID, &item.SessionID, &item.StepType, &item.ClusterKey,
		&item.FactsJSON, &item.Response, &item.Priority, &item.Depth, &item.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("NextWorkItem: %w", err)
	}
	return &item, nil
}

// SetWorkItemResponse records the human response for a work item.
func (idx *Index) SetWorkItemResponse(id int64, response string) error {
	_, err := idx.db.Exec(
		`UPDATE review_work_items SET response = ? WHERE id = ?`,
		response, id,
	)
	if err != nil {
		return fmt.Errorf("SetWorkItemResponse: %w", err)
	}
	return nil
}

// GCReviewSessions deletes all but the most recent `keep` sessions for a branch.
// Work items are cascaded via the foreign key constraint.
func (idx *Index) GCReviewSessions(branch string, keep int) error {
	// Enable foreign keys for cascade deletes.
	if _, err := idx.db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("GCReviewSessions pragma: %w", err)
	}
	_, err := idx.db.Exec(
		`DELETE FROM review_sessions
		 WHERE branch = ? AND id NOT IN (
		     SELECT id FROM review_sessions
		     WHERE branch = ?
		     ORDER BY rowid DESC
		     LIMIT ?
		 )`,
		branch, branch, keep,
	)
	if err != nil {
		return fmt.Errorf("GCReviewSessions: %w", err)
	}
	return nil
}

// WorkItemStats returns the count of completed and remaining work items for a session.
func (idx *Index) WorkItemStats(sessionID string) (completed, remaining int, err error) {
	err = idx.db.QueryRow(
		`SELECT COUNT(*) FROM review_work_items WHERE session_id = ? AND response IS NOT NULL`,
		sessionID,
	).Scan(&completed)
	if err != nil {
		return 0, 0, fmt.Errorf("WorkItemStats completed: %w", err)
	}
	err = idx.db.QueryRow(
		`SELECT COUNT(*) FROM review_work_items WHERE session_id = ? AND response IS NULL`,
		sessionID,
	).Scan(&remaining)
	if err != nil {
		return 0, 0, fmt.Errorf("WorkItemStats remaining: %w", err)
	}
	return completed, remaining, nil
}
