// CRUD operations on the facts table: insert/update, delete, get by path,
// embedding retrieval, and meta key-value storage (last_commit tracking).
// All mutations keep the vec0 index in sync within transactions.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog/log"
)

// Upsert inserts or replaces a FactRecord, keeping the vec0 index in sync.
// The body is read from the objects table via BlobHash for embedding computation.
func (idx *Index) Upsert(rec FactRecord) error {
	domainJSON, err := json.Marshal(rec.Domain)
	if err != nil {
		return fmt.Errorf("marshal domain: %w", err)
	}
	entitiesJSON, err := json.Marshal(rec.Entities)
	if err != nil {
		return fmt.Errorf("marshal entities: %w", err)
	}
	refsJSON, err := json.Marshal(rec.Refs)
	if err != nil {
		return fmt.Errorf("marshal refs: %w", err)
	}

	// Compute embedding vector if an embedder is configured.
	var vecData []byte
	if idx.embedder != nil {
		// Read body from objects table for embedding computation.
		var data []byte
		err := idx.db.QueryRow(
			`SELECT data FROM objects WHERE hash = ? AND type = ?`,
			rec.BlobHash, BlobObjectType,
		).Scan(&data)
		if err != nil {
			return fmt.Errorf("upsert: blob %s not found: %w", rec.BlobHash, err)
		}
		body := extractBody(data)
		vec, err := idx.embedder.Embed(body)
		if err == nil && len(vec) > 0 {
			vecData = float32SliceToBytes(vec)
		}
	}

	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Insert or replace into facts.
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO facts(path, title, blob_hash, domain, entities, confidence, sources, refs, commit_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Path, rec.Title, rec.BlobHash,
		string(domainJSON), string(entitiesJSON),
		rec.Confidence, rec.Sources,
		string(refsJSON), rec.CommitHash,
	); err != nil {
		return fmt.Errorf("upsert fact: %w", err)
	}

	// Insert embedding into facts_vec.
	if vecData != nil {
		newRowid := int64(0)
		_ = tx.QueryRow(`SELECT rowid FROM facts WHERE path=?`, rec.Path).Scan(&newRowid)
		if newRowid > 0 {
			if _, err := tx.Exec(`DELETE FROM facts_vec WHERE rowid = ?`, newRowid); err != nil {
				return fmt.Errorf("delete old vec row: %w", err)
			}
			if _, err := tx.Exec(
				`INSERT INTO facts_vec(rowid, embedding) VALUES (?, ?)`,
				newRowid, vecData,
			); err != nil {
				return fmt.Errorf("insert vec row: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Sync graph: create/update nodes and edges for this fact.
	if err := idx.graphSyncFact(rec); err != nil {
		log.Warn().Err(err).Str("path", rec.Path).Msg("graph sync failed on upsert")
	}

	// Build similarity edges if embeddings are available.
	if idx.embedder != nil {
		if err := idx.graphBuildSimilarityEdges(rec.Path); err != nil {
			log.Warn().Err(err).Str("path", rec.Path).Msg("graph similarity edges failed")
		}
	}

	return nil
}

// Delete removes a fact and its vec0 entry by path.
func (idx *Index) Delete(path string) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Delete from facts_vec first (referential integrity).
	var oldRowid int64
	err = tx.QueryRow(`SELECT rowid FROM facts WHERE path=?`, path).Scan(&oldRowid)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read fact for delete: %w", err)
	}
	if err == nil {
		if _, err := tx.Exec(`DELETE FROM facts_vec WHERE rowid = ?`, oldRowid); err != nil {
			return fmt.Errorf("delete vec row: %w", err)
		}
	}

	if _, err := tx.Exec(`DELETE FROM facts WHERE path=?`, path); err != nil {
		return fmt.Errorf("delete fact: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Mark fact as deleted in graph (preserves lineage via incoming DERIVED_FROM).
	if err := idx.graphDeleteFact(path); err != nil {
		log.Warn().Err(err).Str("path", path).Msg("graph delete failed")
	}

	return nil
}

// GetByPath retrieves a FactWithBody by its path, hydrating the body from
// the objects table. Returns nil, nil if not found.
func (idx *Index) GetByPath(path string) (*FactWithBody, error) {
	row := idx.db.QueryRow(
		`SELECT f.path, f.title, f.blob_hash, f.domain, f.entities, f.confidence, f.sources, f.refs, f.commit_hash, o.data
		 FROM facts f
		 JOIN objects o ON o.hash = f.blob_hash AND o.type = ?
		 WHERE f.path = ?`, BlobObjectType, path,
	)
	return scanFactWithBody(row)
}

// GetEmbedding returns the stored embedding vector for a fact.
// Returns nil, nil if no embedding exists for this path.
func (idx *Index) GetEmbedding(path string) ([]float32, error) {
	var blob []byte
	err := idx.db.QueryRow(
		`SELECT fv.embedding FROM facts_vec fv JOIN facts f ON fv.rowid = f.rowid WHERE f.path = ?`,
		path,
	).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get embedding: %w", err)
	}
	if len(blob) == 0 {
		return nil, nil
	}
	return bytesToFloat32Slice(blob)
}

// SetLastCommit stores the last processed commit hash in the meta table.
func (idx *Index) SetLastCommit(hash string) error {
	_, err := idx.db.Exec(
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('last_commit', ?)`,
		hash,
	)
	return err
}

// GetLastCommit returns the last processed commit hash, or "" if not set.
func (idx *Index) GetLastCommit() (string, error) {
	var hash string
	err := idx.db.QueryRow(`SELECT value FROM meta WHERE key='last_commit'`).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return hash, nil
}

// scanFactWithBody scans a FactWithBody from a *sql.Row (facts JOIN objects).
func scanFactWithBody(row *sql.Row) (*FactWithBody, error) {
	var f FactWithBody
	var domainJSON, entitiesJSON, refsJSON string
	var rawData []byte
	err := row.Scan(
		&f.Path, &f.Title, &f.BlobHash,
		&domainJSON, &entitiesJSON,
		&f.Confidence, &f.Sources,
		&refsJSON, &f.CommitHash, &rawData,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scanFactWithBody: %w", err)
	}
	json.Unmarshal([]byte(domainJSON), &f.Domain)
	json.Unmarshal([]byte(entitiesJSON), &f.Entities)
	json.Unmarshal([]byte(refsJSON), &f.Refs)
	f.Body = extractBody(rawData)
	return &f, nil
}

// scanFactRecord scans a FactRecord (without body) from a *sql.Row.
func scanFactRecord(row *sql.Row) (*FactRecord, error) {
	var rec FactRecord
	var domainJSON, entitiesJSON, refsJSON string
	err := row.Scan(
		&rec.Path, &rec.Title, &rec.BlobHash,
		&domainJSON, &entitiesJSON,
		&rec.Confidence, &rec.Sources,
		&refsJSON, &rec.CommitHash,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan fact: %w", err)
	}
	json.Unmarshal([]byte(domainJSON), &rec.Domain)
	json.Unmarshal([]byte(entitiesJSON), &rec.Entities)
	json.Unmarshal([]byte(refsJSON), &rec.Refs)
	return &rec, nil
}

// scanFactRecordFromRows scans a FactRecord from *sql.Rows (used in multi-row queries).
func scanFactRecordFromRows(rows *sql.Rows) (*FactRecord, error) {
	var rec FactRecord
	var domainJSON, entitiesJSON, refsJSON string
	err := rows.Scan(
		&rec.Path, &rec.Title, &rec.BlobHash,
		&domainJSON, &entitiesJSON,
		&rec.Confidence, &rec.Sources,
		&refsJSON, &rec.CommitHash,
	)
	if err != nil {
		return nil, fmt.Errorf("scan fact row: %w", err)
	}
	json.Unmarshal([]byte(domainJSON), &rec.Domain)
	json.Unmarshal([]byte(entitiesJSON), &rec.Entities)
	json.Unmarshal([]byte(refsJSON), &rec.Refs)
	return &rec, nil
}

// scanFactWithBodyFromRows scans a FactWithBody from *sql.Rows (facts JOIN objects).
func scanFactWithBodyFromRows(rows *sql.Rows) (*FactWithBody, error) {
	var f FactWithBody
	var domainJSON, entitiesJSON, refsJSON string
	var rawData []byte
	err := rows.Scan(
		&f.Path, &f.Title, &f.BlobHash,
		&domainJSON, &entitiesJSON,
		&f.Confidence, &f.Sources,
		&refsJSON, &f.CommitHash, &rawData,
	)
	if err != nil {
		return nil, fmt.Errorf("scanFactWithBodyFromRows: %w", err)
	}
	json.Unmarshal([]byte(domainJSON), &f.Domain)
	json.Unmarshal([]byte(entitiesJSON), &f.Entities)
	json.Unmarshal([]byte(refsJSON), &f.Refs)
	f.Body = extractBody(rawData)
	return &f, nil
}
