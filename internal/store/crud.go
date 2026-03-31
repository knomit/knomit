// CRUD operations on the facts table: insert/update, delete, get by path,
// embedding retrieval, and meta key-value storage (last_commit tracking).
// All mutations keep the vec0 index in sync within transactions.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
)

// Upsert inserts or replaces a FactRecord on the given branch, keeping the
// vec0 index in sync. COW dedup: if (path, blob_hash) already exists in the
// facts table, only the branch_facts pointer is updated.
func (idx *Index) Upsert(ctx context.Context, branch, commitHash string, rec FactRecord) error {
	branchID, err := idx.EnsureBranch(ctx, branch, "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("upsert: %w", err)
	}

	// Marshal JSON fields upfront (needed for both COW hit and miss).
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

	factType := rec.Type
	if factType == "" {
		factType = "observation"
	}

	// Compute embedding vector if an embedder is configured.
	var vecData []byte
	if emb := idx.getEmbedder(); emb != nil {
		var data []byte
		err := conn(ctx, idx.db).QueryRowContext(ctx,
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

	// Begin transaction for atomic COW check + insert.
	ctx, tx, ownTx, err := beginTxIfNeeded(ctx, idx.db)
	if err != nil {
		return fmt.Errorf("upsert begin tx: %w", err)
	}
	if ownTx {
		defer tx.Rollback()
	}
	db := conn(ctx, idx.db)

	// Atomic: insert fact if it doesn't exist yet (no TOCTOU race).
	_, err = db.ExecContext(ctx,
		`INSERT OR IGNORE INTO facts(path, blob_hash, title, type, domain, entities, confidence, sources, refs, evidence_weight)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Path, rec.BlobHash, rec.Title, factType,
		string(domainJSON), string(entitiesJSON),
		rec.Confidence, rec.Sources,
		string(refsJSON), rec.EvidenceWeight,
	)
	if err != nil {
		return fmt.Errorf("upsert fact: %w", err)
	}

	// Read the ID (works whether we just inserted or it already existed).
	var factID int64
	err = db.QueryRowContext(ctx,
		`SELECT id FROM facts WHERE path = ? AND blob_hash = ?`,
		rec.Path, rec.BlobHash,
	).Scan(&factID)
	if err != nil {
		return fmt.Errorf("upsert select id: %w", err)
	}

	// COW hit check: are junction tables already populated for this fact?
	var junctionExists int
	db.QueryRowContext(ctx,
		`SELECT 1 FROM fact_entities WHERE fact_id = ? LIMIT 1`, factID,
	).Scan(&junctionExists)
	if junctionExists > 0 || (len(rec.Entities) == 0 && hasAnyBranchFact(ctx, db, factID)) {
		// COW hit: fact fully indexed, just update branch pointer.
		_, err = db.ExecContext(ctx,
			`INSERT OR REPLACE INTO branch_facts(branch_id, path, fact_id, commit_hash) VALUES (?, ?, ?, ?)`,
			branchID, rec.Path, factID, commitHash)
		if err != nil {
			return fmt.Errorf("upsert branch_facts (cow hit): %w", err)
		}
		if ownTx {
			return tx.Commit()
		}
		return nil
	}

	// COW miss: populate junction tables, embeddings, branch_facts.
	for _, entity := range rec.Entities {
		if entity == "" {
			continue
		}
		if _, err := db.ExecContext(ctx,
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
		if _, err := db.ExecContext(ctx,
			`INSERT INTO fact_domains(fact_id, domain) VALUES (?, ?)`,
			factID, domain,
		); err != nil {
			return fmt.Errorf("upsert fact_domains: %w", err)
		}
	}

	// Insert embedding into facts_vec using the fact's id as rowid.
	if vecData != nil {
		if _, err := db.ExecContext(ctx, `DELETE FROM facts_vec WHERE rowid = ?`, factID); err != nil {
			return fmt.Errorf("delete old vec row: %w", err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO facts_vec(rowid, embedding) VALUES (?, ?)`,
			factID, vecData,
		); err != nil {
			return fmt.Errorf("insert vec row: %w", err)
		}
	}

	// Insert branch_facts pointer.
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO branch_facts(branch_id, path, fact_id, commit_hash) VALUES (?, ?, ?, ?)`,
		branchID, rec.Path, factID, commitHash,
	); err != nil {
		return fmt.Errorf("upsert branch_facts: %w", err)
	}

	if ownTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	// Sync graph: create/update nodes and edges for this fact.
	if err := idx.graphSyncFact(ctx, rec); err != nil {
		log.Warn().Err(err).Str("path", rec.Path).Msg("graph sync failed on upsert")
	}

	// Build similarity edges if embeddings are available.
	if idx.getEmbedder() != nil {
		if err := idx.graphBuildSimilarityEdges(ctx, rec.Path, rec.BlobHash); err != nil {
			log.Warn().Err(err).Str("path", rec.Path).Msg("graph similarity edges failed")
		}
	}

	return nil
}

// hasAnyBranchFact checks if any branch_facts row exists for the given fact_id.
func hasAnyBranchFact(ctx context.Context, db ctxExecer, factID int64) bool {
	var n int
	db.QueryRowContext(ctx, `SELECT 1 FROM branch_facts WHERE fact_id = ? LIMIT 1`, factID).Scan(&n)
	return n > 0
}

// Delete removes a fact from the given branch. If no other branch references
// the fact, the underlying facts row (and its vec/graph data) is also deleted.
func (idx *Index) Delete(ctx context.Context, branch, path string) error {
	branchID, err := idx.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}

	ctx, tx, ownTx, err := beginTxIfNeeded(ctx, idx.db)
	if err != nil {
		return fmt.Errorf("delete begin tx: %w", err)
	}
	if ownTx {
		defer tx.Rollback()
	}
	db := conn(ctx, idx.db)

	// Look up fact_id + blob_hash in one query.
	var factID int64
	var blobHash string
	err = db.QueryRowContext(ctx,
		`SELECT bf.fact_id, f.blob_hash FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 WHERE bf.branch_id = ? AND bf.path = ?`, branchID, path,
	).Scan(&factID, &blobHash)
	if err == sql.ErrNoRows {
		if ownTx {
			tx.Commit()
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete lookup: %w", err)
	}

	// Delete branch_facts pointer.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM branch_facts WHERE branch_id = ? AND path = ?`, branchID, path,
	); err != nil {
		return fmt.Errorf("delete branch_facts: %w", err)
	}

	// Check remaining references atomically (within same tx).
	var refCount int
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM branch_facts WHERE fact_id = ?`, factID,
	).Scan(&refCount)

	if refCount == 0 {
		// Orphaned: delete the fact (cascades to junction tables, triggers facts_vec).
		if _, err := db.ExecContext(ctx,
			`DELETE FROM facts WHERE id = ?`, factID,
		); err != nil {
			return fmt.Errorf("delete fact: %w", err)
		}
	}

	if ownTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	// Graph cleanup outside tx (idempotent).
	if refCount == 0 {
		if err := idx.graphDeleteFact(ctx, path, blobHash); err != nil {
			log.Warn().Err(err).Str("path", path).Msg("graph delete failed")
		}
	}
	return nil
}

// GetByPath retrieves a FactWithBody by its path on the given branch,
// hydrating the body from the objects table. Returns nil, nil if not found.
func (idx *Index) GetByPath(ctx context.Context, branch, path string) (*FactWithBody, error) {
	branchID, err := idx.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("getByPath: %w", err)
	}
	row := conn(ctx, idx.db).QueryRowContext(ctx,
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
func (idx *Index) GetEmbedding(ctx context.Context, branch, path string) ([]float32, error) {
	branchID, err := idx.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("getEmbedding: %w", err)
	}
	var blob []byte
	err = conn(ctx, idx.db).QueryRowContext(ctx,
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
func (idx *Index) getEmbeddingByFact(ctx context.Context, path, blobHash string) ([]float32, error) {
	var blob []byte
	err := conn(ctx, idx.db).QueryRowContext(ctx,
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

// casLastCommit atomically updates the last-commit watermark for a branch,
// but only if it currently equals prev. Returns true if the update succeeded.
func (idx *Index) casLastCommit(ctx context.Context, branch, prev, next string) (bool, error) {
	key := "last_commit:" + branch
	db := conn(ctx, idx.db)

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

// SetLastCommit stores the last processed commit hash in the meta table,
// scoped to the given branch.
func (idx *Index) SetLastCommit(ctx context.Context, branch, hash string) error {
	key := "last_commit:" + branch
	_, err := conn(ctx, idx.db).ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`,
		key, hash,
	)
	return err
}

