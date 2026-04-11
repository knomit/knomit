package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ── Explain ───────────────────────────────────────────────────────────────────

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
func (si *searchIndex) ExplainFact(ctx context.Context, branch, path string) (ExplainResult, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain: %w", err)
	}

	// Resolve the blob_hash for this path on the given branch so we query
	// the specific fact version visible on this branch, not all versions.
	var blobHash string
	err = conn(ctx, si.rh.db).QueryRowContext(ctx,
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
	incoming, err := si.queryRefSummaries(ctx,
		fmt.Sprintf(`MATCH (f:%s)-[:%s]->(t:%s {path: $path}) WHERE NOT f.deleted = true RETURN f.path AS path, f.title AS title, false AS deleted`,
			NodeFact, EdgeDerivedFrom, NodeFact),
		pj,
	)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain incoming: %w", err)
	}

	// Outgoing: refs from the specific version visible on this branch.
	outgoing, err := si.queryRefSummaries(ctx,
		fmt.Sprintf(`MATCH (f:%s {path: $path})-[:%s]->(t:%s) WHERE f.blob_hash = $blob_hash RETURN t.path AS path, t.title AS title, t.deleted AS deleted`,
			NodeFact, EdgeDerivedFrom, NodeFact),
		pj,
	)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain outgoing: %w", err)
	}

	return ExplainResult{
		Incoming: si.filterByBranch(ctx, filterSelf(incoming, path), branchID),
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
func (si *searchIndex) filterByBranch(ctx context.Context, refs []RefSummary, branchID int64) []RefSummary {
	if len(refs) == 0 {
		return refs
	}
	placeholders := make([]string, len(refs))
	args := make([]any, len(refs)+1)
	args[0] = branchID
	for i, r := range refs {
		placeholders[i] = "?"
		args[i+1] = r.Path
	}
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT path FROM branch_facts WHERE branch_id = ? AND path IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return refs
	}
	defer rows.Close()
	visible := make(map[string]bool, len(refs))
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return refs
		}
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

