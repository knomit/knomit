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
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

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
func (idx *Index) graphSyncFact(rec FactRecord) error {
	return idx.graphSyncFactTx(idx.db, rec)
}

// graphSyncFactTx is the transactional version of graphSyncFact.
func (idx *Index) graphSyncFactTx(tx execer, rec FactRecord) error {
	path := escapeCypherKey(rec.Path)
	title := escapeCypherVal(rec.Title)

	// 1. MERGE Fact node, then SET properties in a separate statement.
	// GraphQLite does not apply SET clauses in the same MERGE statement; a
	// subsequent MATCH+SET is required to update properties reliably.
	q := fmt.Sprintf(`SELECT cypher('MERGE (f:%s {path: "%s"})')`, NodeFact, path)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph merge fact: %w", err)
	}
	q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}) SET f.title = "%s", f.user_id = "%s", f.confidence = %f, f.sources = %d, f.deleted = false, f.type = "%s"')`,
		NodeFact, path, title, path, rec.Confidence, rec.Sources, escapeCypherVal(rec.Type))
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph set fact props: %w", err)
	}

	// 2. Delete old relationship edges for this fact.
	for _, edgeType := range []string{EdgeTagged, EdgeInDomain, EdgeUnder, EdgeDerivedFrom} {
		q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"})-[r:%s]->() DELETE r')`, NodeFact, path, edgeType)
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
		q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}), (e:%s {name: "%s"}) MERGE (f)-[:%s]->(e)')`, NodeFact, path, NodeEntity, e, EdgeTagged)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph tagged %s: %w", entity, err)
		}
	}

	// 4. MERGE Domain hierarchy + IN_DOMAIN edges.
	for _, domain := range rec.Domain {
		if err := idx.graphMergeDomainHierarchy(tx, rec.Path, domain); err != nil {
			return err
		}
	}

	// 5. MERGE OntologyNode hierarchy + UNDER edge.
	if err := idx.graphMergeOntologyHierarchy(tx, rec.Path); err != nil {
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
		if err := idx.graphAddDerivedFromTx(tx, rec.Path, localRefs); err != nil {
			return fmt.Errorf("graph sync derived_from: %w", err)
		}
	}

	return nil
}

// graphMergeDomainHierarchy creates the full domain ancestor chain and links
// the fact to the leaf domain via IN_DOMAIN.
func (idx *Index) graphMergeDomainHierarchy(tx execer, factPath, domain string) error {
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
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}), (d:%s {path: "%s"}) MERGE (f)-[:%s]->(d)')`, NodeFact, escapeCypherKey(factPath), NodeDomain, leaf, EdgeInDomain)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph in_domain %s: %w", domain, err)
	}
	return nil
}

// graphMergeOntologyHierarchy creates OntologyNode chain from the fact's file
// path and links the fact to the leaf via UNDER.
func (idx *Index) graphMergeOntologyHierarchy(tx execer, factPath string) error {
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
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}), (o:%s {path: "%s"}) MERGE (f)-[:%s]->(o)')`, NodeFact, fp, NodeOntologyNode, leaf, EdgeUnder)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph under %s: %w", factPath, err)
	}
	return nil
}

// graphDeleteFact marks a Fact node as deleted and removes its outgoing edges
// (except incoming DERIVED_FROM, which preserves lineage).
func (idx *Index) graphDeleteFact(path string) error {
	return idx.graphDeleteFactTx(idx.db, path)
}

func (idx *Index) graphDeleteFactTx(tx execer, path string) error {
	p := escapeCypherKey(path)
	// Delete outgoing edges.
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"})-[r]->() DELETE r')`, NodeFact, p)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph delete outgoing edges: %w", err)
	}
	// Delete incoming SIMILAR_TO edges (bidirectional cleanup).
	q = fmt.Sprintf(`SELECT cypher('MATCH ()-[r:%s]->(f:%s {path: "%s"}) DELETE r')`, EdgeSimilarTo, NodeFact, p)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph delete incoming SIMILAR_TO: %w", err)
	}
	// Mark node as deleted.
	q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}) SET f.deleted = true')`, NodeFact, p)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph mark deleted: %w", err)
	}
	return nil
}

// graphAddDerivedFromTx creates DERIVED_FROM edges from a new fact to its source facts.
// GraphQLite bug: when the target node is absent, MATCH degenerates and MERGE creates
// a self-loop (n)-[:DERIVED_FROM]->(n). We accept this and filter self-loops at query
// time in ExplainFact instead of pre-checking (which would silently drop valid edges
// when facts are indexed in different orders during rebuild).
func (idx *Index) graphAddDerivedFromTx(tx execer, newPath string, sourcePaths []string) error {
	np := escapeCypherKey(newPath)
	for _, src := range sourcePaths {
		sp := escapeCypherKey(src)
		q := fmt.Sprintf(`SELECT cypher('MATCH (n:%s {path: "%s"}), (s:%s {path: "%s"}) MERGE (n)-[:%s]->(s)')`, NodeFact, np, NodeFact, sp, EdgeDerivedFrom)
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

// graphBuildSimilarityEdges creates SIMILAR_TO edges from a fact to its
// top-K nearest neighbors (by cosine similarity via sqlite-vec KNN).
// Edges below the similarity threshold are not created.
//
// IMPORTANT: This function queries sqlite-vec (facts_vec) directly via idx.db,
// so it must be called AFTER the surrounding transaction has committed.
// Calling it inside a transaction will not see uncommitted embedding writes.
func (idx *Index) graphBuildSimilarityEdges(path string) error {
	emb, err := idx.getEmbeddingByFactPath(path)
	if err != nil || emb == nil {
		return nil
	}

	vecBlob := float32SliceToBytes(emb)

	// Collect neighbors first, then close rows before running Cypher mutations.
	// Running Exec() on the same *sql.DB while rows are open can interfere in
	// SQLite's single-writer model.
	type neighbor struct {
		path       string
		similarity float64
	}
	rows, err := idx.db.Query(
		`SELECT f.path, (1.0 - fv.distance) as similarity
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
		if err := rows.Scan(&n.path, &n.similarity); err != nil {
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

	// Delete old outgoing SIMILAR_TO edges for this fact.
	p := escapeCypherKey(path)
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"})-[r:%s]->() DELETE r')`, NodeFact, p, EdgeSimilarTo)
	if _, err := idx.db.Exec(q); err != nil {
		return fmt.Errorf("delete old SIMILAR_TO: %w", err)
	}

	for _, n := range neighbors {
		if n.path == path {
			continue
		}
		if n.similarity < knnThreshold {
			continue
		}
		np := escapeCypherKey(n.path)
		q = fmt.Sprintf(`SELECT cypher('MATCH (a:%s {path: "%s"}), (b:%s {path: "%s"}) MERGE (a)-[:%s]->(b)')`, NodeFact, p, NodeFact, np, EdgeSimilarTo)
		if _, err := idx.db.Exec(q); err != nil {
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
func (idx *Index) ClusterFacts(branch string, resolution float64, minCommunitySize int) (ClusterResult, error) {
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

	rows, err := idx.db.Query(query)
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
	branchID, err := idx.BranchID(branch)
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
		eRows, err := idx.db.Query(qry, args...)
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
func (idx *Index) graphExpandSearch(branchID int64, seeds map[string]float64, maxHops int) map[string]float64 {
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
	rows, err := idx.db.Query(q)
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
	rows, err = idx.db.Query(q)
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
		rows, err := idx.db.Query(
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
