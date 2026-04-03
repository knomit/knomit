// Three-phase rebuild: facts, embeddings, graph.
// Each phase runs in a single transaction for efficiency.
package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog/log"
)

// rebuildFacts bulk-inserts facts via SQL JOIN with knomit_parse_fact(),
// then populates branch_facts for the given branch.
func (idx *store) rebuildFacts(ctx context.Context, git *Service, branch, head string, progress RebuildProgress) (int, error) {
	branchID, err := idx.EnsureBranch(ctx, branch, "refs/heads/"+branch)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: ensure branch: %w", err)
	}

	paths, hashes, err := git.ListAllWithHash(ctx, branch)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: list all: %w", err)
	}

	if progress != nil {
		progress("facts", 0, len(paths))
	}

	// Create temp table for the rebuild entries.
	if _, err := conn(ctx, idx.db).ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS _rebuild_entries (path TEXT PRIMARY KEY, blob_hash TEXT NOT NULL)`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: create temp table: %w", err)
	}
	// Ensure it's empty (in case of prior incomplete rebuild).
	if _, err := conn(ctx, idx.db).ExecContext(ctx, `DELETE FROM _rebuild_entries`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear temp table: %w", err)
	}

	// Bulk-insert all (path, blobHash) pairs in a single transaction.
	tx, err := idx.db.BeginTx(ctx, nil)
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
	res, err := conn(ctx, idx.db).ExecContext(ctx, `
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
	`, BlobObjectType)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: bulk insert: %w", err)
	}

	affected, _ := res.RowsAffected()
	n := int(affected)

	// Populate branch_facts: link each fact to this branch with its commit_hash.
	// Prefer commit_log entries scoped to this branch; fall back to legacy rows
	// with NULL branch_id (written before the branch existed in the branches table).
	if _, err := conn(ctx, idx.db).ExecContext(ctx, `
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
	if _, err := conn(ctx, idx.db).ExecContext(ctx, `DROP TABLE IF EXISTS _rebuild_entries`); err != nil {
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
func (idx *store) rebuildEmbeddings(ctx context.Context, progress RebuildProgress) (int, error) {
	emb := idx.getEmbedder()
	if emb == nil {
		return 0, nil
	}

	batcher, hasBatch := emb.(BatchEmbedder)

	var total int
	if err := conn(ctx, idx.db).QueryRowContext(ctx, `SELECT COUNT(*) FROM facts WHERE rowid NOT IN (SELECT rowid FROM facts_vec)`).Scan(&total); err != nil {
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
	rows, err := conn(ctx, idx.db).QueryContext(ctx, `SELECT rowid, path FROM facts WHERE rowid NOT IN (SELECT rowid FROM facts_vec)`)
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
		qargs := append([]any{BlobObjectType}, args...)

		bodyRows, err := conn(ctx, idx.db).QueryContext(ctx, q, qargs...)
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

		tx, err := idx.db.BeginTx(ctx, nil)
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
func (idx *store) rebuildGraph(ctx context.Context, progress RebuildProgress) (int, error) {
	// Read all facts ordered by oldest commit first so that when a fact's
	// DERIVED_FROM edges are created, its ref targets are already graph nodes.
	rows, err := conn(ctx, idx.db).QueryContext(ctx, `
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
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rebuildGraph: begin tx: %w", err)
	}
	defer tx.Rollback()

	for i, rec := range facts {
		if err := idx.graphSyncFactTx(ctx, tx, rec); err != nil {
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
	if idx.getEmbedder() != nil {
		type simEdge struct{ fromPath, fromBH, toPath, toBH string }
		var edges []simEdge

		for _, rec := range facts {
			emb, err := idx.getEmbeddingByFact(ctx, rec.Path, rec.BlobHash)
			if err != nil || emb == nil {
				continue
			}
			vecBlob := float32SliceToBytes(emb)
			rows, err := conn(ctx, idx.db).QueryContext(ctx,
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
			simTx, err := idx.db.BeginTx(ctx, nil)
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
