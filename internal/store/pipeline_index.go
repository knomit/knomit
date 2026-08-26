package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PipelineSession represents an active pipeline session for a tool on a branch.
//
// Status tracks lifecycle (active|completed|abandoned). Phase tracks
// workflow position inside an active session: "work" (prune/distill items
// being processed), "reflect" (single reflect item enqueued/being served),
// or "done" (all items including reflect answered, ready for completion).
// Phase is what makes the reviewer stateless across MCP calls — see
// AdvancePipelineSessionPhase for the CAS guarantee.
type PipelineSession struct {
	ID     string
	Tool   string
	Branch string
	Status string // "active", "completed", "abandoned"
	Phase  string // "work", "reflect", "done"
	Scoped bool   // true when session was started with a scope filter active
	Stats  PipelineSessionStats
	// Health carries corpus-health descriptor lines recorded while planning the
	// session. IN MEMORY ONLY — it is written and read inside the same
	// StartSession call, so it is deliberately not a column: nothing needs it
	// after the turn that produced it.
	Health []string
	// Abandoned is the id of the active session this one DISPLACED at creation,
	// if there was one. IN MEMORY ONLY, like Health, and set by
	// CreatePipelineSession alone — it describes one moment, not a stored
	// property of the row.
	//
	// Starting a session abandons any active session for the same tool+branch.
	// That is deliberate (one session per tool per branch) but it was silent on
	// both sides: the winner did not know it took the slot, and the loser found
	// out only when its next answer was rejected as "not active" — after it had
	// composed one (knomit#113 / #121 residue).
	Abandoned string
	CreatedAt string
	UpdatedAt string
}