// refSummariesByEdgeSource returns RefSummary entries for all target nodes
// reachable from sourceNodeID via edges of edgeType, where the target has label targetLabel.
// It reads path and title properties from the EAV tables.
func (si *searchIndex) refSummariesByEdgeSource(ctx context.Context, sourceNodeID int64, edgeType, targetLabel string) ([]RefSummary, error) {
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, `
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
func (si *searchIndex) refSummariesByEdgeTarget(ctx context.Context, targetNodeID int64, edgeType, sourceLabel string) ([]RefSummary, error) {
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, `
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
func (si *searchIndex) queryRefSummaries(ctx context.Context, cypherQuery, paramsJSON string) ([]RefSummary, error) {
	q := `SELECT json_extract(value, '$.path'), json_extract(value, '$.title'), json_extract(value, '$.deleted') FROM json_each(cypher('` + cypherQuery + `', ?))`
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, q, paramsJSON)
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

// ── Graph operations ──────────────────────────────────────────────────────────
// Graph operations: Cypher wrappers for maintaining the knowledge graph.
// All graph mutations use MERGE for idempotency.
//
// Parameterized queries (cypher('...', params)) work for read operations
// (MATCH/RETURN) but NOT for write operations (MERGE/SET/DELETE) in the
// installed GraphQLite build. Write operations embed values via string
// interpolation using escapeCypherKey/escapeCypherVal.
//
// Note: MERGE+SET in a single Cypher statement does not work in GraphQLite;
// a subsequent MATCH+SET is required to update properties reliably.

// Node labels used in GraphQLite Cypher queries.
const (
	NodeFact         = "Fact"
	NodeEntity       = "Entity"
	NodeDomain       = "Domain"
	NodeOntologyNode = "OntologyNode"
	NodeFactVersion  = "FactVersion" // historical snapshot of a Fact at a specific commit
)

// Edge types used in GraphQLite Cypher queries.
const (
	EdgeTagged          = "TAGGED"            // Fact → Entity
	EdgeInDomain        = "IN_DOMAIN"         // Fact → Domain
	EdgeUnder           = "UNDER"             // Fact → OntologyNode
	EdgeDerivedFrom     = "DERIVED_FROM"      // Fact → Fact (local ref lineage)
	EdgeSimilarTo       = "SIMILAR_TO"        // Fact ↔ Fact (KNN similarity)
	EdgeDomainChildOf   = "DOMAIN_CHILD_OF"   // Domain → Domain (hierarchy)
	EdgeOntologyChildOf = "ONTOLOGY_CHILD_OF" // OntologyNode → OntologyNode (hierarchy)
	EdgePrevVersion     = "PREV_VERSION"      // FactVersion → older FactVersion (same path)
)

// graphSyncFact creates or updates graph nodes and edges for a fact.
// This implements the Learn mutation from the spec:
//  1. MERGE Fact node
//  2. Delete old TAGGED, IN_DOMAIN, UNDER, DERIVED_FROM edges
//  3. MERGE Entity nodes + TAGGED edges
//  4. MERGE Domain hierarchy + IN_DOMAIN edges
//  5. MERGE OntologyNode hierarchy + UNDER edge
//  6. Sync DERIVED_FROM edges from local refs
func (si *searchIndex) graphSyncFact(ctx context.Context, rec FactRecord) error {
	return si.graphSyncFactTx(ctx, si.rh.db, rec)
}

// graphSyncFactTx is the transactional version of graphSyncFact.
func (si *searchIndex) graphSyncFactTx(ctx context.Context, tx execer, rec FactRecord) error {
	path := escapeCypherKey(rec.Path)
	bh := escapeCypherKey(rec.BlobHash)
	title := escapeCypherVal(rec.Title)

	// 1. MERGE Fact node keyed by {path, blob_hash} — each fact version gets
	// its own graph node (immutable once created). Then SET properties in a
	// separate statement.
	//
	// GraphQLite limitation: MATCH with multiple property predicates silently
	// fails for write operations (SET, edge MERGE, DELETE). MERGE with multiple
	// properties works for node creation, but all subsequent write operations
	// must use MATCH{path} + WHERE blob_hash = "..." to filter correctly.
	q := fmt.Sprintf(`SELECT cypher('MERGE (f:%s {path: "%s", blob_hash: "%s"})')`, NodeFact, path, bh)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph merge fact: %w", err)
	}
	q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}) WHERE f.blob_hash = "%s" SET f.title = "%s", f.user_id = "%s", f.confidence = %f, f.sources = %d, f.deleted = false, f.type = "%s"')`,
		NodeFact, path, bh, title, path, rec.Confidence, rec.Sources, escapeCypherVal(rec.Type))
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph set fact props: %w", err)
	}

	// 2. Delete old relationship edges for this fact version.
	for _, edgeType := range []string{EdgeTagged, EdgeInDomain, EdgeUnder, EdgeDerivedFrom} {
		q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"})-[r:%s]->() WHERE f.blob_hash = "%s" DELETE r')`, NodeFact, path, edgeType, bh)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph delete old %s edges: %w", edgeType, err)
		}
	}

	// 3. MERGE Entity nodes + TAGGED edges.
	// GraphQLite silently ignores the third MERGE in a multi-MERGE query, so
	// we split: first MERGE the entity node, then MATCH both and MERGE the edge.
	for _, entity := range rec.Entities {
		e := escapeCypherKey(entity)
		q = fmt.Sprintf(`SELECT cypher('MERGE (e:%s {name: "%s"})')`, NodeEntity, e)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph merge entity %s: %w", entity, err)
		}
		q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}), (e:%s {name: "%s"}) WHERE f.blob_hash = "%s" MERGE (f)-[:%s]->(e)')`, NodeFact, path, NodeEntity, e, bh, EdgeTagged)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph tagged %s: %w", entity, err)
		}
	}

	// 4. MERGE Domain hierarchy + IN_DOMAIN edges.
	for _, domain := range rec.Domain {
		if err := si.graphMergeDomainHierarchy(ctx, tx, rec.Path, rec.BlobHash, domain); err != nil {
			return err
		}
	}

	// 5. MERGE OntologyNode hierarchy + UNDER edge.
	if err := si.graphMergeOntologyHierarchy(ctx, tx, rec.Path, rec.BlobHash); err != nil {
		return err
	}

	// 6. Sync DERIVED_FROM edges from local refs (invariant: always matches rec.Refs).
	var localRefs []string
	for _, r := range rec.Refs {
		if !strings.HasPrefix(r, "http://") && !strings.HasPrefix(r, "https://") {
			localRefs = append(localRefs, r)
		}
	}
	if len(localRefs) > 0 {
		if err := si.graphAddDerivedFromTx(ctx, tx, rec.Path, rec.BlobHash, localRefs); err != nil {
			return fmt.Errorf("graph sync derived_from: %w", err)
		}
	}

	return nil
}

