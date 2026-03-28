// History graph phase: creates FactVersion nodes from commit_log entries,
// linking them with PREV_VERSION chains and DERIVED_FROM edges.
//
// GraphQLite limitation: MATCH (a:L {p1: "x"}), (b:L {p1: "y"}) does not
// correctly find two distinct nodes of the same label by property values — it
// degenerates into a self-loop (a)-[:R]->(a). To work around this, PREV_VERSION
// and DERIVED_FROM edges are created via direct SQL INSERT INTO edges after
// looking up node IDs through the EAV property tables.
package store

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
)

// graphNodeIDByProp returns the node ID for a node with the given label, where
// the property named propKey equals propVal. Returns 0 if not found.
func (idx *Index) graphNodeIDByProp(label, propKey, propVal string) (int64, error) {
	var nodeID int64
	err := idx.db.QueryRow(`
		SELECT np.node_id
		FROM node_props_text np
		JOIN property_keys pk ON pk.id = np.key_id
		JOIN node_labels nl ON nl.node_id = np.node_id
		WHERE pk.key = ? AND np.value = ? AND nl.label = ?
		LIMIT 1
	`, propKey, propVal, label).Scan(&nodeID)
	if err != nil {
		return 0, err
	}
	return nodeID, nil
}

// graphInsertEdge inserts an edge directly into the edges table, bypassing
// the GraphQLite Cypher layer. This avoids the two-node MATCH self-loop bug.
func (idx *Index) graphInsertEdge(sourceID, targetID int64, edgeType string) error {
	_, err := idx.db.Exec(
		`INSERT OR IGNORE INTO edges (source_id, target_id, type) VALUES (?, ?, ?)`,
		sourceID, targetID, edgeType,
	)
	return err
}

