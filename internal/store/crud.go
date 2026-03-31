// CRUD operations on the facts table: insert/update, delete, get by path,
// embedding retrieval, and meta key-value storage (last_commit tracking).
// All mutations keep the vec0 index in sync within transactions.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
)

// Upsert inserts or replaces a FactRecord on the given branch, keeping the
// vec0 index in sync. COW dedup: if (path, blob_hash) already exists in the
// facts table, only the branch_facts pointer is updated.
func (idx *Index) Upsert(branch, commitHash string, rec FactRecord) error {
	branchID, err := idx.EnsureBranch(branch, "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("upsert: %w", err)
	}

	// COW check: does this exact (path, blob_hash) already exist?
	var existingID int64
	err = idx.db.QueryRow(
		`SELECT id FROM facts WHERE path = ? AND blob_hash = ?`,
		rec.Path, rec.BlobHash,
	).Scan(&existingID)
	if err == nil {
		// COW hit: fact content already exists, just update the branch pointer.
		if _, err := idx.db.Exec(
			`INSERT OR REPLACE INTO branch_facts(branch_id, path, fact_id, commit_hash) VALUES (?, ?, ?, ?)`,
			branchID, rec.Path, existingID, commitHash,
		); err != nil {
			return fmt.Errorf("upsert branch_facts (cow hit): %w", err)
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("upsert cow check: %w", err)
	}

	// COW miss: full create path.
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
	if emb := idx.getEmbedder(); emb != nil {
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
		vec, err := emb.Embed(text)
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

	// Insert into facts.
	result, err := tx.Exec(
		`INSERT INTO facts(path, blob_hash, title, type, domain, entities, confidence, sources, refs, evidence_weight)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Path, rec.BlobHash, rec.Title, factType,
		string(domainJSON), string(entitiesJSON),
		rec.Confidence, rec.Sources,
		string(refsJSON), rec.EvidenceWeight,
	)
	if err != nil {
		return fmt.Errorf("upsert fact: %w", err)
	}

	factID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("upsert last insert id: %w", err)
	}

	// Populate junction tables using fact_id.
	for _, entity := range rec.Entities {
		if entity == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO fact_entities(fact_id, entity) VALUES (?, ?)`,
			factID, entity,
		); err != nil {
			return fmt.Errorf("upsert fact_entities: %w", err)
		}
	}
	for _, domain := range rec.Domain {
		if domain == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO fact_domains(fact_id, domain) VALUES (?, ?)`,
			factID, domain,
		); err != nil {
			return fmt.Errorf("upsert fact_domains: %w", err)
		}
	}

	// Insert embedding into facts_vec using the fact's id as rowid.
	if vecData != nil {
		if _, err := tx.Exec(`DELETE FROM facts_vec WHERE rowid = ?`, factID); err != nil {
			return fmt.Errorf("delete old vec row: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO facts_vec(rowid, embedding) VALUES (?, ?)`,
			factID, vecData,
		); err != nil {
			return fmt.Errorf("insert vec row: %w", err)
		}
	}

	// Insert branch_facts pointer.
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO branch_facts(branch_id, path, fact_id, commit_hash) VALUES (?, ?, ?, ?)`,
		branchID, rec.Path, factID, commitHash,
	); err != nil {
		return fmt.Errorf("upsert branch_facts: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Sync graph: create/update nodes and edges for this fact.
	if err := idx.graphSyncFact(rec); err != nil {
		log.Warn().Err(err).Str("path", rec.Path).Msg("graph sync failed on upsert")
	}

	// Build similarity edges if embeddings are available.
	if idx.getEmbedder() != nil {
		if err := idx.graphBuildSimilarityEdges(rec.Path, rec.BlobHash); err != nil {
			log.Warn().Err(err).Str("path", rec.Path).Msg("graph similarity edges failed")
		}
	}

	return nil
}

// Delete removes a fact from the given branch. If no other branch references
// the fact, the underlying facts row (and its vec/graph data) is also deleted.
func (idx *Index) Delete(branch, path string) error {
	branchID, err := idx.branchID(branch)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}

	// Look up the fact_id from branch_facts.
	var factID int64
	err = idx.db.QueryRow(
		`SELECT fact_id FROM branch_facts WHERE branch_id = ? AND path = ?`,
		branchID, path,
	).Scan(&factID)
	if err == sql.ErrNoRows {
		return nil // nothing to delete
	}
	if err != nil {
		return fmt.Errorf("delete lookup: %w", err)
	}

	// Remove the branch_facts row.
	if _, err := idx.db.Exec(
		`DELETE FROM branch_facts WHERE branch_id = ? AND path = ?`,
		branchID, path,
	); err != nil {
		return fmt.Errorf("delete branch_facts: %w", err)
	}

	// Check if any other branch still references this fact.
	var refCount int
	if err := idx.db.QueryRow(
		`SELECT COUNT(*) FROM branch_facts WHERE fact_id = ?`, factID,
	).Scan(&refCount); err != nil {
		return fmt.Errorf("delete refcount: %w", err)
	}

	if refCount == 0 {
		// Look up blob_hash before deleting (needed for graph node identification).
		var blobHash string
		_ = idx.db.QueryRow(`SELECT blob_hash FROM facts WHERE id = ?`, factID).Scan(&blobHash)

		// No more references — delete the underlying fact (cascade handles
		// fact_entities, fact_domains; trigger handles facts_vec).
		if _, err := idx.db.Exec(`DELETE FROM facts WHERE id = ?`, factID); err != nil {
			return fmt.Errorf("delete fact: %w", err)
		}

		// Mark fact as deleted in graph (preserves lineage via incoming DERIVED_FROM).
		if err := idx.graphDeleteFact(path, blobHash); err != nil {
			log.Warn().Err(err).Str("path", path).Msg("graph delete failed")
		}
	}

	return nil
}

// GetByPath retrieves a FactWithBody by its path on the given branch,
// hydrating the body from the objects table. Returns nil, nil if not found.
func (idx *Index) GetByPath(branch, path string) (*FactWithBody, error) {
	branchID, err := idx.branchID(branch)
	if err != nil {
		return nil, fmt.Errorf("getByPath: %w", err)
	}
	row := idx.db.QueryRow(
		`SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities,
		        f.confidence, f.sources, f.refs, f.evidence_weight,
		        bf.commit_hash, o.data
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 JOIN objects o ON o.hash = f.blob_hash AND o.type = ?
		 WHERE bf.branch_id = ? AND bf.path = ?`, BlobObjectType, branchID, path,
	)
	return scanFactWithBody(row)
}

// GetEmbedding returns the stored embedding vector for a fact on the given branch.
// Returns nil, nil if no embedding exists for this path.
func (idx *Index) GetEmbedding(branch, path string) ([]float32, error) {
	branchID, err := idx.branchID(branch)
	if err != nil {
		return nil, fmt.Errorf("getEmbedding: %w", err)
	}
	var blob []byte
	err = idx.db.QueryRow(
		`SELECT fv.embedding
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 JOIN facts_vec fv ON fv.rowid = f.id
		 WHERE bf.branch_id = ? AND bf.path = ?`,
		branchID, path,
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

// getEmbeddingByFact returns the stored embedding vector for a specific fact
// version identified by (path, blob_hash). No branch scoping.
// Used internally by graph similarity edge computation.
func (idx *Index) getEmbeddingByFact(path, blobHash string) ([]float32, error) {
	var blob []byte
	err := idx.db.QueryRow(
		`SELECT fv.embedding
		 FROM facts f
		 JOIN facts_vec fv ON fv.rowid = f.id
		 WHERE f.path = ? AND f.blob_hash = ?`,
		path, blobHash,
	).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getEmbeddingByFactPath: %w", err)
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

// scanFactWithBody scans a FactWithBody from a *sql.Row (branch_facts JOIN facts JOIN objects).
// Expected column order: path, title, blob_hash, type, domain, entities,
// confidence, sources, refs, evidence_weight, commit_hash, data.
func scanFactWithBody(row *sql.Row) (*FactWithBody, error) {
	var f FactWithBody
	var domainJSON, entitiesJSON, refsJSON string
	var rawData []byte
	err := row.Scan(
		&f.Path, &f.Title, &f.BlobHash, &f.Type,
		&domainJSON, &entitiesJSON,
		&f.Confidence, &f.Sources,
		&refsJSON, &f.EvidenceWeight, &f.CommitHash, &rawData,
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

// scanFactRecordFromRows scans a FactRecord from *sql.Rows (used in multi-row queries).
// Expected column order: path, title, blob_hash, type, domain, entities,
// confidence, sources, refs, evidence_weight (10 columns, no commit_hash).
func scanFactRecordFromRows(rows *sql.Rows) (*FactRecord, error) {
	var rec FactRecord
	var domainJSON, entitiesJSON, refsJSON string
	err := rows.Scan(
		&rec.Path, &rec.Title, &rec.BlobHash, &rec.Type,
		&domainJSON, &entitiesJSON,
		&rec.Confidence, &rec.Sources,
		&refsJSON, &rec.EvidenceWeight,
	)
	if err != nil {
		return nil, fmt.Errorf("scan fact row: %w", err)
	}
	json.Unmarshal([]byte(domainJSON), &rec.Domain)
	json.Unmarshal([]byte(entitiesJSON), &rec.Entities)
	json.Unmarshal([]byte(refsJSON), &rec.Refs)
	return &rec, nil
}

// scanFactWithBodyFromRows scans a FactWithBody from *sql.Rows (branch_facts JOIN facts JOIN objects).
// Expected column order: path, title, blob_hash, type, domain, entities,
// confidence, sources, refs, evidence_weight, commit_hash, data.
func scanFactWithBodyFromRows(rows *sql.Rows) (*FactWithBody, error) {
	var f FactWithBody
	var domainJSON, entitiesJSON, refsJSON string
	var rawData []byte
	err := rows.Scan(
		&f.Path, &f.Title, &f.BlobHash, &f.Type,
		&domainJSON, &entitiesJSON,
		&f.Confidence, &f.Sources,
		&refsJSON, &f.EvidenceWeight, &f.CommitHash, &rawData,
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

// RecentFacts returns facts on the given branch under pathPrefix ordered by
// most recent commit, paginated by offset/limit. If query is non-empty, it
// performs a semantic search first and returns only matching facts (still
// ordered by time). domain, entities, and epOps are optional additional filters.
func (idx *Index) RecentFacts(branch, pathPrefix, query string, limit, offset int, includeTypes, excludeTypes, domain, entities, epOps []string) ([]RecentFactEntry, int, error) {
	if query != "" {
		return idx.recentFactsSearch(branch, pathPrefix, query, limit, offset, includeTypes, excludeTypes, domain, entities, epOps)
	}

	branchID, err := idx.branchID(branch)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts: %w", err)
	}

	flt := newFactFilter(SearchQuery{
		Path:         pathPrefix,
		IncludeTypes: includeTypes,
		ExcludeTypes: excludeTypes,
		Domain:       domain,
		Entities:     entities,
	})

	// Build the ep filter clause (operates on cl.operation from the LEFT JOIN).
	epClause := ""
	epArgs := []any{}
	if len(epOps) > 0 {
		ph := strings.Repeat("?,", len(epOps))
		epArgs = make([]any, len(epOps))
		for i, op := range epOps {
			epArgs[i] = op
		}
		epClause = " AND COALESCE(cl.operation, '') IN (" + ph[:len(ph)-1] + ")"
	}

	countArgs := append(append([]any{branchID}, flt.args...), epArgs...)
	var total int
	if err := idx.db.QueryRow(
		`SELECT COUNT(*)
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 LEFT JOIN commit_log cl ON bf.commit_hash = cl.commit_hash AND f.path = cl.path
		 WHERE bf.branch_id = ?`+flt.SQL()+epClause,
		countArgs...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("RecentFacts count: %w", err)
	}

	queryArgs := append(append(append([]any{branchID}, flt.args...), epArgs...), limit, offset)
	rows, err := idx.db.Query(
		`SELECT f.path, f.title, f.type, COALESCE(cl.committed_at, 0), COALESCE(cl.operation, '')
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 LEFT JOIN commit_log cl ON bf.commit_hash = cl.commit_hash AND f.path = cl.path
		 WHERE bf.branch_id = ?`+flt.SQL()+epClause+`
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
func (idx *Index) recentFactsSearch(branch, pathPrefix, query string, limit, offset int, includeTypes, excludeTypes, domain, entities, epOps []string) ([]RecentFactEntry, int, error) {
	branchID, err := idx.branchID(branch)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts search: %w", err)
	}

	results, err := idx.Search(branch, SearchQuery{
		Text:         query,
		Path:         pathPrefix,
		IncludeTypes: includeTypes,
		ExcludeTypes: excludeTypes,
		Domain:       domain,
		Entities:     entities,
		EpisodeOps:   epOps,
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
	args := make([]any, len(results)+1)
	args[0] = branchID
	for i, r := range results {
		placeholders[i] = "?"
		args[i+1] = r.Path
		scoreByPath[r.Path] = r.Score
	}

	rows, err := idx.db.Query(
		`SELECT f.path, f.title, f.type, COALESCE(cl.committed_at, 0), COALESCE(cl.operation, '')
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 LEFT JOIN commit_log cl ON bf.commit_hash = cl.commit_hash AND f.path = cl.path
		 WHERE bf.branch_id = ? AND f.path IN (`+join(placeholders, ",")+`)
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

// LastCommitForPath returns the commit hash of the most recent commit_log
// entry for the given path, provided that entry's action is not 'deleted'.
// Returns ("", false) if the path is not found or its latest action is deleted.
func (idx *Index) LastCommitForPath(branch, path string) (string, bool) {
	branchID, err := idx.branchID(branch)
	if err != nil {
		return "", false
	}
	var hash, action string
	// Try branch-scoped query first (entries with branch_id set).
	err = idx.db.QueryRow(
		`SELECT commit_hash, action FROM commit_log WHERE branch_id = ? AND path = ? ORDER BY rowid DESC LIMIT 1`,
		branchID, path,
	).Scan(&hash, &action)
	if err != nil {
		// Fallback: legacy rows with NULL branch_id.
		err = idx.db.QueryRow(
			`SELECT commit_hash, action FROM commit_log WHERE path = ? ORDER BY rowid DESC LIMIT 1`,
			path,
		).Scan(&hash, &action)
	}
	if err != nil || hash == "" || action == "deleted" {
		return "", false
	}
	return hash, true
}

// CommitTimestamp returns the committed_at unix timestamp for the given commit hash.
// Returns (0, false) if the hash is not in commit_log.
func (idx *Index) CommitTimestamp(commitHash string) (int64, bool) {
	var ts sql.NullInt64
	err := idx.db.QueryRow(
		`SELECT committed_at FROM commit_log WHERE commit_hash = ? LIMIT 1`,
		commitHash,
	).Scan(&ts)
	if err != nil || !ts.Valid {
		return 0, false
	}
	return ts.Int64, true
}

func join(ss []string, sep string) string {
	var b strings.Builder
	for i, s := range ss {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s)
	}
	return b.String()
}