// graphMergeDomainHierarchy creates the full domain ancestor chain and links
// the fact to the leaf domain via IN_DOMAIN.
func (si *searchIndex) graphMergeDomainHierarchy(ctx context.Context, tx execer, factPath, factBlobHash, domain string) error {
	parts := strings.Split(domain, "/")
	for i := range parts {
		seg := strings.Join(parts[:i+1], "/")
		escaped := escapeCypherKey(seg)
		q := fmt.Sprintf(`SELECT cypher('MERGE (:%s {path: "%s"})')`, NodeDomain, escaped)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph merge domain %s: %w", seg, err)
		}
		if i > 0 {
			parent := escapeCypherKey(strings.Join(parts[:i], "/"))
			q = fmt.Sprintf(`SELECT cypher('MATCH (c:%s {path: "%s"}), (p:%s {path: "%s"}) MERGE (c)-[:%s]->(p)')`, NodeDomain, escaped, NodeDomain, parent, EdgeDomainChildOf)
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("graph domain child_of %s: %w", seg, err)
			}
		}
	}
	leaf := escapeCypherKey(domain)
	fp := escapeCypherKey(factPath)
	fbh := escapeCypherKey(factBlobHash)
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}), (d:%s {path: "%s"}) WHERE f.blob_hash = "%s" MERGE (f)-[:%s]->(d)')`, NodeFact, fp, NodeDomain, leaf, fbh, EdgeInDomain)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph in_domain %s: %w", domain, err)
	}
	return nil
}

// graphMergeOntologyHierarchy creates OntologyNode chain from the fact's file
// path and links the fact to the leaf via UNDER.
func (si *searchIndex) graphMergeOntologyHierarchy(ctx context.Context, tx execer, factPath, factBlobHash string) error {
	parts := strings.Split(factPath, "/")
	if len(parts) < 2 {
		return nil
	}
	dirParts := parts[:len(parts)-1]

	for i := range dirParts {
		seg := strings.Join(dirParts[:i+1], "/")
		escaped := escapeCypherKey(seg)
		q := fmt.Sprintf(`SELECT cypher('MERGE (:%s {path: "%s"})')`, NodeOntologyNode, escaped)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph merge ontology %s: %w", seg, err)
		}
		if i > 0 {
			parent := escapeCypherKey(strings.Join(dirParts[:i], "/"))
			q = fmt.Sprintf(`SELECT cypher('MATCH (c:%s {path: "%s"}), (p:%s {path: "%s"}) MERGE (c)-[:%s]->(p)')`, NodeOntologyNode, escaped, NodeOntologyNode, parent, EdgeOntologyChildOf)
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("graph ontology child_of %s: %w", seg, err)
			}
		}
	}
	leaf := escapeCypherKey(strings.Join(dirParts, "/"))
	fp := escapeCypherKey(factPath)
	fbh := escapeCypherKey(factBlobHash)
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}), (o:%s {path: "%s"}) WHERE f.blob_hash = "%s" MERGE (f)-[:%s]->(o)')`, NodeFact, fp, NodeOntologyNode, leaf, fbh, EdgeUnder)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph under %s: %w", factPath, err)
	}
	return nil
}

// graphDeleteFact marks a Fact node as deleted and removes its outgoing edges
// (except incoming DERIVED_FROM, which preserves lineage).
func (si *searchIndex) graphDeleteFact(ctx context.Context, path, blobHash string) error {
	return si.graphDeleteFactTx(ctx, si.rh.db, path, blobHash)
}

