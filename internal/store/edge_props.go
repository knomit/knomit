package store

import (
	"context"
	"fmt"
)

// graphInsertEdgeReturningID inserts an edge into the edges table and returns
// its rowid. Uses INSERT (no OR IGNORE) because it is for DERIVED_FROM, which
// migration 000015's ux_edges_merge_identity exempts: multi-edges are
// intentional there, one per ref-event, distinguished by their source_commit /
// target_commit properties. Dedup is the caller's job — see the property-aware
// guard in derived_from.go.
//
// Only sound for DERIVED_FROM. Every other type is covered by the unique index,
// so a duplicate would fail here rather than producing the new row this returns.
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
// table edge_props_text. Key ids are resolved in bulk (see graphPropKeyIDs)
// rather than a statement pair per key.
func (si *searchIndex) graphSetEdgeProps(ctx context.Context, edgeID int64, props map[string]string) error {
	db := conn(ctx, si.rh.db)
	keys := sortedKeys(props)
	keyIDs, err := graphPropKeyIDs(ctx, db, keys)
	if err != nil {
		return fmt.Errorf("graphSetEdgeProps: %w", err)
	}
	for _, key := range keys {
		if _, err := db.ExecContext(ctx,
			`INSERT OR REPLACE INTO edge_props_text(edge_id, key_id, value) VALUES (?, ?, ?)`,
			edgeID, keyIDs[key], props[key],
		); err != nil {
			return fmt.Errorf("graphSetEdgeProps: set %s: %w", key, err)
		}
	}
	return nil
}
