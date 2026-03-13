// Graph operations: Cypher wrappers for maintaining the knowledge graph.
// All graph mutations use MERGE for idempotency and string interpolation
// (cypher() does not support parameterized queries).
package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// graphSyncFact creates or updates graph nodes and edges for a fact.
// This implements the Learn mutation from the spec:
//  1. MERGE Fact node
//  2. Delete old TAGGED, IN_DOMAIN edges
//  3. MERGE Entity nodes + TAGGED edges
//  4. MERGE Domain hierarchy + IN_DOMAIN edges
//  5. MERGE OntologyNode hierarchy + UNDER edge
func (idx *Index) graphSyncFact(rec FactRecord) error {
	return idx.graphSyncFactTx(idx.db, rec)
}

// graphSyncFactTx is the transactional version of graphSyncFact.
func (idx *Index) graphSyncFactTx(tx execer, rec FactRecord) error {
	path := escapeCypher(rec.Path)
	title := escapeCypher(rec.Title)

	// 1. MERGE Fact node, then SET properties in a separate statement.
	// GraphQLite does not apply SET clauses in the same MERGE statement; a
	// subsequent MATCH+SET is required to update properties reliably.
	q := fmt.Sprintf(`SELECT cypher('MERGE (f:Fact {path: "%s"})')`, path)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph merge fact: %w", err)
	}
	q = fmt.Sprintf(`SELECT cypher('MATCH (f:Fact {path: "%s"}) SET f.title = "%s", f.user_id = "%s", f.confidence = %f, f.sources = %d, f.deleted = false')`,
		path, title, path, rec.Confidence, rec.Sources)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph set fact props: %w", err)
	}

	// 2. Delete old relationship edges for this fact
	for _, edgeType := range []string{"TAGGED", "IN_DOMAIN", "UNDER"} {
		q = fmt.Sprintf(`SELECT cypher('MATCH (f:Fact {path: "%s"})-[r:%s]->() DELETE r')`, path, edgeType)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph delete old %s edges: %w", edgeType, err)
		}
	}

	// 3. MERGE Entity nodes + TAGGED edges
	for _, entity := range rec.Entities {
		e := escapeCypher(entity)
		q = fmt.Sprintf(`SELECT cypher('MERGE (e:Entity {name: "%s"}) MERGE (f:Fact {path: "%s"}) MERGE (f)-[:TAGGED]->(e)')`, e, path)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph merge entity %s: %w", entity, err)
		}
	}

	// 4. MERGE Domain hierarchy + IN_DOMAIN edges
	for _, domain := range rec.Domain {
		if err := idx.graphMergeDomainHierarchy(tx, path, domain); err != nil {
			return err
		}
	}

	// 5. MERGE OntologyNode hierarchy + UNDER edge
	if err := idx.graphMergeOntologyHierarchy(tx, rec.Path); err != nil {
		return err
	}

	return nil
}

// graphMergeDomainHierarchy creates the full domain ancestor chain and links
// the fact to the leaf domain via IN_DOMAIN.
func (idx *Index) graphMergeDomainHierarchy(tx execer, factPath, domain string) error {
	parts := strings.Split(domain, "/")
	for i := range parts {
		seg := strings.Join(parts[:i+1], "/")
		escaped := escapeCypher(seg)
		q := fmt.Sprintf(`SELECT cypher('MERGE (:Domain {path: "%s"})')`, escaped)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph merge domain %s: %w", seg, err)
		}
		if i > 0 {
			parent := escapeCypher(strings.Join(parts[:i], "/"))
			q = fmt.Sprintf(`SELECT cypher('MATCH (c:Domain {path: "%s"}), (p:Domain {path: "%s"}) MERGE (c)-[:DOMAIN_CHILD_OF]->(p)')`, escaped, parent)
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("graph domain child_of %s: %w", seg, err)
			}
		}
	}
	leaf := escapeCypher(domain)
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:Fact {path: "%s"}), (d:Domain {path: "%s"}) MERGE (f)-[:IN_DOMAIN]->(d)')`, factPath, leaf)
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
		escaped := escapeCypher(seg)
		q := fmt.Sprintf(`SELECT cypher('MERGE (:OntologyNode {path: "%s"})')`, escaped)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph merge ontology %s: %w", seg, err)
		}
		if i > 0 {
			parent := escapeCypher(strings.Join(dirParts[:i], "/"))
			q = fmt.Sprintf(`SELECT cypher('MATCH (c:OntologyNode {path: "%s"}), (p:OntologyNode {path: "%s"}) MERGE (c)-[:ONTOLOGY_CHILD_OF]->(p)')`, escaped, parent)
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("graph ontology child_of %s: %w", seg, err)
			}
		}
	}
	leaf := escapeCypher(strings.Join(dirParts, "/"))
	fp := escapeCypher(factPath)
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:Fact {path: "%s"}), (o:OntologyNode {path: "%s"}) MERGE (f)-[:UNDER]->(o)')`, fp, leaf)
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
	p := escapeCypher(path)
	// Delete outgoing edges
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:Fact {path: "%s"})-[r]->() DELETE r')`, p)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph delete outgoing edges: %w", err)
	}
	// Delete incoming SIMILAR_TO edges (bidirectional cleanup)
	q = fmt.Sprintf(`SELECT cypher('MATCH ()-[r:SIMILAR_TO]->(f:Fact {path: "%s"}) DELETE r')`, p)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph delete incoming SIMILAR_TO: %w", err)
	}
	// Mark node as deleted
	q = fmt.Sprintf(`SELECT cypher('MATCH (f:Fact {path: "%s"}) SET f.deleted = true')`, p)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph mark deleted: %w", err)
	}
	return nil
}

// graphAddDerivedFrom creates DERIVED_FROM edges from a new fact to its source facts.
func (idx *Index) graphAddDerivedFrom(newPath string, sourcePaths []string) error {
	np := escapeCypher(newPath)
	for _, src := range sourcePaths {
		sp := escapeCypher(src)
		q := fmt.Sprintf(`SELECT cypher('MATCH (n:Fact {path: "%s"}), (s:Fact {path: "%s"}) MERGE (n)-[:DERIVED_FROM]->(s)')`, np, sp)
		if _, err := idx.db.Exec(q); err != nil {
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
	emb, err := idx.GetEmbedding(path)
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
		 JOIN facts f ON f.rowid = fv.rowid
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
	p := escapeCypher(path)
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:Fact {path: "%s"})-[r:SIMILAR_TO]->() DELETE r')`, p)
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
		np := escapeCypher(n.path)
		q = fmt.Sprintf(`SELECT cypher('MATCH (a:Fact {path: "%s"}), (b:Fact {path: "%s"}) MERGE (a)-[:SIMILAR_TO]->(b)')`, p, np)
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
func (idx *Index) ClusterFacts(resolution float64, minCommunitySize int) (ClusterResult, error) {
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
		JOIN node_labels nl ON nl.node_id = lr.node_id AND nl.label = 'Fact'
		JOIN node_props_text npt ON npt.node_id = lr.node_id
			AND npt.key_id = (SELECT id FROM property_keys WHERE key = 'path' LIMIT 1)
	`, resolution)

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

	// Post-filter: exclude deleted facts (check existence in `facts` table),
	// then apply minCommunitySize.
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
		qry := `SELECT path FROM facts WHERE path IN (` + strings.Join(placeholders, ",") + `)`
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

// execer abstracts *sql.DB and *sql.Tx for transactional graph operations.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

// escapeCypher escapes double quotes and backslashes in strings for Cypher interpolation.
func escapeCypher(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
