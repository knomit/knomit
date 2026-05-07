package store

import (
	"context"
	"fmt"
	"time"
)

// MethodologyReinforcement is one row in methodology_reinforcements: a
// transition during a review session that the agent identified as
// re-confirming an existing methodology fact. The denormalised
// (methodology, transition) shape lets future tooling answer "which
// transitions reinforced X?" and "how recently was this methodology
// reinforced?" without unpacking JSON.
type MethodologyReinforcement struct {
	ID              int64
	Branch          string
	MethodologyPath string
	TransitionPath  string
	SessionID       string
	Rationale       string
	ReinforcedAt    string
}

// MethodologyIndex is the storage layer for methodology-related side
// signals. Today it owns reinforcements; future additions (retirement
// records, reinforcement decay, etc.) belong here.
type MethodologyIndex interface {
	InsertReinforcement(ctx context.Context, r MethodologyReinforcement) error
	ListReinforcementsByPath(ctx context.Context, branch, methodologyPath string) ([]MethodologyReinforcement, error)
	ListReinforcementsBySession(ctx context.Context, sessionID string) ([]MethodologyReinforcement, error)
}

type methodologyIndex struct {
	rh *repoHandler
}

var _ MethodologyIndex = (*methodologyIndex)(nil)

// InsertReinforcement records one (methodology, transition) reinforcement.
// reinforced_at is server-stamped; callers should not pre-populate it.
func (mi *methodologyIndex) InsertReinforcement(ctx context.Context, r MethodologyReinforcement) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := conn(ctx, mi.rh.db).ExecContext(ctx,
		`INSERT INTO methodology_reinforcements
		   (branch, methodology_path, transition_path, session_id, rationale, reinforced_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.Branch, r.MethodologyPath, r.TransitionPath, r.SessionID, r.Rationale, now,
	)
	if err != nil {
		return fmt.Errorf("InsertReinforcement: %w", err)
	}
	return nil
}

// ListReinforcementsByPath returns rows for a methodology, scoped by branch.
// Order is insertion order (id ASC) so callers can read off "first
// reinforced" / "most recently reinforced" by position.
func (mi *methodologyIndex) ListReinforcementsByPath(ctx context.Context, branch, methodologyPath string) ([]MethodologyReinforcement, error) {
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx,
		`SELECT id, branch, methodology_path, transition_path, session_id, rationale, reinforced_at
		 FROM methodology_reinforcements
		 WHERE branch = ? AND methodology_path = ?
		 ORDER BY id ASC`,
		branch, methodologyPath,
	)
	if err != nil {
		return nil, fmt.Errorf("ListReinforcementsByPath: %w", err)
	}
	defer rows.Close()
	return scanReinforcements(rows)
}

// ListReinforcementsBySession returns every reinforcement recorded during
// the given review session, in insertion order.
func (mi *methodologyIndex) ListReinforcementsBySession(ctx context.Context, sessionID string) ([]MethodologyReinforcement, error) {
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx,
		`SELECT id, branch, methodology_path, transition_path, session_id, rationale, reinforced_at
		 FROM methodology_reinforcements
		 WHERE session_id = ?
		 ORDER BY id ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("ListReinforcementsBySession: %w", err)
	}
	defer rows.Close()
	return scanReinforcements(rows)
}

type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanReinforcements(rows rowScanner) ([]MethodologyReinforcement, error) {
	var out []MethodologyReinforcement
	for rows.Next() {
		var r MethodologyReinforcement
		if err := rows.Scan(&r.ID, &r.Branch, &r.MethodologyPath, &r.TransitionPath,
			&r.SessionID, &r.Rationale, &r.ReinforcedAt); err != nil {
			return nil, fmt.Errorf("scanReinforcements: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scanReinforcements: rows: %w", err)
	}
	return out, nil
}
