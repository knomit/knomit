package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Blind definitions, one per motif cluster (blueprint §3.2).
//
// Everything here keys on cluster_key, never on canonical_id. The
// representative spelling flips as usage shifts; the cluster it names does not,
// and a definition keyed to the representative would be orphaned by a change
// that meant nothing (designer rider 2026-08-21).

// DefinitionTarget is one cluster the definition pass should author for, with
// the name it will be shown.
//
// Name is the ONLY corpus content that reaches the definition prompt. Carriers
// are deliberately absent: a writer who never saw them cannot name the systems
// they are about, which is what makes the generic register achievable rather
// than merely requested.
type DefinitionTarget struct {
	ClusterKey string
	Name       string
	// Interim is the definition currently standing for this cluster, if any.
	// Non-empty means the cluster HAS a usable sentence and is queued because
	// its membership moved — not because it has nothing.
	Interim string
}

// ClustersNeedingDefinition returns the live clusters whose definition is
// missing or was authored over a different membership.
//
// Staleness is a COMPARISON, not a flag. Nothing has to remember to mark a
// cluster dirty, and nothing can forget: a judge merge, a spelling joining
// mechanically, and a member retiring all move membership, and all three should
// prompt a fresh sentence. A flag set by the merge path would have caught only
// the first.
func (mi *motifIndex) ClustersNeedingDefinition(ctx context.Context, branch string) ([]DefinitionTarget, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("ClustersNeedingDefinition: %w", err)
	}
	membership, err := mi.clusterMembership(ctx, branchID)
	if err != nil {
		return nil, err
	}
	clusters, err := mi.Clusters(ctx, branch)
	if err != nil {
		return nil, err
	}
	stored, err := mi.definitionRows(ctx, branchID)
	if err != nil {
		return nil, err
	}
	// Ordered by the Clusters ordering (most frequent first), so a bounded
	// pass spends its budget on the vocabulary the corpus actually leans on.
	var out []DefinitionTarget
	for _, c := range clusters {
		row, defined := stored[c.ClusterKey]
		if defined && row.members == membership[c.ClusterKey] {
			continue
		}
		out = append(out, DefinitionTarget{
			ClusterKey: c.ClusterKey,
			Name:       c.CanonicalID,
			Interim:    row.definition, // empty when never defined
		})
	}
	return out, nil
}

type definitionRow struct {
	definition string
	members    string
}

func (mi *motifIndex) definitionRows(ctx context.Context, branchID int64) (map[string]definitionRow, error) {
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx,
		`SELECT cluster_key, definition, members FROM motif_definitions WHERE branch_id = ?`,
		branchID)
	if err != nil {
		return nil, fmt.Errorf("definitionRows: %w", err)
	}
	defer rows.Close()
	out := map[string]definitionRow{}
	for rows.Next() {
		var key string
		var r definitionRow
		if err := rows.Scan(&key, &r.definition, &r.members); err != nil {
			return nil, fmt.Errorf("definitionRows: scan: %w", err)
		}
		out[key] = r
	}
	return out, rows.Err()
}

// PutDefinition stores a cluster's definition, stamped with the membership it
// was authored over.
func (mi *motifIndex) PutDefinition(ctx context.Context, branch, clusterKey, definition string) error {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("PutDefinition: %w", err)
	}
	membership, err := mi.clusterMembership(ctx, branchID)
	if err != nil {
		return err
	}
	if _, err := conn(ctx, mi.rh.db).ExecContext(ctx,
		`INSERT INTO motif_definitions(branch_id, cluster_key, definition, members, authored_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(branch_id, cluster_key) DO UPDATE SET
		     definition  = excluded.definition,
		     members     = excluded.members,
		     authored_at = excluded.authored_at`,
		branchID, clusterKey, definition, membership[clusterKey],
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("PutDefinition: %w", err)
	}
	return nil
}

// Definition returns a cluster's standing definition, if it has one.
//
// Returns a STALE definition rather than nothing (designer ruling): a judge
// merge asserts the phrasings name the same mechanism, so the survivor's
// sentence is approximately right for the union, and gapping the cluster is
// worse than a slightly wide sentence. ClustersNeedingDefinition is what
// queues it for refresh.
func (mi *motifIndex) Definition(ctx context.Context, branch, clusterKey string) (string, bool, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return "", false, fmt.Errorf("Definition: %w", err)
	}
	var def string
	err = conn(ctx, mi.rh.db).QueryRowContext(ctx,
		`SELECT definition FROM motif_definitions WHERE branch_id = ? AND cluster_key = ?`,
		branchID, clusterKey).Scan(&def)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("Definition: %w", err)
	}
	return def, true, nil
}
