package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

type searchIndex struct {
	rh *repoHandler
}

// casLastCommit atomically updates the last-commit watermark for a branch,
// but only if it currently equals prev. Returns true if the update succeeded.
func (si *searchIndex) casLastCommit(ctx context.Context, branch, prev, next string) (bool, error) {
	key := "last_commit:" + branch
	db := conn(ctx, si.rh.db)

	if prev == "" {
		// First sync: insert only if key doesn't exist.
		result, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)`, key, next)
		if err != nil {
			return false, err
		}
		n, _ := result.RowsAffected()
		return n > 0, nil
	}

	result, err := db.ExecContext(ctx,
		`UPDATE meta SET value = ? WHERE key = ? AND value = ?`, next, key, prev)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// setLastCommit stores the last processed commit hash in the meta table,
// scoped to the given branch.
func (si *searchIndex) setLastCommit(ctx context.Context, branch, hash string) error {
	key := "last_commit:" + branch
	_, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`,
		key, hash,
	)
	return err
}

// SyncWatermark returns the last processed commit hash for the given branch,
// or "" if not set.
func (si *searchIndex) SyncWatermark(ctx context.Context, branch string) (string, error) {
	key := "last_commit:" + branch
	var hash string
	err := conn(ctx, si.rh.db).QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return hash, nil
}

// ── Sync ──────────────────────────────────────────────────────────────────────
// Git sync: keeps the search index up to date with the git-backed fact store.
// Supports both full rebuilds (first run) and incremental updates (diffing
// since the last indexed commit).

// Sync brings the index up to date with the git store.
//
// Algorithm:
//  1. Read meta.last_commit.
//  2. If missing → full rebuild (ListAll, index everything).
//  3. If last_commit == HEAD → no-op.
//  4. Else → DiffFiles(last_commit), upsert added+modified, delete removed.
//  5. Update meta.last_commit = HEAD.
func (si *searchIndex) Sync(ctx context.Context, branch string) error {
	// Ensure the branch exists in the branches table.
	if _, err := si.rh.EnsureBranch(ctx, branch, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("sync: ensure branch: %w", err)
	}

	head, err := si.rh.HeadCommit(ctx, branch)
	if err != nil {
		return fmt.Errorf("sync: head commit: %w", err)
	}

	last, err := si.SyncWatermark(ctx, branch)
	if err != nil {
		return fmt.Errorf("sync: get last commit: %w", err)
	}

	if last == head {
		log.Debug().Str("head", head[:8]).Msg("index sync: already at HEAD, skipping")
		return nil
	}

	if last == "" {
		// Full rebuild: no previous commit recorded, so index every file.
		log.Info().Str("head", head[:8]).Msg("index sync: full rebuild (no previous commit)")
		paths, err := si.rh.ListAll(ctx, branch)
		if err != nil {
			return fmt.Errorf("sync: list all: %w", err)
		}
		for _, path := range paths {
			if err := si.indexFile(ctx, branch, path, head); err != nil {
				return err
			}
		}
		log.Info().Int("files", len(paths)).Msg("index sync: full rebuild complete")
	} else {
		// Incremental update: only process files changed since last_commit.
		added, modified, deleted, err := si.rh.DiffFiles(ctx, branch, last)
		if err != nil {
			return fmt.Errorf("sync: diff files: %w", err)
		}
		log.Debug().
			Str("from", last[:8]).Str("to", head[:8]).
			Int("added", len(added)).Int("modified", len(modified)).Int("deleted", len(deleted)).
			Msg("index sync: incremental update")
		for _, path := range append(added, modified...) {
			if err := si.indexFile(ctx, branch, path, head); err != nil {
				return err
			}
		}
		for _, path := range deleted {
			if err := si.delete(ctx, branch, path); err != nil {
				return fmt.Errorf("sync: delete %q: %w", path, err)
			}
		}
	}

	ok, err := si.casLastCommit(ctx, branch, last, head)
	if err != nil {
		return fmt.Errorf("sync cas: %w", err)
	}
	if !ok {
		log.Debug().Str("branch", branch).Msg("sync: CAS failed, another sync won")
	}
	return nil
}