func (si *searchIndex) graphDeleteFactTx(ctx context.Context, tx execer, path, blobHash string) error {
	p := escapeCypherKey(path)
	bh := escapeCypherKey(blobHash)
	// Delete outgoing edges.
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"})-[r]->() WHERE f.blob_hash = "%s" DELETE r')`, NodeFact, p, bh)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph delete outgoing edges: %w", err)
	}
	// Delete incoming SIMILAR_TO edges (bidirectional cleanup).
	q = fmt.Sprintf(`SELECT cypher('MATCH ()-[r:%s]->(f:%s {path: "%s"}) WHERE f.blob_hash = "%s" DELETE r')`, EdgeSimilarTo, NodeFact, p, bh)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph delete incoming SIMILAR_TO: %w", err)
	}
	// Mark node as deleted.
	q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}) WHERE f.blob_hash = "%s" SET f.deleted = true')`, NodeFact, p, bh)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph mark deleted: %w", err)
	}
	return nil
}

// graphAddDerivedFromTx creates DERIVED_FROM edges from a new fact version to
// its source facts. The source (new fact) is matched by {path, blob_hash}; the
// target is matched by path only (any version at that path).
//
// GraphQLite bug: when the target node is absent, MATCH degenerates and MERGE
// creates a self-loop (n)-[:DERIVED_FROM]->(n). We accept this and filter
// self-loops at query time in ExplainFact instead of pre-checking (which would
// silently drop valid edges when facts are indexed in different orders during
// rebuild).
func (si *searchIndex) graphAddDerivedFromTx(ctx context.Context, tx execer, newPath, newBlobHash string, sourcePaths []string) error {
	np := escapeCypherKey(newPath)
	nbh := escapeCypherKey(newBlobHash)
	for _, src := range sourcePaths {
		sp := escapeCypherKey(src)
		q := fmt.Sprintf(`SELECT cypher('MATCH (n:%s {path: "%s"}), (s:%s {path: "%s"}) WHERE n.blob_hash = "%s" MERGE (n)-[:%s]->(s)')`, NodeFact, np, NodeFact, sp, nbh, EdgeDerivedFrom)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph derived_from %s→%s: %w", newPath, src, err)
		}
	}
	return nil
}

const (
	knnK         = 10   // top-K nearest neighbors per fact
	knnThreshold = 0.60 // minimum cosine similarity for SIMILAR_TO edges
)

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
		if err := rows.Scan(&n.path, &n.blobHash, &n.similarity); err != nil {
			rows.Close()
			return fmt.Errorf("scan knn row: %w", err)
		}
		neighbors = append(neighbors, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("knn rows: %w", err)
	}
	rows.Close()

	// Delete old outgoing SIMILAR_TO edges for this fact version.
	p := escapeCypherKey(path)
	bh := escapeCypherKey(blobHash)
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"})-[r:%s]->() WHERE f.blob_hash = "%s" DELETE r')`, NodeFact, p, EdgeSimilarTo, bh)
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, q); err != nil {
		return fmt.Errorf("delete old SIMILAR_TO: %w", err)
	}

	for _, n := range neighbors {
		if n.path == path && n.blobHash == blobHash {
			continue
		}
		if n.similarity < knnThreshold {
			continue
		}
		np := escapeCypherKey(n.path)
		nbh := escapeCypherKey(n.blobHash)
		q = fmt.Sprintf(`SELECT cypher('MATCH (a:%s {path: "%s"}), (b:%s {path: "%s"}) WHERE a.blob_hash = "%s" AND b.blob_hash = "%s" MERGE (a)-[:%s]->(b)')`, NodeFact, p, NodeFact, np, bh, nbh, EdgeSimilarTo)
		if _, err := conn(ctx, si.rh.db).ExecContext(ctx, q); err != nil {
			return fmt.Errorf("create SIMILAR_TO %s→%s: %w", path, n.path, err)
		}
	}
	return nil
}

// ClusterResult holds the output of ClusterFacts.
type ClusterResult struct {
	Clusters map[int][]string // community ID → fact paths
	Noise    []string         // fact paths in communities below minCommunitySize
}