// rebuildGraphHistory creates FactVersion nodes for every (path, commit_hash)
// row in commit_log. Versions of the same path are chained newest→oldest via
// PREV_VERSION. Each version's refs (local paths only) get DERIVED_FROM edges
// to the corresponding Fact node. Deleted entries are skipped.
//
// Returns the number of FactVersion nodes successfully created.
func (idx *Index) rebuildGraphHistory(git GitReader, branch string, progress RebuildProgress) (int, error) {
	rows, err := idx.db.Query(`
		SELECT path, commit_hash, committed_at
		FROM commit_log
		WHERE action != 'deleted'
		ORDER BY path ASC, committed_at ASC
	`)
	if err != nil {
		return 0, fmt.Errorf("rebuildGraphHistory: query: %w", err)
	}

	type versionRow struct {
		path, commitHash string
		committedAt      int64
	}
	var versions []versionRow
	for rows.Next() {
		var v versionRow
		if err := rows.Scan(&v.path, &v.commitHash, &v.committedAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("rebuildGraphHistory: scan: %w", err)
		}
		versions = append(versions, v)
	}
	rows.Close()

	total := len(versions)
	if progress != nil {
		progress("history", 0, total)
	}

	// Phase 1: create all FactVersion nodes in a single transaction.
	// Edges must be created after commit so that node IDs are visible.
	type prevEdge struct {
		newerHash, olderHash string
	}
	type createdVersion struct {
		path, commitHash string
		refs             []string
	}

	tx, err := idx.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("rebuildGraphHistory: begin tx: %w", err)
	}
	defer tx.Rollback()

	done := 0
	prevByPath := make(map[string]string)         // path → previous commit_hash
	prevEdgesByPath := make(map[string][]prevEdge) // path → edges to create
	var created []createdVersion

	for _, v := range versions {
		content, err := git.ReadFileAtCommit(branch, v.path, v.commitHash)
		if err != nil {
			log.Debug().Err(err).Str("path", v.path).Str("commit", v.commitHash[:8]).Msg("rebuildGraphHistory: skip (file not found at commit)")
			continue
		}

		rec, err := parseFact(v.path, content, v.commitHash)
		if err != nil {
			continue
		}

		if err := idx.graphSyncFactVersionTx(tx, rec, v.committedAt); err != nil {
			log.Warn().Err(err).Str("path", v.path).Str("commit", v.commitHash[:8]).Msg("rebuildGraphHistory: sync version failed, skipping")
			continue
		}

		if prev, ok := prevByPath[v.path]; ok {
			prevEdgesByPath[v.path] = append(prevEdgesByPath[v.path], prevEdge{v.commitHash, prev})
		}
		prevByPath[v.path] = v.commitHash

		var localRefs []string
		for _, ref := range rec.Refs {
			if !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
				localRefs = append(localRefs, ref)
			}
		}
		created = append(created, createdVersion{v.path, v.commitHash, localRefs})
		done++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rebuildGraphHistory: commit nodes tx: %w", err)
	}

	if progress != nil {
		progress("history", done, total)
	}

	// Phase 2: create PREV_VERSION and DERIVED_FROM edges via direct SQL.
	// GraphQLite's two-node MATCH-MERGE pattern creates self-loops when both
	// nodes share the same label, so we look up node IDs directly and INSERT.
	for path, edges := range prevEdgesByPath {
		for _, e := range edges {
			newerID, err := idx.graphNodeIDByProp(NodeFactVersion, "commit_hash", e.newerHash)
			if err != nil || newerID == 0 {
				log.Warn().Str("path", path).Str("commit", e.newerHash[:8]).Msg("rebuildGraphHistory: newer node not found for PREV_VERSION")
				continue
			}
			olderID, err := idx.graphNodeIDByProp(NodeFactVersion, "commit_hash", e.olderHash)
			if err != nil || olderID == 0 {
				log.Warn().Str("path", path).Str("commit", e.olderHash[:8]).Msg("rebuildGraphHistory: older node not found for PREV_VERSION")
				continue
			}
			if err := idx.graphInsertEdge(newerID, olderID, EdgePrevVersion); err != nil {
				log.Warn().Err(err).Str("path", path).Msg("rebuildGraphHistory: PREV_VERSION insert failed")
			}
		}
	}

	for _, cv := range created {
		for _, ref := range cv.refs {
			versionID, err := idx.graphNodeIDByProp(NodeFactVersion, "commit_hash", cv.commitHash)
			if err != nil || versionID == 0 {
				continue
			}
			targetID, err := idx.graphNodeIDByProp(NodeFact, "path", ref)
			if err != nil || targetID == 0 {
				// Target Fact node doesn't exist — skip (no self-loop risk with direct SQL).
				continue
			}
			if err := idx.graphInsertEdge(versionID, targetID, EdgeDerivedFrom); err != nil {
				log.Warn().Err(err).Str("ref", ref).Msg("rebuildGraphHistory: DERIVED_FROM insert failed")
			}
		}
	}

	return done, nil
}

// graphSyncFactVersionTx creates or updates a FactVersion node (MERGE + SET)
// within the given transaction. Edges (PREV_VERSION, DERIVED_FROM) are created
// after commit via direct SQL to avoid the GraphQLite two-node MATCH self-loop bug.
func (idx *Index) graphSyncFactVersionTx(tx execer, rec FactRecord, committedAt int64) error {
	p := escapeCypherKey(rec.Path)
	ch := escapeCypherKey(rec.CommitHash)
	title := escapeCypherVal(rec.Title)

	// MERGE the FactVersion node.
	q := fmt.Sprintf(`SELECT cypher('MERGE (v:%s {path: "%s", commit_hash: "%s"})')`,
		NodeFactVersion, p, ch)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graphSyncFactVersionTx: merge node: %w", err)
	}

	// SET properties (title, committed_at).
	// GraphQLite requires a separate MATCH+SET after MERGE to update properties.
	q = fmt.Sprintf(`SELECT cypher('MATCH (v:%s {path: "%s", commit_hash: "%s"}) SET v.title = "%s", v.committed_at = %d')`,
		NodeFactVersion, p, ch, title, committedAt)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graphSyncFactVersionTx: set props: %w", err)
	}

	return nil
}