// GetLastCommit returns the last processed commit hash for the given branch,
// or "" if not set.
func (idx *Index) GetLastCommit(ctx context.Context, branch string) (string, error) {
	key := "last_commit:" + branch
	var hash string
	err := conn(ctx, idx.db).QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&hash)
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
func (idx *Index) RecentFacts(ctx context.Context, branch, pathPrefix, query string, limit, offset int, includeTypes, excludeTypes, domain, entities, epOps []string) ([]RecentFactEntry, int, error) {
	if query != "" {
		return idx.recentFactsSearch(ctx, branch, pathPrefix, query, limit, offset, includeTypes, excludeTypes, domain, entities, epOps)
	}

	branchID, err := idx.branchID(ctx, branch)
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
	if err := conn(ctx, idx.db).QueryRowContext(ctx,
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
	rows, err := conn(ctx, idx.db).QueryContext(ctx,
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
func (idx *Index) recentFactsSearch(ctx context.Context, branch, pathPrefix, query string, limit, offset int, includeTypes, excludeTypes, domain, entities, epOps []string) ([]RecentFactEntry, int, error) {
	branchID, err := idx.branchID(ctx, branch)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts search: %w", err)
	}

	results, err := idx.Search(ctx, branch, SearchQuery{
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

	rows, err := conn(ctx, idx.db).QueryContext(ctx,
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
func (idx *Index) LastCommitForPath(ctx context.Context, branch, path string) (string, bool) {
	branchID, err := idx.branchID(ctx, branch)
	if err != nil {
		return "", false
	}
	var hash, action string
	// Try branch-scoped query first (entries with branch_id set).
	err = conn(ctx, idx.db).QueryRowContext(ctx,
		`SELECT commit_hash, action FROM commit_log WHERE branch_id = ? AND path = ? ORDER BY rowid DESC LIMIT 1`,
		branchID, path,
	).Scan(&hash, &action)
	if err != nil {
		// Fallback: legacy rows with NULL branch_id.
		err = conn(ctx, idx.db).QueryRowContext(ctx,
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
func (idx *Index) CommitTimestamp(ctx context.Context, commitHash string) (int64, bool) {
	var ts sql.NullInt64
	err := conn(ctx, idx.db).QueryRowContext(ctx,
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