// RebuildProgress is called during Rebuild to report progress.
type RebuildProgress func(phase string, done, total int)

// Rebuild clears the last-commit marker and re-indexes every file from HEAD
// using three phases: facts, embeddings, graph.
func (si *searchIndex) Rebuild(ctx context.Context, branch string, progress RebuildProgress) error {
	if err := si.setLastCommit(ctx, branch, ""); err != nil {
		return fmt.Errorf("rebuild: clear last commit: %w", err)
	}

	head, err := si.rh.HeadCommit(ctx, branch)
	if err != nil {
		return fmt.Errorf("rebuild: head commit: %w", err)
	}

	// Phase 1: facts
	start := time.Now()
	n, err := si.rebuildFacts(ctx, branch, head, progress)
	if err != nil {
		return fmt.Errorf("rebuild: facts: %w", err)
	}
	log.Info().Int("facts", n).Str("elapsed", fmt.Sprintf("%.1fs", time.Since(start).Seconds())).Msg("rebuild: phase 1 (facts) complete")

	// Phase 2: embeddings
	start = time.Now()
	embedded, err := si.rebuildEmbeddings(ctx, progress)
	if err != nil {
		return fmt.Errorf("rebuild: embeddings: %w", err)
	}
	log.Info().Int("embedded", embedded).Str("elapsed", fmt.Sprintf("%.1fs", time.Since(start).Seconds())).Msg("rebuild: phase 2 (embeddings) complete")

	// Phase 3: graph
	start = time.Now()
	graphed, err := si.rebuildGraph(ctx, progress)
	if err != nil {
		return fmt.Errorf("rebuild: graph: %w", err)
	}
	log.Info().Int("graphed", graphed).Str("elapsed", fmt.Sprintf("%.1fs", time.Since(start).Seconds())).Msg("rebuild: phase 3 (graph) complete")

	// Phase 4: history (FactVersion nodes from commit_log)
	start = time.Now()
	versioned, err := si.rebuildGraphHistory(ctx, branch, progress)
	if err != nil {
		return fmt.Errorf("rebuild: history: %w", err)
	}
	log.Info().Int("versions", versioned).Str("elapsed", fmt.Sprintf("%.1fs", time.Since(start).Seconds())).Msg("rebuild: phase 4 (history) complete")

	return si.setLastCommit(ctx, branch, head)
}

// indexFile reads a single file from git, parses it as a fact, and upserts
// it into the index. Files that fail to parse (e.g. kb.md manifest) are
// silently skipped.
//
// commitHash is the fallback; if commit_log has a more specific last-touch
// commit for this path, that is used instead.
func (si *searchIndex) indexFile(ctx context.Context, branch, path, commitHash string) error {
	content, blobHash, err := si.rh.readFileWithHash(ctx, branch, path)
	if err != nil {
		return fmt.Errorf("indexFile: read %s: %w", path, err)
	}

	// Use the most recent non-merge commit that touched this file.
	if last, lerr := si.rh.LastCommitForPath(ctx, branch, path); lerr == nil && last != "" {
		commitHash = last
	}

	rec, err := parseFact(path, content)
	if err != nil {
		return nil // not a fact file (e.g. kb.md manifest, ontology.yaml)
	}
	rec.BlobHash = blobHash

	return si.upsert(ctx, branch, commitHash, rec)
}

// ── GC ────────────────────────────────────────────────────────────────────────

