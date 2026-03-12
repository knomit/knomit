// CRUD operations on the facts table: insert/update, delete, get by path,
// embedding retrieval, and meta key-value storage (last_commit tracking).
// All mutations keep the FTS5 and vec0 indexes in sync within transactions.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Upsert inserts or replaces a FactRecord, keeping the FTS5 and vec0 indexes
// in sync. The operation runs in a single transaction:
//  1. Read old row (if any) to prepare FTS5 explicit delete.
//  2. Compute embedding via Embedder (if configured).
//  3. INSERT OR REPLACE into facts.
//  4. Remove old FTS5 entry, insert new one.
//  5. Remove old vec0 entry, insert new one.
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

	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Read old row values (if any) so we can issue the FTS5 explicit delete command.
	var oldRowid int64
	var oldTitle, oldBody, oldEntities, oldDomain string
	var hasOld bool
	err = tx.QueryRow(
		`SELECT rowid, title, body, entities, domain FROM facts WHERE path=?`,
		rec.Path,
	).Scan(&oldRowid, &oldTitle, &oldBody, &oldEntities, &oldDomain)
	if err == nil {
		hasOld = true
	} else if err != sql.ErrNoRows {
		return fmt.Errorf("read old fact: %w", err)
	}

	// Compute embedding vector if an embedder is configured.
	var vecData []byte
	if idx.embedder != nil {
		vec, err := idx.embedder.Embed(rec.Body)
		if err == nil && len(vec) > 0 {
			vecData = float32SliceToBytes(vec)
		}
	}

	// Insert or replace into facts.
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO facts(path, title, body, domain, entities, confidence, sources, refs, commit_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Path, rec.Title, rec.Body,
		string(domainJSON), string(entitiesJSON),
		rec.Confidence, rec.Sources,
		string(refsJSON), rec.CommitHash,
	); err != nil {
		return fmt.Errorf("upsert fact: %w", err)
	}

	// If an old row existed, remove it from FTS5 using the explicit 'delete' command.
	if hasOld {
		if _, err := tx.Exec(
			`INSERT INTO facts_fts(facts_fts, rowid, title, body, entities, domain)
			 VALUES('delete', ?, ?, ?, ?, ?)`,
			oldRowid, oldTitle, oldBody, oldEntities, oldDomain,
		); err != nil {
			return fmt.Errorf("delete old fts row: %w", err)
		}
	}

	// Insert new row into FTS using the new rowid.
	if _, err := tx.Exec(
		`INSERT INTO facts_fts(rowid, title, body, entities, domain)
		 VALUES ((SELECT rowid FROM facts WHERE path=?), ?, ?, ?, ?)`,
		rec.Path, rec.Title, rec.Body, string(entitiesJSON), string(domainJSON),
	); err != nil {
		return fmt.Errorf("insert fts row: %w", err)
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

	return tx.Commit()
}

// Delete removes a fact and its FTS5 + vec0 entries by path.
func (idx *Index) Delete(path string) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Read old values to issue the FTS5 explicit 'delete' command.
	var oldRowid int64
	var oldTitle, oldBody, oldEntities, oldDomain string
	err = tx.QueryRow(
		`SELECT rowid, title, body, entities, domain FROM facts WHERE path=?`,
		path,
	).Scan(&oldRowid, &oldTitle, &oldBody, &oldEntities, &oldDomain)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read fact for delete: %w", err)
	}
	if err == nil {
		// Delete from facts_vec first (referential integrity).
		if _, err := tx.Exec(`DELETE FROM facts_vec WHERE rowid = ?`, oldRowid); err != nil {
			return fmt.Errorf("delete vec row: %w", err)
		}
		// Delete from FTS5.
		if _, err := tx.Exec(
			`INSERT INTO facts_fts(facts_fts, rowid, title, body, entities, domain)
			 VALUES('delete', ?, ?, ?, ?, ?)`,
			oldRowid, oldTitle, oldBody, oldEntities, oldDomain,
		); err != nil {
			return fmt.Errorf("delete fts row: %w", err)
		}
	}

	if _, err := tx.Exec(`DELETE FROM facts WHERE path=?`, path); err != nil {
		return fmt.Errorf("delete fact: %w", err)
	}

	return tx.Commit()
}

// GetByPath retrieves a FactRecord by its path. Returns nil, nil if not found.
func (idx *Index) GetByPath(path string) (*FactRecord, error) {
	row := idx.db.QueryRow(
		`SELECT path, title, body, domain, entities, confidence, sources, refs, commit_hash
		 FROM facts WHERE path=?`,
		path,
	)
	return scanFactRecord(row)
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

// scanFactRecord scans a single FactRecord from a *sql.Row.
// JSON-encoded fields (domain, entities, refs) are deserialized automatically.
func scanFactRecord(row *sql.Row) (*FactRecord, error) {
	var rec FactRecord
	var domainJSON, entitiesJSON, refsJSON string
	err := row.Scan(
		&rec.Path, &rec.Title, &rec.Body,
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
	if err := json.Unmarshal([]byte(domainJSON), &rec.Domain); err != nil {
		return nil, fmt.Errorf("unmarshal domain: %w", err)
	}
	if err := json.Unmarshal([]byte(entitiesJSON), &rec.Entities); err != nil {
		return nil, fmt.Errorf("unmarshal entities: %w", err)
	}
	if err := json.Unmarshal([]byte(refsJSON), &rec.Refs); err != nil {
		return nil, fmt.Errorf("unmarshal refs: %w", err)
	}
	return &rec, nil
}

// scanFactRecordFromRows scans a FactRecord from *sql.Rows (used in multi-row queries).
func scanFactRecordFromRows(rows *sql.Rows) (*FactRecord, error) {
	var rec FactRecord
	var domainJSON, entitiesJSON, refsJSON string
	err := rows.Scan(
		&rec.Path, &rec.Title, &rec.Body,
		&domainJSON, &entitiesJSON,
		&rec.Confidence, &rec.Sources,
		&refsJSON, &rec.CommitHash,
	)
	if err != nil {
		return nil, fmt.Errorf("scan fact row: %w", err)
	}
	if err := json.Unmarshal([]byte(domainJSON), &rec.Domain); err != nil {
		return nil, fmt.Errorf("unmarshal domain: %w", err)
	}
	if err := json.Unmarshal([]byte(entitiesJSON), &rec.Entities); err != nil {
		return nil, fmt.Errorf("unmarshal entities: %w", err)
	}
	if err := json.Unmarshal([]byte(refsJSON), &rec.Refs); err != nil {
		return nil, fmt.Errorf("unmarshal refs: %w", err)
	}
	return &rec, nil
}
