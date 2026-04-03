package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// RefSummary is a lightweight fact reference returned by ExplainFact.
type RefSummary struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Deleted bool   `json:"deleted,omitempty"`
}

// ExplainResult holds the incoming and outgoing reference summary for a fact.
type ExplainResult struct {
	Incoming []RefSummary `json:"incoming"`
	Outgoing []RefSummary `json:"outgoing"`
}

// ExplainFact returns the incoming and outgoing [:DERIVED_FROM] neighbours for
// the given fact path, scoped to facts visible on the given branch.
// Incoming excludes deleted referrers. Outgoing includes deleted targets
// (marked with Deleted: true) so the UI can show them distinctly.
// Self-loops are filtered out: GraphQLite creates (n)-[:DERIVED_FROM]->(n) when
// the target node is absent at edge-creation time (upstream bug).
func (idx *store) ExplainFact(ctx context.Context, branch, path string) (ExplainResult, error) {
	branchID, err := idx.rh.branchID(ctx, branch)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain: %w", err)
	}

	// Resolve the blob_hash for this path on the given branch so we query
	// the specific fact version visible on this branch, not all versions.
	var blobHash string
	err = conn(ctx, idx.rh.db).QueryRowContext(ctx,
		`SELECT f.blob_hash FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id
		 WHERE bf.branch_id = ? AND bf.path = ?`, branchID, path,
	).Scan(&blobHash)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain: resolve blob_hash: %w", err)
	}

	params, _ := json.Marshal(map[string]string{"path": path, "blob_hash": blobHash})
	pj := string(params)

	// Incoming: all non-deleted facts that reference ANY version at this path.
	// Scoped to branch-visible facts via filterByBranch.
	incoming, err := idx.queryRefSummaries(ctx,
		fmt.Sprintf(`MATCH (f:%s)-[:%s]->(t:%s {path: $path}) WHERE NOT f.deleted = true RETURN f.path AS path, f.title AS title, false AS deleted`,
			NodeFact, EdgeDerivedFrom, NodeFact),
		pj,
	)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain incoming: %w", err)
	}

	// Outgoing: refs from the specific version visible on this branch.
	outgoing, err := idx.queryRefSummaries(ctx,
		fmt.Sprintf(`MATCH (f:%s {path: $path})-[:%s]->(t:%s) WHERE f.blob_hash = $blob_hash RETURN t.path AS path, t.title AS title, t.deleted AS deleted`,
			NodeFact, EdgeDerivedFrom, NodeFact),
		pj,
	)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain outgoing: %w", err)
	}

	return ExplainResult{
		Incoming: idx.filterByBranch(ctx, filterSelf(incoming, path), branchID),
		Outgoing: filterSelf(outgoing, path),
	}, nil
}

// filterSelf removes any RefSummary whose path equals selfPath (self-loops).
func filterSelf(refs []RefSummary, selfPath string) []RefSummary {
	out := refs[:0]
	for _, r := range refs {
		if r.Path != selfPath {
			out = append(out, r)
		}
	}
	return out
}

// filterByBranch keeps only RefSummary entries whose path is visible on the
// given branch (present in branch_facts).
func (idx *store) filterByBranch(ctx context.Context, refs []RefSummary, branchID int64) []RefSummary {
	if len(refs) == 0 {
		return refs
	}
	visible := make(map[string]bool)
	rows, err := conn(ctx, idx.rh.db).QueryContext(ctx, `SELECT path FROM branch_facts WHERE branch_id = ?`, branchID)
	if err != nil {
		return refs
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		rows.Scan(&p)
		visible[p] = true
	}
	out := refs[:0]
	for _, r := range refs {
		if visible[r.Path] {
			out = append(out, r)
		}
	}
	return out
}

// isDeletedVal interprets the raw value returned by json_extract for a boolean
// property. SQLite maps JSON true→int64(1), JSON false→int64(0), and Cypher
// literal false→string("0"). Returns true only for int64(1).
func isDeletedVal(v interface{}) bool {
	switch val := v.(type) {
	case bool:
		return val
	case int64:
		return val == 1
	case []byte:
		return string(val) == "1"
	}
	return false
}

// VersionSummary is one entry in a fact's version history.
type VersionSummary struct {
	CommitHash  string `json:"commit_hash"`
	CommittedAt int64  `json:"committed_at"`
	Title       string `json:"title"`
}

