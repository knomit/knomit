package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	storegit "knomit/internal/store/git"
)

// ── Explain ───────────────────────────────────────────────────────────────────

// RefSummary is a lightweight fact reference returned by ExplainFact.
type RefSummary struct {
	Path        string `json:"path"`
	Title       string `json:"title"`
	Type        string `json:"type,omitempty"`   // epistemic type of the source (incoming) or target (outgoing) fact
	Commit      string `json:"commit,omitempty"` // source_commit for incoming, target_commit for outgoing
	Deleted     bool   `json:"deleted,omitempty"`
	CommittedAt int64  `json:"committed_at,omitempty"` // Unix seconds; 0 if commit_log row missing
}

// ExplainResult holds the incoming and outgoing reference summary for a fact.
type ExplainResult struct {
	Incoming []RefSummary `json:"incoming"`
	Outgoing []RefSummary `json:"outgoing"`
}

// ExplainFact returns the incoming and outgoing refs for path on branch at
// HEAD by resolving the path's HEAD-active commit and delegating to the
// commit-anchored methods. This unifies the HEAD and commit-anchored read
// paths on a single underlying query.
func (gs *graphStore) ExplainFact(ctx context.Context, branch, path string) (ExplainResult, error) {
	branchID, err := gs.rh.branchID(ctx, branch)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("ExplainFact: branchID: %w", err)
	}

	// active_commit_for(path, branch) lives in branch_facts.commit_hash.
	// A missing row means the path is not currently live on this branch
	// (retracted at HEAD, or never indexed) — surface as ErrFactNotLive so
	// handlers can map it to 404. Older versions may still be reachable via
	// commit-anchored endpoints; that's outside ExplainFact's HEAD-only scope.
	var activeCommit string
	err = conn(ctx, gs.rh.db).QueryRowContext(ctx,
		`SELECT commit_hash FROM branch_facts WHERE branch_id = ? AND path = ?`,
		branchID, path,
	).Scan(&activeCommit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExplainResult{}, ErrFactNotLive
		}
		return ExplainResult{}, fmt.Errorf("ExplainFact: resolve active commit: %w", err)
	}

	in, err := gs.IncomingAtCommit(ctx, branch, path, activeCommit)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("ExplainFact incoming: %w", err)
	}
	out, err := gs.OutgoingAtCommit(ctx, branch, path, activeCommit)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("ExplainFact outgoing: %w", err)
	}
	return ExplainResult{Incoming: in, Outgoing: out}, nil
}

// ── Graph operations ──────────────────────────────────────────────────────────
// Maintenance of the property graph, written as parameterised SQL over the EAV
// tables (see graph_sql.go). Mutations are idempotent by identity: Rebuild
// never wipes the graph, so it re-runs these writes on every pass.

// Node labels used in the property graph.
const (
	NodeFact         = "Fact"
	NodeEntity       = "Entity"
	NodeDomain       = "Domain"
	NodeOntologyNode = "OntologyNode"
)

// Edge types used in the property graph.
const (
	EdgeTagged          = "TAGGED"            // Fact → Entity
	EdgeInDomain        = "IN_DOMAIN"         // Fact → Domain
	EdgeUnder           = "UNDER"             // Fact → OntologyNode
	EdgeDerivedFrom     = "DERIVED_FROM"      // Fact → Fact (local ref lineage)
	EdgeSimilarTo       = "SIMILAR_TO"        // Fact ↔ Fact (KNN similarity)
	EdgeDomainChildOf   = "DOMAIN_CHILD_OF"   // Domain → Domain (hierarchy)
	EdgeOntologyChildOf = "ONTOLOGY_CHILD_OF" // OntologyNode → OntologyNode (hierarchy)
)

// graphSyncFact creates or updates graph nodes and node-edge relationships
// (entity / domain / ontology) for a fact. DERIVED_FROM edges are NOT
// written here — they are written post-commit by writePostCommitDerivedFrom
// in search_crud.go, because the new graphAddDerivedFromAtCommitTx requires
// node IDs that are only visible after the surrounding tx commits.
//
// Steps:
//  1. MERGE Fact node
//  2. Delete old TAGGED, IN_DOMAIN, UNDER, DERIVED_FROM edges
//  3. MERGE Entity nodes + TAGGED edges
//  4. MERGE Domain hierarchy + IN_DOMAIN edges
//  5. MERGE OntologyNode hierarchy + UNDER edge
func (si *searchIndex) graphSyncFact(ctx context.Context, rec FactRecord) error {
	return si.graphSyncFactTx(ctx, si.rh.db, rec)
}

