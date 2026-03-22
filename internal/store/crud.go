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
		text := rec.Title + " " + extractBody(data)
		vec, err := idx.embedder.Embed(text)
		if err == nil && len(vec) > 0 {
			vecData = float32SliceToBytes(vec)
		}
	}

	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Default type to "observation" if empty.
	factType := rec.Type
	if factType == "" {
		factType = "observation"
	}

	// Insert or replace into facts.
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO facts(path, title, blob_hash, type, domain, entities, confidence, sources, refs, commit_hash, evidence_weight)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Path, rec.Title, rec.BlobHash, factType,
		string(domainJSON), string(entitiesJSON),
		rec.Confidence, rec.Sources,
		string(refsJSON), rec.CommitHash, rec.EvidenceWeight,
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
		`SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities, f.confidence, f.sources, f.refs, f.commit_hash, f.evidence_weight, o.data
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

// SetLastCommit stores the last processed commit hash in the meta table,
// scoped to the given branch.
func (idx *Index) SetLastCommit(branch, hash string) error {
	key := "last_commit:" + branch
	_, err := idx.db.Exec(
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`,
		key, hash,
	)
	return err
}

// GetLastCommit returns the last processed commit hash for the given branch,
// or "" if not set.
func (idx *Index) GetLastCommit(branch string) (string, error) {
	key := "last_commit:" + branch
	var hash string
	err := idx.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&hash)
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
		&f.Path, &f.Title, &f.BlobHash, &f.Type,
		&domainJSON, &entitiesJSON,
		&f.Confidence, &f.Sources,
		&refsJSON, &f.CommitHash, &f.EvidenceWeight, &rawData,
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
		&rec.Path, &rec.Title, &rec.BlobHash, &rec.Type,
		&domainJSON, &entitiesJSON,
		&rec.Confidence, &rec.Sources,
		&refsJSON, &rec.CommitHash, &rec.EvidenceWeight,
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
		&rec.Path, &rec.Title, &rec.BlobHash, &rec.Type,
		&domainJSON, &entitiesJSON,
		&rec.Confidence, &rec.Sources,
		&refsJSON, &rec.CommitHash, &rec.EvidenceWeight,
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
		&f.Path, &f.Title, &f.BlobHash, &f.Type,
		&domainJSON, &entitiesJSON,
		&f.Confidence, &f.Sources,
		&refsJSON, &f.CommitHash, &f.EvidenceWeight, &rawData,
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

// RecentFactEntry is a lightweight record for the recent-facts endpoint.
type RecentFactEntry struct {
	Path        string  `json:"path"`
	Title       string  `json:"title"`
	Type        string  `json:"type"`
	CommittedAt int64   `json:"committed_at"`
	Operation   string  `json:"operation,omitempty"`
	Score       float64 `json:"score,omitempty"`
}

// RecentFacts returns facts under pathPrefix ordered by most recent commit,
// paginated by offset/limit. If query is non-empty, it performs a semantic
// search first and returns only matching facts (still ordered by time).
func (idx *Index) RecentFacts(pathPrefix, query string, limit, offset int, includeTypes, excludeTypes []string) ([]RecentFactEntry, int, error) {
	if query != "" {
		return idx.recentFactsSearch(pathPrefix, query, limit, offset, includeTypes, excludeTypes)
	}

	// Build WHERE clause with optional type filters.
	where := `f.path LIKE ? || '%'`
	args := []any{pathPrefix}
	if len(includeTypes) > 0 {
		placeholders := make([]string, len(includeTypes))
		for i, t := range includeTypes {
			placeholders[i] = "?"
			args = append(args, t)
		}
		where += " AND f.type IN (" + join(placeholders, ",") + ")"
	}
	if len(excludeTypes) > 0 {
		placeholders := make([]string, len(excludeTypes))
		for i, t := range excludeTypes {
			placeholders[i] = "?"
			args = append(args, t)
		}
		where += " AND f.type NOT IN (" + join(placeholders, ",") + ")"
	}

	var total int
	if err := idx.db.QueryRow(
		`SELECT COUNT(*) FROM facts f WHERE `+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("RecentFacts count: %w", err)
	}

	queryArgs := append(args, limit, offset)
	rows, err := idx.db.Query(
		`SELECT f.path, f.title, f.type, COALESCE(cl.committed_at, 0), COALESCE(cl.operation, '')
		 FROM facts f
		 LEFT JOIN commit_log cl ON f.commit_hash = cl.commit_hash AND f.path = cl.path
		 WHERE `+where+`
		 ORDER BY cl.committed_at DESC, f.path ASC
		 LIMIT ? OFFSET ?`,
		queryArgs...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts query: %w", err)
	}
	defer rows.Close()

	var entries []RecentFactEntry
	for rows.Next() {
		var e RecentFactEntry
		if err := rows.Scan(&e.Path, &e.Title, &e.Type, &e.CommittedAt, &e.Operation); err != nil {
			return nil, 0, fmt.Errorf("RecentFacts scan: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// recentFactsSearch uses semantic search to find matching facts, then returns
// them ordered by committed_at with pagination.
func (idx *Index) recentFactsSearch(pathPrefix, query string, limit, offset int, includeTypes, excludeTypes []string) ([]RecentFactEntry, int, error) {
	results, err := idx.Search(SearchQuery{
		Text:         query,
		Path:         pathPrefix,
		IncludeTypes: includeTypes,
		ExcludeTypes: excludeTypes,
		Limit:        500, // large enough to get all matches for pagination
	})
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts search: %w", err)
	}
	if len(results) == 0 {
		return []RecentFactEntry{}, 0, nil
	}

	// Build score map from search results
	scoreByPath := make(map[string]float64, len(results))
	placeholders := make([]string, len(results))
	args := make([]any, len(results))
	for i, r := range results {
		placeholders[i] = "?"
		args[i] = r.Path
		scoreByPath[r.Path] = r.Score
	}

	rows, err := idx.db.Query(
		`SELECT f.path, f.title, f.type, COALESCE(cl.committed_at, 0), COALESCE(cl.operation, '')
		 FROM facts f
		 LEFT JOIN commit_log cl ON f.commit_hash = cl.commit_hash AND f.path = cl.path
		 WHERE f.path IN (`+join(placeholders, ",")+`)
		 ORDER BY cl.committed_at DESC, f.path ASC`,
		args...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts search query: %w", err)
	}
	defer rows.Close()

	var all []RecentFactEntry
	for rows.Next() {
		var e RecentFactEntry
		if err := rows.Scan(&e.Path, &e.Title, &e.Type, &e.CommittedAt, &e.Operation); err != nil {
			return nil, 0, fmt.Errorf("RecentFacts search scan: %w", err)
		}
		e.Score = scoreByPath[e.Path]
		all = append(all, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	total := len(all)
	if offset >= total {
		return []RecentFactEntry{}, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return all[offset:end], total, nil
}

func join(ss []string, sep string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
}