// FactVersionHistory returns all known historical versions of path, newest first.
// Only versions that have been graph-indexed (FactVersion nodes exist) are returned.
// Uses direct SQL against EAV tables for reliability (GraphQLite parameterized
// MATCH does not reliably return SET properties via json_each).
// branch is validated but not used for filtering — version history is commit-level.
func (idx *store) FactVersionHistory(ctx context.Context, branch, path string) ([]VersionSummary, error) {
	if _, err := idx.rh.branchID(ctx, branch); err != nil {
		return nil, fmt.Errorf("FactVersionHistory: %w", err)
	}
	// committed_at is stored in node_props_real (graphSetFactVersionProps uses
	// INSERT INTO node_props_real); title is in node_props_text.
	rows, err := conn(ctx, idx.rh.db).QueryContext(ctx, `
		SELECT
			COALESCE(ch_prop.value, '') AS commit_hash,
			CAST(COALESCE(ca_prop.value, 0) AS INTEGER) AS committed_at,
			COALESCE(title_prop.value, '') AS title
		FROM node_labels nl
		JOIN node_props_text path_prop ON path_prop.node_id = nl.node_id
			AND path_prop.key_id = (SELECT id FROM property_keys WHERE key = 'path' LIMIT 1)
		LEFT JOIN node_props_text ch_prop ON ch_prop.node_id = nl.node_id
			AND ch_prop.key_id = (SELECT id FROM property_keys WHERE key = 'commit_hash' LIMIT 1)
		LEFT JOIN node_props_real ca_prop ON ca_prop.node_id = nl.node_id
			AND ca_prop.key_id = (SELECT id FROM property_keys WHERE key = 'committed_at' LIMIT 1)
		LEFT JOIN node_props_text title_prop ON title_prop.node_id = nl.node_id
			AND title_prop.key_id = (SELECT id FROM property_keys WHERE key = 'title' LIMIT 1)
		WHERE nl.label = ? AND path_prop.value = ?
		ORDER BY COALESCE(ca_prop.value, 0) DESC
	`, NodeFactVersion, path)
	if err != nil {
		return nil, fmt.Errorf("FactVersionHistory: %w", err)
	}
	defer rows.Close()

	var result []VersionSummary
	for rows.Next() {
		var v VersionSummary
		if err := rows.Scan(&v.CommitHash, &v.CommittedAt, &v.Title); err != nil {
			return nil, fmt.Errorf("FactVersionHistory: scan: %w", err)
		}
		if v.CommitHash == "" {
			continue
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

// ExplainFactAt returns the incoming and outgoing DERIVED_FROM neighbours for
// the FactVersion identified by (path, commitHash).
//
// Outgoing: refs declared in this specific version (FactVersion→Fact edges).
// Incoming: all FactVersions that reference this path's Fact node.
// Self-loops are filtered out.
//
// Both queries use direct SQL against the EAV tables to avoid GraphQLite's
// same-label two-node MATCH self-loop bug (established in Task 3).
func (idx *store) ExplainFactAt(ctx context.Context, branch, path, commitHash string) (ExplainResult, error) {
	// branch is accepted for API consistency but not used for filtering:
	// ExplainFactAt queries historical FactVersion nodes which exist outside
	// branch scoping. The incoming/outgoing refs reflect what was true at
	// the given commit, regardless of current branch state.
	_, err := idx.rh.branchID(ctx, branch)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("ExplainFactAt: %w", err)
	}

	// Resolve FactVersion node ID for (path, commitHash).
	var versionID int64
	err = conn(ctx, idx.rh.db).QueryRowContext(ctx, `
		SELECT np.node_id
		FROM node_props_text np
		JOIN property_keys pk ON pk.id = np.key_id
		JOIN node_labels nl ON nl.node_id = np.node_id
		WHERE pk.key = 'commit_hash' AND np.value = ? AND nl.label = ?
		LIMIT 1
	`, commitHash, NodeFactVersion).Scan(&versionID)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("ExplainFactAt: resolve version node: %w", err)
	}

	// Outgoing: DERIVED_FROM edges from this FactVersion to Fact nodes.
	outgoing, err := idx.refSummariesByEdgeSource(ctx, versionID, EdgeDerivedFrom, NodeFact)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("ExplainFactAt outgoing: %w", err)
	}

	// Incoming: DERIVED_FROM edges from any FactVersion to the Fact node for path.
	factID, err := idx.graphNodeIDByProp(ctx, NodeFact, "path", path)
	if err != nil || factID == 0 {
		// No Fact node means no incoming refs — return what we have.
		return ExplainResult{
			Outgoing: filterSelf(outgoing, path),
			Incoming: []RefSummary{},
		}, nil
	}
	incoming, err := idx.refSummariesByEdgeTarget(ctx, factID, EdgeDerivedFrom, NodeFactVersion)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("ExplainFactAt incoming: %w", err)
	}

	return ExplainResult{
		Incoming: filterSelf(incoming, path),
		Outgoing: filterSelf(outgoing, path),
	}, nil
}

