package store

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
)

// GC removes orphaned data: facts not referenced by any branch, their graph
// nodes, orphaned Entity/Domain/OntologyNode graph nodes, and commit_log
// entries for deleted branches.
func (idx *store) GC(ctx context.Context) error {
	// 1. Collect orphaned facts before deleting (needed for graph cleanup).
	type orphanFact struct {
		id       int64
		path     string
		blobHash string
	}
	rows, err := conn(ctx, idx.rh.db).QueryContext(ctx,
		`SELECT id, path, blob_hash FROM facts WHERE id NOT IN (SELECT fact_id FROM branch_facts)`,
	)
	if err != nil {
		return fmt.Errorf("gc: find orphans: %w", err)
	}
	var orphans []orphanFact
	for rows.Next() {
		var o orphanFact
		if err := rows.Scan(&o.id, &o.path, &o.blobHash); err != nil {
			rows.Close()
			return fmt.Errorf("gc: scan orphan: %w", err)
		}
		orphans = append(orphans, o)
	}
	rows.Close()

	if len(orphans) > 0 {
		// Delete orphaned facts (cascades to fact_entities, fact_domains, facts_vec via trigger).
		for _, o := range orphans {
			if _, err := conn(ctx, idx.rh.db).ExecContext(ctx, `DELETE FROM facts WHERE id = ?`, o.id); err != nil {
				return fmt.Errorf("gc: delete fact %d: %w", o.id, err)
			}
		}

		// 2. Clean up graph Fact nodes for orphaned fact versions.
		for _, o := range orphans {
			if err := idx.graphDeleteFact(ctx, o.path, o.blobHash); err != nil {
				log.Warn().Err(err).Str("path", o.path).Msg("gc: graph delete fact failed")
			}
		}
	}

	// 3. Clean up orphaned Entity nodes (no TAGGED edges from any living Fact).
	idx.gcOrphanedGraphNodes(ctx, NodeEntity, EdgeTagged)

	// 4. Clean up orphaned Domain nodes (no IN_DOMAIN edges from any living Fact).
	idx.gcOrphanedGraphNodes(ctx, NodeDomain, EdgeInDomain)

	// 5. Clean up orphaned OntologyNode nodes (no UNDER edges from any living Fact).
	idx.gcOrphanedGraphNodes(ctx, NodeOntologyNode, EdgeUnder)

	// 6. Delete commit_log entries for deleted branches.
	if _, err := conn(ctx, idx.rh.db).ExecContext(ctx,
		`DELETE FROM commit_log WHERE branch_id IS NOT NULL AND branch_id NOT IN (SELECT id FROM branches)`,
	); err != nil {
		return fmt.Errorf("gc: clean commit_log: %w", err)
	}

	// 7. GC tool sessions: keep 5 most recent per (tool, branch).
	if err := idx.gcSessionTable(ctx, "tool_sessions"); err != nil {
		return fmt.Errorf("gc: tool sessions: %w", err)
	}

	// 8. GC pipeline sessions: keep 5 most recent per (tool, branch).
	if err := idx.gcSessionTable(ctx, "pipeline_sessions"); err != nil {
		return fmt.Errorf("gc: pipeline sessions: %w", err)
	}

	return nil
}

// gcSessionTable deletes all but the 5 most recent sessions per (tool, branch)
// from the given table. Child rows are cascade-deleted via foreign keys.
func (idx *store) gcSessionTable(ctx context.Context, table string) error {
	_, err := conn(ctx, idx.rh.db).ExecContext(ctx,
		fmt.Sprintf(
			`DELETE FROM %s WHERE rowid NOT IN (
			    SELECT rowid FROM (
			        SELECT rowid, ROW_NUMBER() OVER (PARTITION BY tool, branch ORDER BY rowid DESC) AS rn
			        FROM %s
			    ) WHERE rn <= 5
			)`, table, table),
	)
	return err
}

// gcOrphanedGraphNodes removes graph nodes of the given label that have no
// incoming edges of edgeType from any Fact node.
func (idx *store) gcOrphanedGraphNodes(ctx context.Context, label, edgeType string) {
	q := fmt.Sprintf(
		`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (n:%s) WHERE NOT (:%s)-[:%s]->(n) RETURN n.path AS path'))`,
		label, NodeFact, edgeType,
	)
	rows, err := conn(ctx, idx.rh.db).QueryContext(ctx, q)
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
		if _, err := conn(ctx, idx.rh.db).ExecContext(ctx, delQ); err != nil {
			log.Warn().Err(err).Str("label", label).Str("path", path).Msg("gc: delete orphaned node failed")
		}
	}
}
