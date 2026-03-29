package store

import (
	"fmt"

	"github.com/rs/zerolog/log"
)

// GC removes orphaned data: facts not referenced by any branch, their graph
// nodes, orphaned Entity/Domain/OntologyNode graph nodes, and commit_log
// entries for deleted branches.
func (idx *Index) GC() error {
	// 1. Collect orphaned fact paths before deleting (needed for graph cleanup).
	rows, err := idx.db.Query(
		`SELECT id, path FROM facts WHERE id NOT IN (SELECT fact_id FROM branch_facts)`,
	)
	if err != nil {
		return fmt.Errorf("gc: find orphans: %w", err)
	}
	var orphanPaths []string
	var orphanIDs []int64
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return fmt.Errorf("gc: scan orphan: %w", err)
		}
		orphanIDs = append(orphanIDs, id)
		orphanPaths = append(orphanPaths, path)
	}
	rows.Close()

	if len(orphanIDs) > 0 {
		// Delete orphaned facts (cascades to fact_entities, fact_domains, facts_vec via trigger).
		for _, id := range orphanIDs {
			if _, err := idx.db.Exec(`DELETE FROM facts WHERE id = ?`, id); err != nil {
				return fmt.Errorf("gc: delete fact %d: %w", id, err)
			}
		}

		// 2. Clean up graph Fact nodes for orphaned paths.
		for _, path := range orphanPaths {
			if err := idx.graphDeleteFact(path); err != nil {
				log.Warn().Err(err).Str("path", path).Msg("gc: graph delete fact failed")
			}
		}
	}

	// 3. Clean up orphaned Entity nodes (no TAGGED edges from any living Fact).
	idx.gcOrphanedGraphNodes(NodeEntity, EdgeTagged)

	// 4. Clean up orphaned Domain nodes (no IN_DOMAIN edges from any living Fact).
	idx.gcOrphanedGraphNodes(NodeDomain, EdgeInDomain)

	// 5. Clean up orphaned OntologyNode nodes (no UNDER edges from any living Fact).
	idx.gcOrphanedGraphNodes(NodeOntologyNode, EdgeUnder)

	// 6. Delete commit_log entries for deleted branches.
	if _, err := idx.db.Exec(
		`DELETE FROM commit_log WHERE branch_id IS NOT NULL AND branch_id NOT IN (SELECT id FROM branches)`,
	); err != nil {
		return fmt.Errorf("gc: clean commit_log: %w", err)
	}

	return nil
}

// gcOrphanedGraphNodes removes graph nodes of the given label that have no
// incoming edges of edgeType from any Fact node.
func (idx *Index) gcOrphanedGraphNodes(label, edgeType string) {
	q := fmt.Sprintf(
		`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (n:%s) WHERE NOT (:%s)-[:%s]->(n) RETURN n.path AS path'))`,
		label, NodeFact, edgeType,
	)
	rows, err := idx.db.Query(q)
	if err != nil {
		log.Warn().Err(err).Str("label", label).Msg("gc: query orphaned nodes failed")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil || path == "" {
			continue
		}
		ep := escapeCypherKey(path)
		delQ := fmt.Sprintf(`SELECT cypher('MATCH (n:%s {path: "%s"}) DETACH DELETE n')`, label, ep)
		if _, err := idx.db.Exec(delQ); err != nil {
			log.Warn().Err(err).Str("label", label).Str("path", path).Msg("gc: delete orphaned node failed")
		}
	}
}
