// Three-phase rebuild: facts, embeddings, graph.
// Each phase runs in a single transaction for efficiency.
package store

import (
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog/log"
)

// rebuildFacts bulk-inserts facts via SQL JOIN with knomit_parse_fact().
func (idx *Index) rebuildFacts(git GitReader, head string, progress RebuildProgress) (int, error) {
	paths, hashes, err := git.ListAllWithHash()
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: list all: %w", err)
	}

	if progress != nil {
		progress("facts", 0, len(paths))
	}

	// Create temp table for the rebuild entries.
	if _, err := idx.db.Exec(`CREATE TEMP TABLE IF NOT EXISTS _rebuild_entries (path TEXT PRIMARY KEY, blob_hash TEXT NOT NULL)`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: create temp table: %w", err)
	}
	// Ensure it's empty (in case of prior incomplete rebuild).
	if _, err := idx.db.Exec(`DELETE FROM _rebuild_entries`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear temp table: %w", err)
	}

	// Bulk-insert all (path, blobHash) pairs in a single transaction.
	tx, err := idx.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: begin insert tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO _rebuild_entries(path, blob_hash) VALUES (?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: prepare insert: %w", err)
	}
	defer stmt.Close()

	for i := range paths {
		if _, err := stmt.Exec(paths[i], hashes[i]); err != nil {
			return 0, fmt.Errorf("rebuildFacts: insert entry %s: %w", paths[i], err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rebuildFacts: commit entries: %w", err)
	}

	// Bulk INSERT OR REPLACE facts from parsed blob data.
	res, err := idx.db.Exec(`
		WITH parsed_entries AS (
			SELECT e.path, e.blob_hash, knomit_parse_fact(o.data) AS parsed
			FROM _rebuild_entries e
			JOIN objects o ON o.hash = e.blob_hash AND o.type = ?
		)
		INSERT OR REPLACE INTO facts (path, title, blob_hash, type, domain, entities, confidence, sources, refs, commit_hash)
		SELECT
			pe.path,
			json_extract(pe.parsed, '$.title'),
			pe.blob_hash,
			json_extract(pe.parsed, '$.type'),
			json_extract(pe.parsed, '$.domain'),
			json_extract(pe.parsed, '$.entities'),
			json_extract(pe.parsed, '$.confidence'),
			json_extract(pe.parsed, '$.sources'),
			json_extract(pe.parsed, '$.refs'),
			COALESCE(cl.commit_hash, '')
		FROM parsed_entries pe
		LEFT JOIN (
			SELECT path, commit_hash, ROW_NUMBER() OVER (PARTITION BY path ORDER BY committed_at DESC) AS rn
			FROM commit_log
		) cl ON cl.path = pe.path AND cl.rn = 1
		WHERE pe.parsed IS NOT NULL
	`, BlobObjectType)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: bulk insert: %w", err)
	}

	// Clean up temp table.
	if _, err := idx.db.Exec(`DROP TABLE IF EXISTS _rebuild_entries`); err != nil {
		log.Warn().Err(err).Msg("rebuildFacts: drop temp table")
	}

	affected, _ := res.RowsAffected()
	n := int(affected)

	if progress != nil {
		progress("facts", n, n)
	}

	return n, nil
}