// ClusterFacts runs Louvain community detection on the full graph and returns
// community assignments for non-deleted Fact nodes.
//
// resolution controls Louvain granularity: higher = more, smaller communities.
// minCommunitySize: communities smaller than this are relabeled as noise.
func (si *searchIndex) ClusterFacts(ctx context.Context, branch string, resolution float64, minCommunitySize int) (ClusterResult, error) {
	if minCommunitySize <= 0 {
		minCommunitySize = 2
	}

	// GraphQLite's louvain() returns a single JSON string of the form:
	//   [{"column_0": [{"node_id": N, "user_id": null, "community": N}, ...]}]
	//
	// We use a SQL CTE to:
	//   1. Unpack the nested array via json_each.
	//   2. Join node_labels to keep only Fact nodes.
	//   3. Join node_props_text to resolve node_id → fact path.
	//
	// The property_keys table maps key names to integer IDs; we use a subquery
	// to find the key_id for "path" rather than hardcoding the integer.
	query := fmt.Sprintf(`
		WITH louvain_raw AS (
			SELECT
				CAST(json_extract(item.value, '$.node_id') AS INTEGER) AS node_id,
				CAST(json_extract(item.value, '$.community') AS INTEGER) AS community
			FROM (SELECT cypher('RETURN louvain(%f)') AS result) r,
			json_each(json_extract(r.result, '$[0].column_0')) item
		)
		SELECT lr.community, npt.value AS path
		FROM louvain_raw lr
		JOIN node_labels nl ON nl.node_id = lr.node_id AND nl.label = '%s'
		JOIN node_props_text npt ON npt.node_id = lr.node_id
			AND npt.key_id = (SELECT id FROM property_keys WHERE key = 'path' LIMIT 1)
	`, resolution, NodeFact)

	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, query)
	if err != nil {
		return ClusterResult{}, fmt.Errorf("louvain: %w", err)
	}
	defer rows.Close()

	communities := map[int][]string{}
	for rows.Next() {
		var community int
		var path string
		if err := rows.Scan(&community, &path); err != nil {
			continue
		}
		communities[community] = append(communities[community], path)
	}
	if err := rows.Err(); err != nil {
		return ClusterResult{}, fmt.Errorf("louvain rows: %w", err)
	}

	// Resolve branch to branchID for scoped filtering.
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return ClusterResult{}, fmt.Errorf("ClusterFacts: %w", err)
	}

	// Post-filter: exclude facts not visible on this branch (check existence
	// in `branch_facts` table), then apply minCommunitySize.
	allPaths := make([]string, 0)
	for _, members := range communities {
		allPaths = append(allPaths, members...)
	}
	existingPaths := make(map[string]bool, len(allPaths))
	if len(allPaths) > 0 {
		placeholders := make([]string, len(allPaths))
		args := make([]interface{}, len(allPaths))
		for i, p := range allPaths {
			placeholders[i] = "?"
			args[i] = p
		}
		qry := `SELECT path FROM branch_facts WHERE branch_id = ? AND path IN (` + strings.Join(placeholders, ",") + `)`
		args = append([]interface{}{branchID}, args...)
		eRows, err := conn(ctx, si.rh.db).QueryContext(ctx, qry, args...)
		if err == nil {
			for eRows.Next() {
				var p string
				if eRows.Scan(&p) == nil {
					existingPaths[p] = true
				}
			}
			eRows.Close()
		}
	}

	result := ClusterResult{Clusters: map[int][]string{}}
	for id, members := range communities {
		var alive []string
		for _, path := range members {
			if existingPaths[path] {
				alive = append(alive, path)
			}
		}
		if len(alive) < minCommunitySize {
			result.Noise = append(result.Noise, alive...)
		} else {
			result.Clusters[id] = alive
		}
	}

	return result, nil
}

