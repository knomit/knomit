package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PROTOTYPE (proto/factset-streaming): a streamed FactSet's cursor is ONE
// write: a tool_sessions row and its factset_sessions row (env = the frozen
// state: results with their versions, anchor, frozen tips), in one
// transaction. Nothing is written after it.

// CreateStreamFactSet writes a streamed FactSet's cursor and returns its id.
func (ti *ToolIndex) CreateStreamFactSet(ctx context.Context, tool, branch, binding, readSet, owner, state string) (string, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	id := uuid.New().String()
	tx, err := ti.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("CreateStreamFactSet: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tool_sessions(id, tool, branch, path_prefix, binding, read_set, owner, last_commit, status, created_at, updated_at, last_used_at)
		 VALUES (?, ?, ?, '', ?, ?, ?, '', 'active', ?, ?, ?)`,
		id, tool, branch, binding, readSet, owner, now, now, now); err != nil {
		return "", fmt.Errorf("CreateStreamFactSet: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO factset_sessions(session_id, env) VALUES (?, ?)`, id, state); err != nil {
		return "", fmt.Errorf("CreateStreamFactSet: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("CreateStreamFactSet: %w", err)
	}
	return id, nil
}

// GetStreamFactSet reads a streamed FactSet's frozen state; "" when there is
// no such cursor for owner.
func (ti *ToolIndex) GetStreamFactSet(ctx context.Context, id, owner string) (string, error) {
	var state string
	err := ti.db.QueryRowContext(ctx,
		`SELECT f.env FROM factset_sessions f JOIN tool_sessions t ON t.id = f.session_id
		  WHERE f.session_id = ? AND t.owner = ?`, id, owner).Scan(&state)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetStreamFactSet: %w", err)
	}
	return state, nil
}