// GC removes orphaned data: facts not referenced by any branch, their graph
// nodes, orphaned Entity/Domain/OntologyNode graph nodes, and commit_log
// entries for deleted branches.
func (si *searchIndex) GC(ctx context.Context) error {
	// 1. Collect orphaned facts before deleting (needed for graph cleanup).
	type orphanFact struct {
		id       int64
		path     string
		blobHash string
	}
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
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
			if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `DELETE FROM facts WHERE id = ?`, o.id); err != nil {
				return fmt.Errorf("gc: delete fact %d: %w", o.id, err)
			}
		}

		// 2. Clean up graph Fact nodes for orphaned fact versions.
		for _, o := range orphans {
			if err := si.graphDeleteFact(ctx, o.path, o.blobHash); err != nil {
				log.Warn().Err(err).Str("path", o.path).Msg("gc: graph delete fact failed")
			}
		}
	}

	// 3. Clean up orphaned Entity nodes (no TAGGED edges from any living Fact).
	si.gcOrphanedGraphNodes(ctx, NodeEntity, EdgeTagged)

	// 4. Clean up orphaned Domain nodes (no IN_DOMAIN edges from any living Fact).
	si.gcOrphanedGraphNodes(ctx, NodeDomain, EdgeInDomain)

	// 5. Clean up orphaned OntologyNode nodes (no UNDER edges from any living Fact).
	si.gcOrphanedGraphNodes(ctx, NodeOntologyNode, EdgeUnder)

	// 6. Delete commit_log entries for deleted branches.
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`DELETE FROM commit_log WHERE branch_id IS NOT NULL AND branch_id NOT IN (SELECT id FROM branches)`,
	); err != nil {
		return fmt.Errorf("gc: clean commit_log: %w", err)
	}

	// 7. GC tool sessions: keep 5 most recent per (tool, branch).
	if err := si.gcSessionTable(ctx, "tool_sessions"); err != nil {
		return fmt.Errorf("gc: tool sessions: %w", err)
	}

	// 8. GC pipeline sessions: keep 5 most recent per (tool, branch).
	if err := si.gcSessionTable(ctx, "pipeline_sessions"); err != nil {
		return fmt.Errorf("gc: pipeline sessions: %w", err)
	}

	return nil
}

// gcSessionTable deletes all but the 5 most recent sessions per (tool, branch)
// from the given table. Child rows are cascade-deleted via foreign keys.
func (si *searchIndex) gcSessionTable(ctx context.Context, table string) error {
	_, err := conn(ctx, si.rh.db).ExecContext(ctx,
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
func (si *searchIndex) gcOrphanedGraphNodes(ctx context.Context, label, edgeType string) {
	q := fmt.Sprintf(
		`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (n:%s) WHERE NOT (:%s)-[:%s]->(n) RETURN n.path AS path'))`,
		label, NodeFact, edgeType,
	)
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, q)
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
		if _, err := conn(ctx, si.rh.db).ExecContext(ctx, delQ); err != nil {
			log.Warn().Err(err).Str("label", label).Str("path", path).Msg("gc: delete orphaned node failed")
		}
	}
}

// ── Rebuild ───────────────────────────────────────────────────────────────────
// Three-phase rebuild: facts, embeddings, graph.
// Each phase runs in a single transaction for efficiency.