// graphExpandSearch expands vector search seed results through graph traversal.
// Returns additional fact paths with scores. Graph-discovered facts receive a
// bonus score that decreases with hop distance but is capped below the minimum
// vector seed score to never outrank direct vector hits.
//
// Runs exactly 2 Cypher queries total (one per edge type) regardless of how
// many seeds are provided, using OR-chaining to batch all seed paths.
func (si *searchIndex) graphExpandSearch(ctx context.Context, branchID int64, seeds map[string]float64, maxHops int) map[string]float64 {
	if len(seeds) == 0 {
		return nil
	}

	expanded := map[string]float64{}

	// Find minimum seed score for capping.
	minSeedScore := 1.0
	for _, score := range seeds {
		if score < minSeedScore {
			minSeedScore = score
		}
	}
	capScore := minSeedScore - 0.01

	// Build Cypher path filter using OR-chaining. Each path is JSON-encoded to
	// produce a properly-quoted and escaped Cypher string literal. Parameterized
	// queries are not used here because they do not support variadic OR patterns.
	pathParts := make([]string, 0, len(seeds))
	for p := range seeds {
		b, _ := json.Marshal(p)
		pathParts = append(pathParts, `f.path = `+string(b))
	}
	pathFilter := strings.Join(pathParts, " OR ")

	// Batch query 1: SIMILAR_TO neighbors for all seeds.
	// Use NOT neighbor.deleted = true (instead of = false) because GraphQLite
	// stores booleans as JSON booleans which do not compare equal to Cypher
	// literal false. Nodes that are not deleted have deleted=false (set in
	// graphSyncFact) so this filter correctly excludes soft-deleted nodes.
	q := fmt.Sprintf(
		`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:%s)-[:%s]-(neighbor:%s) WHERE (%s) AND NOT neighbor.deleted = true RETURN DISTINCT neighbor.path AS path'))`,
		NodeFact, EdgeSimilarTo, NodeFact, pathFilter,
	)
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, q)
	if err == nil {
		for rows.Next() {
			var neighborPath string
			rows.Scan(&neighborPath)
			if neighborPath == "" {
				continue
			}
			if _, isSeed := seeds[neighborPath]; !isSeed {
				if existing, ok := expanded[neighborPath]; !ok || capScore > existing {
					expanded[neighborPath] = capScore
				}
			}
		}
		rows.Close()
	}

	// Batch query 2: shared-entity neighbors for all seeds.
	q = fmt.Sprintf(
		`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:%s)-[:%s]->(e:%s)<-[:%s]-(neighbor:%s) WHERE (%s) AND NOT neighbor.deleted = true RETURN DISTINCT neighbor.path AS path'))`,
		NodeFact, EdgeTagged, NodeEntity, EdgeTagged, NodeFact,
		pathFilter,
	)
	rows, err = conn(ctx, si.rh.db).QueryContext(ctx, q)
	if err == nil {
		for rows.Next() {
			var neighborPath string
			rows.Scan(&neighborPath)
			if neighborPath == "" {
				continue
			}
			if _, isSeed := seeds[neighborPath]; !isSeed {
				score := capScore - 0.01
				if existing, ok := expanded[neighborPath]; !ok || score > existing {
					expanded[neighborPath] = score
				}
			}
		}
		rows.Close()
	}

	// Post-filter: keep only paths visible on this branch.
	if len(expanded) > 0 {
		placeholders := make([]string, 0, len(expanded))
		args := make([]interface{}, 0, len(expanded)+1)
		args = append(args, branchID)
		for p := range expanded {
			placeholders = append(placeholders, "?")
			args = append(args, p)
		}
		visible := make(map[string]bool, len(expanded))
		rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
			`SELECT path FROM branch_facts WHERE branch_id = ? AND path IN (`+strings.Join(placeholders, ",")+`)`,
			args...,
		)
		if err == nil {
			for rows.Next() {
				var p string
				rows.Scan(&p)
				visible[p] = true
			}
			rows.Close()
		}
		for p := range expanded {
			if !visible[p] {
				delete(expanded, p)
			}
		}
	}

	return expanded
}

// execer abstracts *sql.DB and *sql.Tx for transactional graph operations.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

// jsonParams encodes a single key-value pair as a JSON object string, for use
// as the second argument to cypher() in parameterized read queries.
// For multiple params, use json.Marshal directly.
func jsonParams(key, value string) string {
	b, _ := json.Marshal(map[string]string{key: value})
	return string(b)
}

