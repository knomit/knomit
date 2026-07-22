package store

import (
	"context"
	"fmt"
)

// graphInsertEdgeReturningID inserts an edge into the edges table
// and returns its rowid. Uses INSERT (no OR IGNORE) because (source_id,
// target_id, type) has no uniqueness constraint —
// multi-edges are intentional for time-aware DERIVED_FROM.
// Callers are responsible for deduplication when idempotency is required;
// this function always produces a new row.
func (si *searchIndex) graphInsertEdgeReturningID(ctx context.Context, sourceID, targetID int64, edgeType string) (int64, error) {
	res, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`INSERT INTO edges (source_id, target_id, type) VALUES (?, ?, ?)`,
		sourceID, targetID, edgeType,
	)
	if err != nil {
		return 0, fmt.Errorf("graphInsertEdgeReturningID: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("graphInsertEdgeReturningID: lastID: %w", err)
	}
	return id, nil
}

// graphSetEdgeProps writes text-typed properties on an edge via the EAV
// table edge_props_text.
func (si *searchIndex) graphSetEdgeProps(ctx context.Context, edgeID int64, props map[string]string) error {
	db := conn(ctx, si.rh.db)
	for key, value := range props {
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO property_keys(key) VALUES (?)`, key); err != nil {
			return fmt.Errorf("graphSetEdgeProps: ensure key %s: %w", key, err)
		}
		var keyID int64
		if err := db.QueryRowContext(ctx, `SELECT id FROM property_keys WHERE key = ?`, key).Scan(&keyID); err != nil {
			return fmt.Errorf("graphSetEdgeProps: get key_id for %s: %w", key, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT OR REPLACE INTO edge_props_text(edge_id, key_id, value) VALUES (?, ?, ?)`,
			edgeID, keyID, value,
		); err != nil {
			return fmt.Errorf("graphSetEdgeProps: set %s: %w", key, err)
		}
	}
	return nil
}