// PipelineSessionStats are the running totals of what a session's applied work
// items actually changed in the corpus.
//
// They live on the session row rather than on the engine because the engine is
// per-call stateless: the MCP handler constructs a fresh Reviewer for every
// continue call, so counters held on that struct would be discarded between
// items (see invariants/synthesize/per-call-objects-no-session-state).
//
// This is deliberately a store-owned struct rather than synthesize.ReviewStats:
// store cannot import synthesize (synthesize imports store), and the wire-facing
// ReviewStats belongs to the tool result, not to the DB row. Callers convert at
// the boundary — the two shapes are the same four counters by design.
type PipelineSessionStats struct {
	Pruned      int
	Merged      int
	Updated     int
	Synthesized int
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

// pipelineIndex spans two databases. Watermark methods (durable progress: how
// far review/hypothesize has processed git history) use the MAIN db on rh.
// Session and work-item methods (ephemeral, in-flight work-stealing state) use
// the separate session DB (sessionDB). Session methods use sessionDB DIRECTLY
// and never conn(ctx, …) / beginTxIfNeeded(ctx, …): a ctx-carried *sql.Tx is
// bound to the MAIN db, so routing session SQL through it would hit the wrong
// database (where these tables no longer exist).
type pipelineIndex struct {
	rh        *repoHandler
	sessionDB *sql.DB
}

// Compile-time assertion: pipelineIndex must implement PipelineIndex.
var _ PipelineIndex = (*pipelineIndex)(nil)

// GetPipelineWatermark returns the last-processed commit hash for the given tool+branch,
// or "" if no watermark has been set.
func (pi *pipelineIndex) GetPipelineWatermark(ctx context.Context, tool, branch string) (string, error) {
	var hash string
	err := conn(ctx, pi.rh.db).QueryRowContext(ctx,
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
func (pi *pipelineIndex) SetPipelineWatermark(ctx context.Context, tool, branch, hash string) error {
	_, err := conn(ctx, pi.rh.db).ExecContext(ctx,
		`INSERT OR REPLACE INTO pipeline_watermarks(tool, branch, commit_hash) VALUES (?, ?, ?)`,
		tool, branch, hash,
	)
	if err != nil {
		return fmt.Errorf("SetPipelineWatermark: %w", err)
	}
	return nil
}

// CreatePipelineSession creates a new session for the given tool+branch. Any
// existing active session for the same tool+branch is abandoned first.
func (pi *pipelineIndex) CreatePipelineSession(ctx context.Context, tool, branch string) (*PipelineSession, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Own transaction on the session DB. We deliberately do NOT consult any
	// ctx-carried tx (it would belong to the main db).
	tx, err := pi.sessionDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("CreatePipelineSession: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Abandon any active session for this tool+branch.
	//
	// WHICH ONE, captured before the update (knomit#113 / #121 residue).
	// Starting a session silently DISPLACES an in-flight one, and the
	// invisibility is two-sided: the new caller is not told it took the slot,
	// and the displaced caller is not told either — it finds out when its next
	// continue call is rejected, after spending a turn composing an answer to
	// an item that no longer exists. Nothing here can notify the loser, so the
	// least this can do is let the winner's result say what it displaced.
	var abandoned string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM pipeline_sessions WHERE tool = ? AND branch = ? AND status = 'active' LIMIT 1`,
		tool, branch,
	).Scan(&abandoned); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("CreatePipelineSession find active: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
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
		Phase:     "work",
		Abandoned: abandoned,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO pipeline_sessions(id, tool, branch, status, phase, created_at, updated_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Tool, s.Branch, s.Status, s.Phase, s.CreatedAt, s.UpdatedAt, now,
	); err != nil {
		return nil, fmt.Errorf("CreatePipelineSession insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s, nil
}

// GetPipelineSession returns the session with the given ID, or nil if not found.
func (pi *pipelineIndex) GetPipelineSession(ctx context.Context, id string) (*PipelineSession, error) {
	var s PipelineSession
	var scoped int
	err := pi.sessionDB.QueryRowContext(ctx,
		`SELECT id, tool, branch, status, phase, scoped,
		        stat_pruned, stat_merged, stat_updated, stat_synthesized,
		        created_at, updated_at
		 FROM pipeline_sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.Tool, &s.Branch, &s.Status, &s.Phase, &scoped,
		&s.Stats.Pruned, &s.Stats.Merged, &s.Stats.Updated, &s.Stats.Synthesized,
		&s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetPipelineSession: %w", err)
	}
	s.Scoped = scoped != 0
	return &s, nil
}

// MarkPipelineSessionScoped marks a session as having been started with a
// scope filter. Called by synthesize.Pipeline.StartSession when a non-empty
// ScopeFilter is active, so that Pipeline.completeSession can suppress
// watermark advancement (advancing would hide out-of-scope facts from future
// unscoped sessions).
//
// The flag is tool-agnostic: both knomit_review and knomit_hypothesize run on
// the same Pipeline engine and both accept a scope filter, so both set and
// honour it. Nothing here is hypothesize-specific.
func (pi *pipelineIndex) MarkPipelineSessionScoped(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := pi.sessionDB.ExecContext(ctx,
		`UPDATE pipeline_sessions SET scoped = 1, updated_at = ? WHERE id = ?`,
		now, id,
	)
	if err != nil {
		return fmt.Errorf("MarkPipelineSessionScoped: %w", err)
	}
	return nil
}

// AdvancePipelineSessionPhase atomically transitions a session from `from`
// to `to`. The UPDATE matches on the current phase so concurrent callers
// can't both succeed: exactly one wins, the rest see the row already
// advanced and get (false, nil) — a benign no-op, not an error. This is
// what guarantees the reflect step is enqueued at most once per session,
// replacing the in-memory `reflectChecked` map that was lost between MCP
// calls.
func (pi *pipelineIndex) AdvancePipelineSessionPhase(ctx context.Context, id, from, to string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := pi.sessionDB.ExecContext(ctx,
		`UPDATE pipeline_sessions SET phase = ?, updated_at = ? WHERE id = ? AND phase = ?`,
		to, now, id, from,
	)
	if err != nil {
		return false, fmt.Errorf("AdvancePipelineSessionPhase: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("AdvancePipelineSessionPhase rows: %w", err)
	}
	return n == 1, nil
}

// CompletePipelineSession marks the session as completed.
func (pi *pipelineIndex) CompletePipelineSession(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := pi.sessionDB.ExecContext(ctx,
		`UPDATE pipeline_sessions SET status = 'completed', updated_at = ? WHERE id = ?`,
		now, id,
	)
	if err != nil {
		return fmt.Errorf("CompletePipelineSession: %w", err)
	}
	return nil
}

// InsertPipelineWorkItem inserts a new work item into the pipeline_work_items table.
func (pi *pipelineIndex) InsertPipelineWorkItem(ctx context.Context, item PipelineWorkItem) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := pi.sessionDB.ExecContext(ctx,
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
func (pi *pipelineIndex) NextPipelineWorkItem(ctx context.Context, sessionID string) (*PipelineWorkItem, error) {
	// Heartbeat: serving work keeps the session alive against the idle reaper.
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pi.sessionDB.ExecContext(ctx,
		`UPDATE pipeline_sessions SET last_used_at = ? WHERE id = ?`, now, sessionID,
	); err != nil {
		return nil, fmt.Errorf("NextPipelineWorkItem touch: %w", err)
	}

	// `id ASC` is the tiebreak, not decoration: priority alone does not totally
	// order the queue — every top-level distill item shares priority 0.0, as do
	// same-size prune clusters — and SQLite is free to return ties in any order.
	// Without the tiebreak, two peeks of the same queue state can hand back
	// different items, so a client answering "the current item" may be answering
	// a different row than the one it was shown. Ordering by the insertion-ordered
	// rowid makes the peek a deterministic function of queue state.
	var item PipelineWorkItem
	err := pi.sessionDB.QueryRowContext(ctx,
		`SELECT id, session_id, step_type, cluster_key, facts_json, response, priority, depth, created_at
		 FROM pipeline_work_items
		 WHERE session_id = ? AND response IS NULL
		 ORDER BY priority DESC, id ASC
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

// AnswerPipelineWorkItem atomically claims and answers a work item. The
// UPDATE matches on response IS NULL, so concurrent (or retried) callers
// can't both succeed: exactly one wins and gets (true, nil), the rest see
// the row already answered and get (false, nil) — a benign no-op, not an
// error, mirroring AdvancePipelineSessionPhase.
//
// Winning the CAS is the caller's licence to apply the response's mutations.
// That is what makes the pipeline idempotent on retry: a resubmitted response
// loses the claim and its decisions are never applied a second time, so a
// duplicate submission can no longer mint a second copy of the same
// synthesized facts.
func (pi *pipelineIndex) AnswerPipelineWorkItem(ctx context.Context, id int64, response string) (bool, error) {
	res, err := pi.sessionDB.ExecContext(ctx,
		`UPDATE pipeline_work_items SET response = ? WHERE id = ? AND response IS NULL`,
		response, id,
	)
	if err != nil {
		return false, fmt.Errorf("AnswerPipelineWorkItem: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("AnswerPipelineWorkItem rows: %w", err)
	}
	return n == 1, nil
}

// AddPipelineSessionStats accumulates one applied work item's stats onto the
// session row. The counts are added in SQL rather than read-modify-written, so
// concurrent appliers of different items each contribute exactly their own
// delta.
//
// Running totals live on the row, not on the engine: the MCP handler builds a
// fresh Reviewer per continue call, so there is no in-memory home for them (see
// invariants/synthesize/per-call-objects-no-session-state).
func (pi *pipelineIndex) AddPipelineSessionStats(ctx context.Context, id string, s PipelineSessionStats) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := pi.sessionDB.ExecContext(ctx,
		`UPDATE pipeline_sessions
		 SET stat_pruned      = stat_pruned      + ?,
		     stat_merged      = stat_merged      + ?,
		     stat_updated     = stat_updated     + ?,
		     stat_synthesized = stat_synthesized + ?,
		     updated_at       = ?
		 WHERE id = ?`,
		s.Pruned, s.Merged, s.Updated, s.Synthesized, now, id,
	)
	if err != nil {
		return fmt.Errorf("AddPipelineSessionStats: %w", err)
	}
	return nil
}

// PipelineWorkItemStats returns the count of completed and remaining work items for a session.
func (pi *pipelineIndex) PipelineWorkItemStats(ctx context.Context, sessionID string) (completed, remaining int, err error) {
	err = pi.sessionDB.QueryRowContext(ctx,
		`SELECT
			COALESCE(SUM(CASE WHEN response IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN response IS NULL     THEN 1 ELSE 0 END), 0)
		 FROM pipeline_work_items WHERE session_id = ?`,
		sessionID,
	).Scan(&completed, &remaining)
	if err != nil {
		return 0, 0, fmt.Errorf("PipelineWorkItemStats: %w", err)
	}
	return completed, remaining, nil
}

// PendingPipelineWorkItems returns this session's unanswered items in queue
// order — the same ordering NextPipelineWorkItem serves, so a caller iterating
// them sees the queue as the agent will.
func (pi *pipelineIndex) PendingPipelineWorkItems(ctx context.Context, sessionID string) ([]PipelineWorkItem, error) {
	rows, err := pi.sessionDB.QueryContext(ctx,
		`SELECT id, session_id, step_type, cluster_key, facts_json, response, priority, depth, created_at
		 FROM pipeline_work_items
		 WHERE session_id = ? AND response IS NULL
		 ORDER BY priority DESC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("PendingPipelineWorkItems: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PipelineWorkItem
	for rows.Next() {
		var item PipelineWorkItem
		if err := rows.Scan(&item.ID, &item.SessionID, &item.StepType, &item.ClusterKey,
			&item.FactsJSON, &item.Response, &item.Priority, &item.Depth, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("PendingPipelineWorkItems scan: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PendingPipelineWorkItems rows: %w", err)
	}
	return out, nil
}

// UpdatePipelineWorkItemFacts rewrites an unanswered item's payload.
//
// The `response IS NULL` guard is the claim protocol's CAS, used here in the
// other direction: the claim stops two callers applying one answer, and this
// stops a payload edit landing on an item whose answer is already being
// applied. updated=false means the item was claimed first — a benign no-op,
// exactly as a lost claim is.
func (pi *pipelineIndex) UpdatePipelineWorkItemFacts(ctx context.Context, id int64, factsJSON string) (bool, error) {
	res, err := pi.sessionDB.ExecContext(ctx,
		`UPDATE pipeline_work_items SET facts_json = ? WHERE id = ? AND response IS NULL`,
		factsJSON, id)
	if err != nil {
		return false, fmt.Errorf("UpdatePipelineWorkItemFacts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("UpdatePipelineWorkItemFacts rows: %w", err)
	}
	return n > 0, nil
}

// DeletePipelineWorkItem removes an unanswered item. Same CAS guard as the
// payload rewrite above, for the same reason.
func (pi *pipelineIndex) DeletePipelineWorkItem(ctx context.Context, id int64) (bool, error) {
	res, err := pi.sessionDB.ExecContext(ctx,
		`DELETE FROM pipeline_work_items WHERE id = ? AND response IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("DeletePipelineWorkItem: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("DeletePipelineWorkItem rows: %w", err)
	}
	return n > 0, nil
}