// refSummariesByEdgeSource returns RefSummary entries for all target nodes
// reachable from sourceNodeID via edges of edgeType, where the target has label targetLabel.
// It reads path and title properties from the EAV tables.
func (idx *store) refSummariesByEdgeSource(ctx context.Context, sourceNodeID int64, edgeType, targetLabel string) ([]RefSummary, error) {
	rows, err := conn(ctx, idx.rh.db).QueryContext(ctx, `
		SELECT DISTINCT
			path_prop.value AS path,
			COALESCE(title_prop.value, '') AS title
		FROM edges e
		JOIN node_labels nl ON nl.node_id = e.target_id AND nl.label = ?
		JOIN node_props_text path_prop ON path_prop.node_id = e.target_id
			AND path_prop.key_id = (SELECT id FROM property_keys WHERE key = 'path' LIMIT 1)
		LEFT JOIN node_props_text title_prop ON title_prop.node_id = e.target_id
			AND title_prop.key_id = (SELECT id FROM property_keys WHERE key = 'title' LIMIT 1)
		WHERE e.source_id = ? AND e.type = ?
	`, targetLabel, sourceNodeID, edgeType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRefSummaryRows(rows)
}

// refSummariesByEdgeTarget returns RefSummary entries for all source nodes
// pointing to targetNodeID via edges of edgeType, where the source has label sourceLabel.
// It reads path and title properties from the EAV tables.
func (idx *store) refSummariesByEdgeTarget(ctx context.Context, targetNodeID int64, edgeType, sourceLabel string) ([]RefSummary, error) {
	rows, err := conn(ctx, idx.rh.db).QueryContext(ctx, `
		SELECT DISTINCT
			path_prop.value AS path,
			COALESCE(title_prop.value, '') AS title
		FROM edges e
		JOIN node_labels nl ON nl.node_id = e.source_id AND nl.label = ?
		JOIN node_props_text path_prop ON path_prop.node_id = e.source_id
			AND path_prop.key_id = (SELECT id FROM property_keys WHERE key = 'path' LIMIT 1)
		LEFT JOIN node_props_text title_prop ON title_prop.node_id = e.source_id
			AND title_prop.key_id = (SELECT id FROM property_keys WHERE key = 'title' LIMIT 1)
		WHERE e.target_id = ? AND e.type = ?
	`, sourceLabel, targetNodeID, edgeType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRefSummaryRows(rows)
}

// scanRefSummaryRows scans (path, title) rows into []RefSummary.
func scanRefSummaryRows(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]RefSummary, error) {
	var result []RefSummary
	for rows.Next() {
		var path, title string
		if err := rows.Scan(&path, &title); err != nil {
			return nil, fmt.Errorf("scan ref summary: %w", err)
		}
		if path == "" {
			continue
		}
		result = append(result, RefSummary{Path: path, Title: title})
	}
	return result, rows.Err()
}

// queryRefSummaries runs a Cypher query that returns (path, title, deleted) rows.
// cypherQuery must contain only $param placeholders (no embedded values).
// paramsJSON is the JSON-encoded parameter object passed as cypher()'s second arg.
func (idx *store) queryRefSummaries(ctx context.Context, cypherQuery, paramsJSON string) ([]RefSummary, error) {
	q := `SELECT json_extract(value, '$.path'), json_extract(value, '$.title'), json_extract(value, '$.deleted') FROM json_each(cypher('` + cypherQuery + `', ?))`
	rows, err := conn(ctx, idx.rh.db).QueryContext(ctx, q, paramsJSON)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RefSummary{}
	for rows.Next() {
		var path, title string
		// json_extract on a JSON boolean returns an integer in SQLite (1=true, 0=false).
		// However, Cypher literal `false AS col` may return the string "0".
		// Scan into interface{} to handle both cases uniformly.
		var deletedRaw interface{}
		if err := rows.Scan(&path, &title, &deletedRaw); err != nil {
			return nil, fmt.Errorf("scan ref summary: %w", err)
		}
		if path == "" {
			continue
		}
		deleted := isDeletedVal(deletedRaw)
		result = append(result, RefSummary{Path: path, Title: title, Deleted: deleted})
	}
	return result, rows.Err()
}