// rebuildEmbeddings computes embeddings for all facts missing from facts_vec.
func (idx *Index) rebuildEmbeddings(progress RebuildProgress) (int, error) {
	if idx.embedder == nil {
		return 0, nil
	}

	batcher, hasBatch := idx.embedder.(BatchEmbedder)

	var total int
	if err := idx.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE rowid NOT IN (SELECT rowid FROM facts_vec)`).Scan(&total); err != nil {
		return 0, fmt.Errorf("rebuildEmbeddings: count: %w", err)
	}

	if total == 0 {
		return 0, nil
	}

	if progress != nil {
		progress("embeddings", 0, total)
	}

	rows, err := idx.db.Query(`
		SELECT f.rowid, f.path, o.data
		FROM facts f
		JOIN objects o ON o.hash = f.blob_hash AND o.type = ?
		WHERE f.rowid NOT IN (SELECT rowid FROM facts_vec)
	`, BlobObjectType)
	if err != nil {
		return 0, fmt.Errorf("rebuildEmbeddings: query: %w", err)
	}

	type entry struct {
		rowid int64
		path  string
		body  string
	}

	var entries []entry
	for rows.Next() {
		var e entry
		var data []byte
		if err := rows.Scan(&e.rowid, &e.path, &data); err != nil {
			rows.Close()
			return 0, fmt.Errorf("rebuildEmbeddings: scan: %w", err)
		}
		e.body = extractBody(data)
		entries = append(entries, e)
	}
	rows.Close()

	// Embed and insert in a single transaction.
	tx, err := idx.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("rebuildEmbeddings: begin tx: %w", err)
	}
	defer tx.Rollback()

	const batchSize = 32
	done := 0

	if hasBatch {
		for i := 0; i < len(entries); i += batchSize {
			end := i + batchSize
			if end > len(entries) {
				end = len(entries)
			}
			batch := entries[i:end]

			texts := make([]string, len(batch))
			for j, e := range batch {
				texts[j] = e.body
			}

			vecs, err := batcher.EmbedBatch(texts)
			if err != nil {
				return done, fmt.Errorf("rebuildEmbeddings: embed batch: %w", err)
			}

			for j, vec := range vecs {
				if len(vec) == 0 {
					continue
				}
				if _, err := tx.Exec(`INSERT INTO facts_vec(rowid, embedding) VALUES (?, ?)`,
					batch[j].rowid, float32SliceToBytes(vec)); err != nil {
					return done, fmt.Errorf("rebuildEmbeddings: insert vec %s: %w", batch[j].path, err)
				}
				done++
			}

			if progress != nil {
				progress("embeddings", done, total)
			}
		}
	} else {
		for _, e := range entries {
			vec, err := idx.embedder.Embed(e.body)
			if err != nil {
				log.Warn().Err(err).Str("path", e.path).Msg("rebuildEmbeddings: embed failed, skipping")
				continue
			}
			if len(vec) == 0 {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO facts_vec(rowid, embedding) VALUES (?, ?)`,
				e.rowid, float32SliceToBytes(vec)); err != nil {
				return done, fmt.Errorf("rebuildEmbeddings: insert vec %s: %w", e.path, err)
			}
			done++
			if progress != nil && (done%10 == 0 || done == total) {
				progress("embeddings", done, total)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rebuildEmbeddings: commit: %w", err)
	}

	return done, nil
}

// rebuildGraph syncs graph nodes/edges for all facts in a single transaction,
// then builds similarity edges after commit.
func (idx *Index) rebuildGraph(progress RebuildProgress) (int, error) {
	// Read all facts.
	rows, err := idx.db.Query(`SELECT path, title, blob_hash, type, domain, entities, confidence, sources, refs, commit_hash FROM facts`)
	if err != nil {
		return 0, fmt.Errorf("rebuildGraph: query facts: %w", err)
	}

	var facts []FactRecord
	for rows.Next() {
		var rec FactRecord
		var domainJSON, entitiesJSON, refsJSON string
		if err := rows.Scan(&rec.Path, &rec.Title, &rec.BlobHash, &rec.Type,
			&domainJSON, &entitiesJSON, &rec.Confidence, &rec.Sources,
			&refsJSON, &rec.CommitHash); err != nil {
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
	tx, err := idx.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("rebuildGraph: begin tx: %w", err)
	}
	defer tx.Rollback()

	for i, rec := range facts {
		if err := idx.graphSyncFactTx(tx, rec); err != nil {
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
	if idx.embedder != nil {
		type simEdge struct{ from, to string }
		var edges []simEdge

		for _, rec := range facts {
			emb, err := idx.GetEmbedding(rec.Path)
			if err != nil || emb == nil {
				continue
			}
			vecBlob := float32SliceToBytes(emb)
			rows, err := idx.db.Query(
				`SELECT f.path, (1.0 - fv.distance) as similarity
				 FROM facts_vec fv
				 JOIN facts f ON f.rowid = fv.rowid
				 WHERE fv.embedding MATCH ? AND fv.k = ?
				 ORDER BY fv.distance ASC`,
				vecBlob, knnK+1,
			)
			if err != nil {
				log.Warn().Err(err).Str("path", rec.Path).Msg("rebuildGraph: knn query failed")
				continue
			}
			for rows.Next() {
				var neighborPath string
				var sim float64
				if err := rows.Scan(&neighborPath, &sim); err != nil {
					break
				}
				if neighborPath != rec.Path && sim >= knnThreshold {
					edges = append(edges, simEdge{from: rec.Path, to: neighborPath})
				}
			}
			rows.Close()
		}

		// Batch-write all similarity edges in a single transaction.
		if len(edges) > 0 {
			simTx, err := idx.db.Begin()
			if err != nil {
				log.Warn().Err(err).Msg("rebuildGraph: begin similarity tx")
			} else {
				for _, e := range edges {
					fp := escapeCypherKey(e.from)
					tp := escapeCypherKey(e.to)
					q := fmt.Sprintf(`SELECT cypher('MATCH (a:Fact {path: "%s"}), (b:Fact {path: "%s"}) MERGE (a)-[:SIMILAR_TO]->(b)')`, fp, tp)
					if _, err := simTx.Exec(q); err != nil {
						log.Warn().Err(err).Str("from", e.from).Str("to", e.to).Msg("rebuildGraph: similarity edge failed")
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