// graphSyncFactTx is the transactional version of graphSyncFact.
//
// Direct SQL over the EAV tables (see graph_sql.go). Properties are stored as
// TEXT: confidence/sources are never read back by any graph query, and the one
// predicate that reads `deleted` compares it against 'true'/'false'.
func (si *searchIndex) graphSyncFactTx(ctx context.Context, tx storegit.CtxExecer, rec FactRecord) error {
	// 1. MERGE Fact node keyed by {path, blob_hash} — each fact version gets
	// its own graph node (immutable once created) — then set its properties.
	factID, err := graphMergeNode(ctx, tx, NodeFact, map[string]string{
		"path":      rec.Path,
		"blob_hash": rec.BlobHash,
	})
	if err != nil {
		return fmt.Errorf("graph merge fact: %w", err)
	}
	if err := graphSetNodeProps(ctx, tx, factID, map[string]string{
		"title":      rec.Title,
		"user_id":    rec.Path, // preserved from the Cypher path: user_id mirrors path
		"confidence": fmt.Sprintf("%f", rec.Confidence),
		"sources":    strconv.Itoa(rec.Sources),
		"deleted":    "false",
		"type":       rec.Type,
	}); err != nil {
		return fmt.Errorf("graph set fact props: %w", err)
	}

	// 2. Delete old relationship edges for this fact version.
	for _, edgeType := range []string{EdgeTagged, EdgeInDomain, EdgeUnder, EdgeDerivedFrom} {
		if err := graphDeleteOutgoingEdges(ctx, tx, factID, edgeType); err != nil {
			return fmt.Errorf("graph delete old %s edges: %w", edgeType, err)
		}
	}

	// 3. MERGE Entity nodes + TAGGED edges.
	for _, entity := range rec.Entities {
		entID, err := graphMergeNode(ctx, tx, NodeEntity, map[string]string{"name": entity})
		if err != nil {
			return fmt.Errorf("graph merge entity %s: %w", entity, err)
		}
		if err := graphMergeEdge(ctx, tx, factID, entID, EdgeTagged); err != nil {
			return fmt.Errorf("graph tagged %s: %w", entity, err)
		}
	}

	// 4. MERGE Domain hierarchy + IN_DOMAIN edges.
	for _, domain := range rec.Domain {
		if err := si.graphMergeDomainHierarchy(ctx, tx, factID, domain); err != nil {
			return err
		}
	}

	// 5. MERGE OntologyNode hierarchy + UNDER edge.
	if err := si.graphMergeOntologyHierarchy(ctx, tx, factID, rec.Path); err != nil {
		return err
	}

	return nil
}

// graphMergeHierarchy creates the ancestor chain for a '/'-separated path,
// linking each segment to its parent with childOfEdge, and returns the leaf
// node id. Shared by the Domain and OntologyNode hierarchies, which differ
// only in node label and child-of edge type.
func (si *searchIndex) graphMergeHierarchy(ctx context.Context, tx storegit.CtxExecer, label, childOfEdge string, parts []string) (int64, error) {
	var leafID int64
	var parentID int64
	for i := range parts {
		seg := strings.Join(parts[:i+1], "/")
		segID, err := graphMergeNode(ctx, tx, label, map[string]string{"path": seg})
		if err != nil {
			return 0, fmt.Errorf("graph merge %s %s: %w", label, seg, err)
		}
		if i > 0 {
			if err := graphMergeEdge(ctx, tx, segID, parentID, childOfEdge); err != nil {
				return 0, fmt.Errorf("graph %s %s: %w", childOfEdge, seg, err)
			}
		}
		parentID = segID
		leafID = segID
	}
	return leafID, nil
}

// graphMergeDomainHierarchy creates the full domain ancestor chain and links
// the fact to the leaf domain via IN_DOMAIN.
func (si *searchIndex) graphMergeDomainHierarchy(ctx context.Context, tx storegit.CtxExecer, factID int64, domain string) error {
	leafID, err := si.graphMergeHierarchy(ctx, tx, NodeDomain, EdgeDomainChildOf, strings.Split(domain, "/"))
	if err != nil {
		return err
	}
	if err := graphMergeEdge(ctx, tx, factID, leafID, EdgeInDomain); err != nil {
		return fmt.Errorf("graph in_domain %s: %w", domain, err)
	}
	return nil
}