// rebuildFacts bulk-inserts facts via SQL JOIN with knomit_parse_fact(),
// then populates branch_facts for the given branch.
func (si *searchIndex) rebuildFacts(ctx context.Context, branch, head string, progress RebuildProgress) (int, error) {
	branchID, err := si.rh.EnsureBranch(ctx, branch, "refs/heads/"+branch)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: ensure branch: %w", err)
	}

	paths, hashes, err := si.rh.ListAllWithHash(ctx, branch)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: list all: %w", err)
	}

	if progress != nil {
		progress("facts", 0, len(paths))
	}

	// Create temp table for the rebuild entries.
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS _rebuild_entries (path TEXT PRIMARY KEY, blob_hash TEXT NOT NULL)`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: create temp table: %w", err)
	}
	// Ensure it's empty (in case of prior incomplete rebuild).
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `DELETE FROM _rebuild_entries`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear temp table: %w", err)
	}

	// Bulk-insert all (path, blobHash) pairs in a single transaction.
	tx, err := si.rh.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: begin insert tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO _rebuild_entries(path, blob_hash) VALUES (?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: prepare insert: %w", err)
	}
	defer stmt.Close()

	for i := range paths {
		if _, err := stmt.ExecContext(ctx, paths[i], hashes[i]); err != nil {
			return 0, fmt.Errorf("rebuildFacts: insert entry %s: %w", paths[i], err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rebuildFacts: commit entries: %w", err)
	}

	// Bulk INSERT OR REPLACE facts from parsed blob data (no commit_hash in facts table).
	res, err := conn(ctx, si.rh.db).ExecContext(ctx, `
		WITH parsed_entries AS (
			SELECT e.path, e.blob_hash, knomit_parse_fact(o.data) AS parsed
			FROM _rebuild_entries e
			JOIN objects o ON o.hash = e.blob_hash AND o.type = ?
		)
		INSERT OR REPLACE INTO facts (path, blob_hash, title, type, domain, entities, confidence, sources, refs, evidence_weight)
		SELECT
			pe.path,
			pe.blob_hash,
			json_extract(pe.parsed, '$.title'),
			json_extract(pe.parsed, '$.type'),
			json_extract(pe.parsed, '$.domain'),
			json_extract(pe.parsed, '$.entities'),
			json_extract(pe.parsed, '$.confidence'),
			json_extract(pe.parsed, '$.sources'),
			json_extract(pe.parsed, '$.refs'),
			COALESCE(json_extract(pe.parsed, '$.evidence_weight'), 0)
		FROM parsed_entries pe
		WHERE pe.parsed IS NOT NULL
	`, blobObjectType)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: bulk insert: %w", err)
	}

	affected, _ := res.RowsAffected()
	n := int(affected)

	// Populate branch_facts: link each fact to this branch with its commit_hash.
	// Prefer commit_log entries scoped to this branch; fall back to legacy rows
	// with NULL branch_id (written before the branch existed in the branches table).
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `
		INSERT OR REPLACE INTO branch_facts (branch_id, path, fact_id, commit_hash)
		SELECT ?, f.path, f.id, COALESCE(cl.commit_hash, '')
		FROM facts f
		JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		LEFT JOIN (
			SELECT path, commit_hash, ROW_NUMBER() OVER (PARTITION BY path ORDER BY (branch_id = ?) DESC, committed_at DESC) AS rn
			FROM commit_log
			WHERE branch_id = ? OR branch_id IS NULL
		) cl ON cl.path = f.path AND cl.rn = 1
	`, branchID, branchID, branchID); err != nil {
		return 0, fmt.Errorf("rebuildFacts: populate branch_facts: %w", err)
	}

	// Clean up temp table.
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `DROP TABLE IF EXISTS _rebuild_entries`); err != nil {
		log.Warn().Err(err).Msg("rebuildFacts: drop temp table")
	}

	if progress != nil {
		progress("facts", n, n)
	}

	return n, nil
}

// rebuildEmbeddings computes embeddings for all facts missing from facts_vec.
// Bodies are fetched one chunk at a time so memory usage is bounded by batchSize,
// not by the total number of facts.
func (si *searchIndex) rebuildEmbeddings(ctx context.Context, progress RebuildProgress) (int, error) {
	emb := si.rh.getEmbedder()
	if emb == nil {
		return 0, nil
	}

	batcher, hasBatch := emb.(BatchEmbedder)

	var total int
	if err := conn(ctx, si.rh.db).QueryRowContext(ctx, `SELECT COUNT(*) FROM facts WHERE rowid NOT IN (SELECT rowid FROM facts_vec)`).Scan(&total); err != nil {
		return 0, fmt.Errorf("rebuildEmbeddings: count: %w", err)
	}

	if total == 0 {
		return 0, nil
	}

	if progress != nil {
		progress("embeddings", 0, total)
	}

	// Phase 1: collect only rowids and paths — no body data.
	type rowMeta struct {
		rowid int64
		path  string
	}
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, `SELECT rowid, path FROM facts WHERE rowid NOT IN (SELECT rowid FROM facts_vec)`)
	if err != nil {
		return 0, fmt.Errorf("rebuildEmbeddings: query rowids: %w", err)
	}
	var metas []rowMeta
	for rows.Next() {
		var m rowMeta
		if err := rows.Scan(&m.rowid, &m.path); err != nil {
			rows.Close()
			return 0, fmt.Errorf("rebuildEmbeddings: scan rowid: %w", err)
		}
		metas = append(metas, m)
	}
	rows.Close()

	// Phase 2: process in chunks — fetch bodies only for the current chunk.
	const batchSize = 32
	done := 0

	type entry struct {
		rowid int64
		path  string
		text  string
	}

	for i := 0; i < len(metas); i += batchSize {
		end := i + batchSize
		if end > len(metas) {
			end = len(metas)
		}
		chunk := metas[i:end]

		// Build IN clause for this chunk's rowids.
		args := make([]any, len(chunk))
		placeholders := make([]byte, 0, len(chunk)*2)
		for j, m := range chunk {
			args[j] = m.rowid
			if j > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
		}
		q := `SELECT f.rowid, f.path, f.title, o.data
			FROM facts f
			JOIN objects o ON o.hash = f.blob_hash AND o.type = ?
			WHERE f.rowid IN (` + string(placeholders) + `)`
		qargs := append([]any{blobObjectType}, args...)

		bodyRows, err := conn(ctx, si.rh.db).QueryContext(ctx, q, qargs...)
		if err != nil {
			return done, fmt.Errorf("rebuildEmbeddings: query bodies: %w", err)
		}
		var entries []entry
		for bodyRows.Next() {
			var e entry
			var title string
			var data []byte
			if err := bodyRows.Scan(&e.rowid, &e.path, &title, &data); err != nil {
				bodyRows.Close()
				return done, fmt.Errorf("rebuildEmbeddings: scan body: %w", err)
			}
			e.text = title + " " + extractBody(data)
			entries = append(entries, e)
		}
		bodyRows.Close()

		if len(entries) == 0 {
			continue
		}

		tx, err := si.rh.db.BeginTx(ctx, nil)
		if err != nil {
			return done, fmt.Errorf("rebuildEmbeddings: begin tx: %w", err)
		}

		if hasBatch {
			texts := make([]string, len(entries))
			for j, e := range entries {
				texts[j] = e.text
			}
			vecs, err := batcher.EmbedBatch(texts)
			if err != nil {
				tx.Rollback()
				return done, fmt.Errorf("rebuildEmbeddings: embed batch: %w", err)
			}
			for j, vec := range vecs {
				if len(vec) == 0 {
					continue
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO facts_vec(rowid, embedding) VALUES (?, ?)`,
					entries[j].rowid, float32SliceToBytes(vec)); err != nil {
					tx.Rollback()
					return done, fmt.Errorf("rebuildEmbeddings: insert vec %s: %w", entries[j].path, err)
				}
				done++
			}
		} else {
			for _, e := range entries {
				vec, err := emb.Embed(e.text)
				if err != nil {
					log.Warn().Err(err).Str("path", e.path).Msg("rebuildEmbeddings: embed failed, skipping")
					continue
				}
				if len(vec) == 0 {
					continue
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO facts_vec(rowid, embedding) VALUES (?, ?)`,
					e.rowid, float32SliceToBytes(vec)); err != nil {
					tx.Rollback()
					return done, fmt.Errorf("rebuildEmbeddings: insert vec %s: %w", e.path, err)
				}
				done++
			}
		}

		if err := tx.Commit(); err != nil {
			return done, fmt.Errorf("rebuildEmbeddings: commit chunk: %w", err)
		}

		if progress != nil {
			progress("embeddings", done, total)
		}
	}

	return done, nil
}

// rebuildGraph syncs graph nodes/edges for all facts in a single transaction,
// then builds similarity edges after commit.
func (si *searchIndex) rebuildGraph(ctx context.Context, progress RebuildProgress) (int, error) {
	// Read all facts ordered by oldest commit first so that when a fact's
	// DERIVED_FROM edges are created, its ref targets are already graph nodes.
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, `
		SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities, f.confidence, f.sources, f.refs, f.evidence_weight
		FROM facts f
		LEFT JOIN (
			SELECT path, MIN(committed_at) AS first_committed FROM commit_log GROUP BY path
		) cl ON cl.path = f.path
		ORDER BY cl.first_committed ASC`)
	if err != nil {
		return 0, fmt.Errorf("rebuildGraph: query facts: %w", err)
	}

	var facts []FactRecord
	for rows.Next() {
		var rec FactRecord
		var domainJSON, entitiesJSON, refsJSON string
		if err := rows.Scan(&rec.Path, &rec.Title, &rec.BlobHash, &rec.Type,
			&domainJSON, &entitiesJSON, &rec.Confidence, &rec.Sources,
			&refsJSON, &rec.EvidenceWeight); err != nil {
			rows.Close()
			return 0, fmt.Errorf("rebuildGraph: scan: %w", err)
		}
		json.Unmarshal([]byte(domainJSON), &rec.Domain)
		json.Unmarshal([]byte(entitiesJSON), &rec.Entities)
		json.Unmarshal([]byte(refsJSON), &rec.Refs)
		facts = append(facts, rec)
	}
	rows.Close()

	total := len(facts)
	if progress != nil {
		progress("graph", 0, total)
	}

	// Single transaction for all graph sync operations.
	tx, err := si.rh.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rebuildGraph: begin tx: %w", err)
	}
	defer tx.Rollback()

	for i, rec := range facts {
		if err := si.graphSyncFactTx(ctx, tx, rec); err != nil {
			log.Warn().Err(err).Str("path", rec.Path).Msg("rebuildGraph: sync failed, skipping")
			continue
		}
		if progress != nil && (i%10 == 0 || i+1 == total) {
			progress("graph", i+1, total)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rebuildGraph: commit: %w", err)
	}

	// Build similarity edges after commit (needs committed data for KNN).
	// Batch: collect all neighbors first, then write all edges in one transaction.
	if si.rh.getEmbedder() != nil {
		type simEdge struct{ fromPath, fromBH, toPath, toBH string }
		var edges []simEdge

		for _, rec := range facts {
			emb, err := si.getEmbeddingByFact(ctx, rec.Path, rec.BlobHash)
			if err != nil || emb == nil {
				continue
			}
			vecBlob := float32SliceToBytes(emb)
			rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
				`SELECT f.path, f.blob_hash, (1.0 - fv.distance) as similarity
				 FROM facts_vec fv
				 JOIN facts f ON f.id = fv.rowid
				 WHERE fv.embedding MATCH ? AND fv.k = ?
				 ORDER BY fv.distance ASC`,
				vecBlob, knnK+1,
			)
			if err != nil {
				log.Warn().Err(err).Str("path", rec.Path).Msg("rebuildGraph: knn query failed")
				continue
			}
			for rows.Next() {
				var neighborPath, neighborBH string
				var sim float64
				if err := rows.Scan(&neighborPath, &neighborBH, &sim); err != nil {
					break
				}
				if (neighborPath != rec.Path || neighborBH != rec.BlobHash) && sim >= knnThreshold {
					edges = append(edges, simEdge{fromPath: rec.Path, fromBH: rec.BlobHash, toPath: neighborPath, toBH: neighborBH})
				}
			}
			rows.Close()
		}

		// Batch-write all similarity edges in a single transaction.
		if len(edges) > 0 {
			simTx, err := si.rh.db.BeginTx(ctx, nil)
			if err != nil {
				log.Warn().Err(err).Msg("rebuildGraph: begin similarity tx")
			} else {
				for _, e := range edges {
					fp := escapeCypherKey(e.fromPath)
					fbh := escapeCypherKey(e.fromBH)
					tp := escapeCypherKey(e.toPath)
					tbh := escapeCypherKey(e.toBH)
					q := fmt.Sprintf(`SELECT cypher('MATCH (a:%s {path: "%s"}), (b:%s {path: "%s"}) WHERE a.blob_hash = "%s" AND b.blob_hash = "%s" MERGE (a)-[:%s]->(b)')`, NodeFact, fp, NodeFact, tp, fbh, tbh, EdgeSimilarTo)
					if _, err := simTx.ExecContext(ctx, q); err != nil {
						log.Warn().Err(err).Str("from", e.fromPath).Str("to", e.toPath).Msg("rebuildGraph: similarity edge failed")
					}
				}
				if err := simTx.Commit(); err != nil {
					log.Warn().Err(err).Msg("rebuildGraph: commit similarity tx")
				}
			}
		}
	}

	return total, nil
}

// rebuildGraphHistory creates FactVersion nodes for every (path, commit_hash)
// row in commit_log. Versions of the same path are chained newest→oldest via
// PREV_VERSION. Each version's refs (local paths only) get DERIVED_FROM edges
// to the corresponding Fact node. Deleted entries are skipped.
//
// Returns the number of FactVersion nodes successfully created.
func (si *searchIndex) rebuildGraphHistory(ctx context.Context, branch string, progress RebuildProgress) (int, error) {
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, `
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

	tx, err := si.rh.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rebuildGraphHistory: begin tx: %w", err)
	}
	defer tx.Rollback()

	done := 0
	prevByPath := make(map[string]string)         // path → previous commit_hash
	prevEdgesByPath := make(map[string][]prevEdge) // path → edges to create
	var created []createdVersion

	for _, v := range versions {
		content, err := si.rh.readFileAtCommit(ctx, branch, v.path, v.commitHash)
		if err != nil {
			log.Debug().Err(err).Str("path", v.path).Str("commit", v.commitHash[:8]).Msg("rebuildGraphHistory: skip (file not found at commit)")
			continue
		}

		rec, err := parseFact(v.path, content)
		if err != nil {
			log.Debug().Err(err).Str("path", v.path).Msg("rebuildGraphHistory: skip (parse failed)")
			continue
		}

		if err := si.graphSyncFactVersionTx(ctx, tx, v.commitHash, rec, v.committedAt); err != nil {
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
		if err := si.graphSetFactVersionProps(ctx, cv.commitHash, cv.rec, cv.committedAt); err != nil {
			log.Warn().Err(err).Str("path", cv.path).Str("commit", cv.commitHash[:8]).Msg("rebuildGraphHistory: set props failed")
		}
	}

	// Phase 2: create PREV_VERSION and DERIVED_FROM edges via direct SQL.
	// GraphQLite's two-node MATCH-MERGE pattern creates self-loops when both
	// nodes share the same label, so we look up node IDs directly and INSERT.
	for path, edges := range prevEdgesByPath {
		for _, e := range edges {
			newerID, err := si.graphNodeIDByProp(ctx, NodeFactVersion, "commit_hash", e.newerHash)
			if err != nil || newerID == 0 {
				log.Warn().Str("path", path).Str("commit", e.newerHash[:8]).Msg("rebuildGraphHistory: newer node not found for PREV_VERSION")
				continue
			}
			olderID, err := si.graphNodeIDByProp(ctx, NodeFactVersion, "commit_hash", e.olderHash)
			if err != nil || olderID == 0 {
				log.Warn().Str("path", path).Str("commit", e.olderHash[:8]).Msg("rebuildGraphHistory: older node not found for PREV_VERSION")
				continue
			}
			if err := si.graphInsertEdge(ctx, newerID, olderID, EdgePrevVersion); err != nil {
				log.Warn().Err(err).Str("path", path).Msg("rebuildGraphHistory: PREV_VERSION insert failed")
			}
		}
	}

	for _, cv := range created {
		versionID, err := si.graphNodeIDByProp(ctx, NodeFactVersion, "commit_hash", cv.commitHash)
		if err != nil || versionID == 0 {
			continue
		}
		for _, ref := range cv.refs {
			targetID, err := si.graphNodeIDByProp(ctx, NodeFact, "path", ref)
			if err != nil || targetID == 0 {
				// Target Fact node doesn't exist — skip (no self-loop risk with direct SQL).
				continue
			}
			if err := si.graphInsertEdge(ctx, versionID, targetID, EdgeDerivedFrom); err != nil {
				log.Warn().Err(err).Str("ref", ref).Msg("rebuildGraphHistory: DERIVED_FROM insert failed")
			}
		}
	}

	return done, nil
}