// escapeCypherKey escapes a string for use in Cypher MATCH/MERGE property
// patterns (e.g. {path: "value"}) that appear inside a SQL single-quoted string.
// GraphQLite's MATCH parser does not support unicode escapes or SQL '' escaping
// inside property patterns, so single quotes are stripped to avoid breaking the
// SQL string wrapper. Null bytes are stripped as they break the SQL parser.
//
// Note: parameterized queries (cypher('...', params)) work for reads (MATCH)
// but not for writes (MERGE/SET/DELETE) in the installed GraphQLite build, so
// write operations must use this escape approach.
func escapeCypherKey(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, `'`, ``)
	return s
}

// escapeCypherVal escapes a string for use in Cypher SET values
// (e.g. SET f.title = "value"). These are more lenient than MATCH patterns
// and support \u unicode escapes, so single quotes become \u0027.
func escapeCypherVal(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, `'`, `\u0027`)
	return s
}

// ── History graph phase ───────────────────────────────────────────────────────
// History graph phase: creates FactVersion nodes from commit_log entries,
// linking them with PREV_VERSION chains and DERIVED_FROM edges.
//
// GraphQLite limitation: MATCH (a:L {p1: "x"}), (b:L {p1: "y"}) does not
// correctly find two distinct nodes of the same label by property values — it
// degenerates into a self-loop (a)-[:R]->(a). To work around this, PREV_VERSION
// and DERIVED_FROM edges are created via direct SQL INSERT INTO edges after
// looking up node IDs through the EAV property tables.

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
// the GraphQLite Cypher layer. This avoids the two-node MATCH self-loop bug.
func (si *searchIndex) graphInsertEdge(ctx context.Context, sourceID, targetID int64, edgeType string) error {
	_, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`INSERT OR IGNORE INTO edges (source_id, target_id, type) VALUES (?, ?, ?)`,
		sourceID, targetID, edgeType,
	)
	return err
}

// graphSyncFactVersionTx creates a FactVersion node (MERGE only) within the
// given transaction. Properties (title, committed_at) must be set after the
// transaction commits via graphSetFactVersionProps, because GraphQLite's
// MATCH+SET does not persist to EAV tables when executed inside a *sql.Tx.
func (si *searchIndex) graphSyncFactVersionTx(ctx context.Context, tx execer, commitHash string, rec FactRecord, committedAt int64) error {
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
func (si *searchIndex) graphSetFactVersionProps(ctx context.Context, commitHash string, rec FactRecord, committedAt int64) error {
	nodeID, err := si.graphNodeIDByProp(ctx, NodeFactVersion, "commit_hash", commitHash)
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
		if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `INSERT OR IGNORE INTO property_keys(key) VALUES (?)`, p.key); err != nil {
			return fmt.Errorf("graphSetFactVersionProps: ensure key %s: %w", p.key, err)
		}
		var keyID int64
		if err := conn(ctx, si.rh.db).QueryRowContext(ctx, `SELECT id FROM property_keys WHERE key = ?`, p.key).Scan(&keyID); err != nil {
			return fmt.Errorf("graphSetFactVersionProps: get key_id for %s: %w", p.key, err)
		}
		if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
			`INSERT OR REPLACE INTO node_props_text(node_id, key_id, value) VALUES (?, ?, ?)`,
			nodeID, keyID, p.value,
		); err != nil {
			return fmt.Errorf("graphSetFactVersionProps: set text prop %s: %w", p.key, err)
		}
	}

	// committed_at is an integer; store in node_props_real (GraphQLite uses REAL for numbers).
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `INSERT OR IGNORE INTO property_keys(key) VALUES (?)`, "committed_at"); err != nil {
		return fmt.Errorf("graphSetFactVersionProps: ensure key committed_at: %w", err)
	}
	var caKeyID int64
	if err := conn(ctx, si.rh.db).QueryRowContext(ctx, `SELECT id FROM property_keys WHERE key = 'committed_at'`).Scan(&caKeyID); err != nil {
		return fmt.Errorf("graphSetFactVersionProps: get key_id for committed_at: %w", err)
	}
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`INSERT OR REPLACE INTO node_props_real(node_id, key_id, value) VALUES (?, ?, ?)`,
		nodeID, caKeyID, committedAt,
	); err != nil {
		return fmt.Errorf("graphSetFactVersionProps: set committed_at: %w", err)
	}

	return nil
}
