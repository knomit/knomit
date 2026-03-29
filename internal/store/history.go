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
	// Edges and property SETs must be applied after commit: node IDs are only
	// visible post-commit, and GraphQLite MATCH+SET doesn't persist EAV properties
	// inside a *sql.Tx.
	type prevEdge struct {
		newerHash, olderHash string
	}
	type createdVersion struct {
		path, commitHash string
		refs             []string
		rec              FactRecord
		committedAt      int64
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

		rec, err := parseFact(v.path, content)
		if err != nil {
			log.Debug().Err(err).Str("path", v.path).Msg("rebuildGraphHistory: skip (parse failed)")
			continue
		}

		if err := idx.graphSyncFactVersionTx(tx, v.commitHash, rec, v.committedAt); err != nil {
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
		created = append(created, createdVersion{v.path, v.commitHash, localRefs, rec, v.committedAt})
		done++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rebuildGraphHistory: commit nodes tx: %w", err)
	}

	if progress != nil {
		progress("history", done, total)
	}

	// Phase 1.5: set title and committed_at on each FactVersion node now that
	// the transaction has committed. GraphQLite MATCH+SET does not persist EAV
	// properties when executed inside a *sql.Tx, so this must run post-commit.
	for _, cv := range created {
		if err := idx.graphSetFactVersionProps(cv.commitHash, cv.rec, cv.committedAt); err != nil {
			log.Warn().Err(err).Str("path", cv.path).Str("commit", cv.commitHash[:8]).Msg("rebuildGraphHistory: set props failed")
		}
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
		versionID, err := idx.graphNodeIDByProp(NodeFactVersion, "commit_hash", cv.commitHash)
		if err != nil || versionID == 0 {
			continue
		}
		for _, ref := range cv.refs {
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

// graphSyncFactVersionTx creates a FactVersion node (MERGE only) within the
// given transaction. Properties (title, committed_at) must be set after the
// transaction commits via graphSetFactVersionProps, because GraphQLite's
// MATCH+SET does not persist to EAV tables when executed inside a *sql.Tx.
func (idx *Index) graphSyncFactVersionTx(tx execer, commitHash string, rec FactRecord, committedAt int64) error {
	p := escapeCypherKey(rec.Path)
	ch := escapeCypherKey(commitHash)

	// MERGE the FactVersion node (identity props only).
	q := fmt.Sprintf(`SELECT cypher('MERGE (v:%s {path: "%s", commit_hash: "%s"})')`,
		NodeFactVersion, p, ch)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graphSyncFactVersionTx: merge node: %w", err)
	}
	return nil
}

// graphSetFactVersionProps sets title and committed_at on an existing FactVersion
// node via direct SQL INSERTs into the EAV tables. GraphQLite's MATCH+SET
// silently drops property writes (confirmed: title/committed_at never appear
// in node_props_text or node_props_real after a Cypher SET), so we bypass
// Cypher entirely and INSERT directly into the EAV tables.
//
// Must be called after the transaction that created the node has committed,
// because node IDs are only visible post-commit.
func (idx *Index) graphSetFactVersionProps(commitHash string, rec FactRecord, committedAt int64) error {
	nodeID, err := idx.graphNodeIDByProp(NodeFactVersion, "commit_hash", commitHash)
	if err != nil || nodeID == 0 {
		return fmt.Errorf("graphSetFactVersionProps: node not found for commit_hash=%s: %w", commitHash, err)
	}

	// Ensure property key IDs exist, then upsert values into EAV tables.
	type textProp struct {
		key   string
		value string
	}
	for _, p := range []textProp{
		{"title", rec.Title},
	} {
		// Ensure property_key row exists.
		if _, err := idx.db.Exec(`INSERT OR IGNORE INTO property_keys(key) VALUES (?)`, p.key); err != nil {
			return fmt.Errorf("graphSetFactVersionProps: ensure key %s: %w", p.key, err)
		}
		var keyID int64
		if err := idx.db.QueryRow(`SELECT id FROM property_keys WHERE key = ?`, p.key).Scan(&keyID); err != nil {
			return fmt.Errorf("graphSetFactVersionProps: get key_id for %s: %w", p.key, err)
		}
		if _, err := idx.db.Exec(
			`INSERT OR REPLACE INTO node_props_text(node_id, key_id, value) VALUES (?, ?, ?)`,
			nodeID, keyID, p.value,
		); err != nil {
			return fmt.Errorf("graphSetFactVersionProps: set text prop %s: %w", p.key, err)
		}
	}

	// committed_at is an integer; store in node_props_real (GraphQLite uses REAL for numbers).
	if _, err := idx.db.Exec(`INSERT OR IGNORE INTO property_keys(key) VALUES (?)`, "committed_at"); err != nil {
		return fmt.Errorf("graphSetFactVersionProps: ensure key committed_at: %w", err)
	}
	var caKeyID int64
	if err := idx.db.QueryRow(`SELECT id FROM property_keys WHERE key = 'committed_at'`).Scan(&caKeyID); err != nil {
		return fmt.Errorf("graphSetFactVersionProps: get key_id for committed_at: %w", err)
	}
	if _, err := idx.db.Exec(
		`INSERT OR REPLACE INTO node_props_real(node_id, key_id, value) VALUES (?, ?, ?)`,
		nodeID, caKeyID, committedAt,
	); err != nil {
		return fmt.Errorf("graphSetFactVersionProps: set committed_at: %w", err)
	}

	return nil
}