// graphMergeOntologyHierarchy creates OntologyNode chain from the fact's file
// path and links the fact to the leaf via UNDER.
func (si *searchIndex) graphMergeOntologyHierarchy(ctx context.Context, tx storegit.CtxExecer, factID int64, factPath string) error {
	parts := strings.Split(factPath, "/")
	if len(parts) < 2 {
		return nil
	}
	dirParts := parts[:len(parts)-1]

	leafID, err := si.graphMergeHierarchy(ctx, tx, NodeOntologyNode, EdgeOntologyChildOf, dirParts)
	if err != nil {
		return err
	}
	if err := graphMergeEdge(ctx, tx, factID, leafID, EdgeUnder); err != nil {
		return fmt.Errorf("graph under %s: %w", factPath, err)
	}
	return nil
}

// graphDeleteFact marks a Fact node as deleted and removes its outgoing edges
// (except incoming DERIVED_FROM, which preserves lineage).
func (si *searchIndex) graphDeleteFact(ctx context.Context, path, blobHash string) error {
	return si.graphDeleteFactTx(ctx, si.rh.db, path, blobHash)
}

// graphSyncHistoricalFactTx MERGEs a Fact node for an orphaned historical
// blob version (one that exists in commit_log but not in the current
// `facts` table — typically because the fact was updated/retracted and
// GC removed the old row). The node is created with deleted=true and
// without outgoing TAGGED/IN_DOMAIN/UNDER edges, since those represent
// "what is currently claimed" relations and don't apply to retired
// versions. DERIVED_FROM edges are written separately by Phase B of
// rebuildGraph using this node as the source endpoint.
//
// This is what makes the temporal graph honest: every (path, blob_hash)
// ever indexed retains a node forever, so historical DERIVED_FROM edges
// can be walked end-to-end. Without it, lineage queries silently break
// at GC'd boundaries.
func (si *searchIndex) graphSyncHistoricalFactTx(ctx context.Context, tx storegit.CtxExecer, rec FactRecord) error {
	// MERGE the node keyed by (path, blob_hash) and set its frozen-in-time
	// properties + deleted=true. If the node already exists (e.g. from a
	// prior live indexing of this blob version that was later soft-deleted),
	// MERGE is a no-op for the node and the property write overwrites with the
	// historical values; deleted stays true.
	factID, err := graphMergeNode(ctx, tx, NodeFact, map[string]string{
		"path":      rec.Path,
		"blob_hash": rec.BlobHash,
	})
	if err != nil {
		return fmt.Errorf("graph merge historical fact: %w", err)
	}
	if err := graphSetNodeProps(ctx, tx, factID, map[string]string{
		"title":      rec.Title,
		"user_id":    rec.Path,
		"confidence": fmt.Sprintf("%f", rec.Confidence),
		"sources":    strconv.Itoa(rec.Sources),
		"deleted":    "true",
		"type":       rec.Type,
	}); err != nil {
		return fmt.Errorf("graph set historical fact props: %w", err)
	}
	return nil
}

func (si *searchIndex) graphDeleteFactTx(ctx context.Context, tx storegit.CtxExecer, path, blobHash string) error {
	factID, err := graphNodeIDByProps(ctx, tx, NodeFact, map[string]string{
		"path":      path,
		"blob_hash": blobHash,
	})
	if err != nil {
		return fmt.Errorf("graph delete fact: node lookup: %w", err)
	}
	if factID == 0 {
		return nil // no such version in the graph — nothing to retract
	}

	// Delete outgoing "current state" edges (TAGGED → Entity, IN_DOMAIN → Domain,
	// UNDER → OntologyNode, SIMILAR_TO → Fact). These represent what the fact
	// currently claims; a retracted fact makes no current claims.
	//
	// DERIVED_FROM edges are NOT deleted: they are immutable historical
	// assertions of lineage at a specific commit. Removing them would erase
	// the temporal view (a target fact would lose incoming edges from its
	// now-retracted referrers, leaving an unexplainable empty in-edge rail).
	for _, edgeType := range []string{EdgeTagged, EdgeInDomain, EdgeUnder, EdgeSimilarTo} {
		if err := graphDeleteOutgoingEdges(ctx, tx, factID, edgeType); err != nil {
			return fmt.Errorf("graph delete outgoing %s edges: %w", edgeType, err)
		}
	}
	// Delete incoming SIMILAR_TO edges (bidirectional cleanup).
	if err := graphDeleteIncomingEdges(ctx, tx, factID, EdgeSimilarTo); err != nil {
		return fmt.Errorf("graph delete incoming SIMILAR_TO: %w", err)
	}
	// Mark node as deleted.
	if err := graphSetNodeProps(ctx, tx, factID, map[string]string{"deleted": "true"}); err != nil {
		return fmt.Errorf("graph mark deleted: %w", err)
	}
	return nil
}

// knnK caps how many nearest neighbours are considered per fact. The cosine
// floor for actually drawing a SIMILAR_TO edge is model-dependent and comes from
// the active embedder's Thresholds().SimilarTo (see internal/retrieval).
const knnK = 10 // top-K nearest neighbors per fact

// graphBuildSimilarityEdges creates SIMILAR_TO edges from a fact version to its
// top-K nearest neighbors (by cosine similarity via sqlite-vec KNN).
// Edges below the similarity threshold are not created.
//
// IMPORTANT: This function queries sqlite-vec (facts_vec) directly via si.db,
// so it must be called AFTER the surrounding transaction has committed.
// Calling it inside a transaction will not see uncommitted embedding writes.
func (si *searchIndex) graphBuildSimilarityEdges(ctx context.Context, path, blobHash string) error {
	emb, err := si.getEmbeddingByFact(ctx, path, blobHash)
	if err != nil || emb == nil {
		return nil
	}

	vecBlob := float32SliceToBytes(emb)

	// Collect neighbors first, then close rows before running Cypher mutations.
	// Running Exec() on the same *sql.DB while rows are open can interfere in
	// SQLite's single-writer model.
	type neighbor struct {
		path       string
		blobHash   string
		similarity float64
	}
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT f.path, f.blob_hash, (1.0 - fv.distance) as similarity
		 FROM facts_vec fv
		 JOIN facts f ON f.id = fv.rowid
		 WHERE fv.embedding MATCH ? AND fv.k = ?
		 ORDER BY fv.distance ASC`,
		vecBlob, knnK+1,
	)
	if err != nil {
		return fmt.Errorf("knn query for %s: %w", path, err)
	}
	var neighbors []neighbor
	for rows.Next() {
		var n neighbor
		var sim sql.NullFloat64
		if err := rows.Scan(&n.path, &n.blobHash, &sim); err != nil {
			rows.Close()
			return fmt.Errorf("scan knn row: %w", err)
		}
		// Skip neighbors with a NULL similarity (degenerate/zero-norm
		// embedding) rather than aborting the whole edge build for this fact.
		// See usableKNNSimilarity for the invariant.
		s, ok := usableKNNSimilarity(sim)
		if !ok {
			log.Debug().Str("source", path).Str("neighbor", n.path).
				Msg("knn: skipping neighbor with NULL similarity (degenerate/zero-norm embedding)")
			continue
		}
		n.similarity = s
		neighbors = append(neighbors, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("knn rows: %w", err)
	}
	rows.Close()

	// Delete old outgoing SIMILAR_TO edges for this fact version.
	db := conn(ctx, si.rh.db)
	srcID, err := graphNodeIDByProps(ctx, db, NodeFact, map[string]string{
		"path":      path,
		"blob_hash": blobHash,
	})
	if err != nil {
		return fmt.Errorf("SIMILAR_TO: source node lookup: %w", err)
	}
	if srcID == 0 {
		return nil // source version has no graph node yet — nothing to link
	}
	if err := graphDeleteOutgoingEdges(ctx, db, srcID, EdgeSimilarTo); err != nil {
		return fmt.Errorf("delete old SIMILAR_TO: %w", err)
	}

	simFloor := EmbedderThresholds(si.rh.getEmbedder()).SimilarTo
	for _, n := range neighbors {
		if n.path == path && n.blobHash == blobHash {
			continue
		}
		if n.similarity < simFloor {
			continue
		}
		tgtID, err := graphNodeIDByProps(ctx, db, NodeFact, map[string]string{
			"path":      n.path,
			"blob_hash": n.blobHash,
		})
		if err != nil {
			return fmt.Errorf("SIMILAR_TO %s→%s: target lookup: %w", path, n.path, err)
		}
		if tgtID == 0 {
			continue // neighbour not in the graph (yet) — skip, as MATCH would
		}
		if err := graphMergeEdge(ctx, db, srcID, tgtID, EdgeSimilarTo); err != nil {
			return fmt.Errorf("create SIMILAR_TO %s→%s: %w", path, n.path, err)
		}
	}
	return nil
}

// graphNodeIDByProp returns the node ID for a node with the given label, where
// the property named propKey equals propVal. Returns 0 if not found.
func (si *searchIndex) graphNodeIDByProp(ctx context.Context, label, propKey, propVal string) (int64, error) {
	var nodeID int64
	err := conn(ctx, si.rh.db).QueryRowContext(ctx, `
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
// the property graph directly.
func (si *searchIndex) graphInsertEdge(ctx context.Context, sourceID, targetID int64, edgeType string) error {
	_, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`INSERT OR IGNORE INTO edges (source_id, target_id, type) VALUES (?, ?, ?)`,
		sourceID, targetID, edgeType,
	)
	return err
}
