package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	storegit "knomit/internal/store/git"
)

type searchIndex struct {
	rh       *repoHandler
	embedMu  sync.RWMutex
	embedder Embedder
}

// SetEmbedder attaches an Embedder to the index. When set, Upsert will call
// Embed on each record's body and persist the result in facts_vec.
func (si *searchIndex) SetEmbedder(e Embedder) {
	si.embedMu.Lock()
	defer si.embedMu.Unlock()
	si.embedder = e
}

// getEmbedder returns the current Embedder under a read lock.
func (si *searchIndex) getEmbedder() Embedder {
	si.embedMu.RLock()
	defer si.embedMu.RUnlock()
	return si.embedder
}

// ── CRUD operations ───────────────────────────────────────────────────────────
// CRUD operations on the facts table: insert/update, delete, get by path,
// embedding retrieval, and meta key-value storage (last_commit tracking).
// All mutations keep the vec0 index in sync within transactions.

// Upsert inserts or replaces a FactRecord on the given branch, keeping the
// vec0 index in sync. COW dedup: if (path, blob_hash) already exists in the
// facts table, only the branch_facts pointer is updated.
func (si *searchIndex) Upsert(ctx context.Context, branch, commitHash string, rec FactRecord) error {
	branchID, err := si.rh.EnsureBranch(ctx, branch, "refs/heads/"+branch)
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
	if emb := si.getEmbedder(); emb != nil {
		var data []byte
		err := conn(ctx, si.rh.db).QueryRowContext(ctx,
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
	ctx, tx, ownTx, err := beginTxIfNeeded(ctx, si.rh.db)
	if err != nil {
		return fmt.Errorf("upsert begin tx: %w", err)
	}
	if ownTx {
		defer tx.Rollback()
	}
	db := conn(ctx, si.rh.db)

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
	if err := si.graphSyncFact(ctx, rec); err != nil {
		log.Warn().Err(err).Str("path", rec.Path).Msg("graph sync failed on upsert")
	}

	// Build similarity edges if embeddings are available.
	if si.getEmbedder() != nil {
		if err := si.graphBuildSimilarityEdges(ctx, rec.Path, rec.BlobHash); err != nil {
			log.Warn().Err(err).Str("path", rec.Path).Msg("graph similarity edges failed")
		}
	}

	return nil
}

// hasAnyBranchFact checks if any branch_facts row exists for the given fact_id.
func hasAnyBranchFact(ctx context.Context, db storegit.CtxExecer, factID int64) bool {
	var n int
	db.QueryRowContext(ctx, `SELECT 1 FROM branch_facts WHERE fact_id = ? LIMIT 1`, factID).Scan(&n)
	return n > 0
}

// Delete removes a fact from the given branch. If no other branch references
// the fact, the underlying facts row (and its vec/graph data) is also deleted.
func (si *searchIndex) Delete(ctx context.Context, branch, path string) error {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}

	ctx, tx, ownTx, err := beginTxIfNeeded(ctx, si.rh.db)
	if err != nil {
		return fmt.Errorf("delete begin tx: %w", err)
	}
	if ownTx {
		defer tx.Rollback()
	}
	db := conn(ctx, si.rh.db)

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
		if err := si.graphDeleteFact(ctx, path, blobHash); err != nil {
			log.Warn().Err(err).Str("path", path).Msg("graph delete failed")
		}
	}
	return nil
}

// GetByPath retrieves a FactWithBody by its path on the given branch,
// hydrating the body from the objects table. Returns nil, nil if not found.
func (si *searchIndex) GetByPath(ctx context.Context, branch, path string) (*FactWithBody, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("getByPath: %w", err)
	}
	row := conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities,
		        f.confidence, f.sources, f.refs, f.evidence_weight,
		        bf.commit_hash, o.data, cl.committed_at
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 JOIN objects o ON o.hash = f.blob_hash AND o.type = ?
		 LEFT JOIN commit_log cl ON cl.commit_hash = bf.commit_hash AND cl.path = bf.path
		 WHERE bf.branch_id = ? AND bf.path = ?`, BlobObjectType, branchID, path,
	)
	return scanFactWithBody(row)
}


// getEmbeddingByFact returns the stored embedding vector for a specific fact
// version identified by (path, blob_hash). No branch scoping.
// Used internally by graph similarity edge computation.
func (si *searchIndex) getEmbeddingByFact(ctx context.Context, path, blobHash string) ([]float32, error) {
	var blob []byte
	err := conn(ctx, si.rh.db).QueryRowContext(ctx,
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

// GetLastCommit returns the last processed commit hash for the given branch,
// or "" if not set.
func (si *searchIndex) GetLastCommit(ctx context.Context, branch string) (string, error) {
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

// scanFactWithBody scans a FactWithBody from a *sql.Row (branch_facts JOIN facts JOIN objects LEFT JOIN commit_log).
// Expected column order: path, title, blob_hash, type, domain, entities,
// confidence, sources, refs, evidence_weight, commit_hash, data, committed_at.
func scanFactWithBody(row *sql.Row) (*FactWithBody, error) {
	var f FactWithBody
	var domainJSON, entitiesJSON, refsJSON string
	var rawData []byte
	var committedAt sql.NullInt64
	err := row.Scan(
		&f.Path, &f.Title, &f.BlobHash, &f.Type,
		&domainJSON, &entitiesJSON,
		&f.Confidence, &f.Sources,
		&refsJSON, &f.EvidenceWeight, &f.CommitHash, &rawData, &committedAt,
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
	if committedAt.Valid {
		f.CommittedAt = committedAt.Int64
	}
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
func (si *searchIndex) RecentFacts(ctx context.Context, branch, pathPrefix, query string, limit, offset int, includeTypes, excludeTypes, domain, entities, epOps []string) ([]RecentFactEntry, int, error) {
	if query != "" {
		return si.recentFactsSearch(ctx, branch, pathPrefix, query, limit, offset, includeTypes, excludeTypes, domain, entities, epOps)
	}

	branchID, err := si.rh.branchID(ctx, branch)
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
	if err := conn(ctx, si.rh.db).QueryRowContext(ctx,
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
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
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
func (si *searchIndex) recentFactsSearch(ctx context.Context, branch, pathPrefix, query string, limit, offset int, includeTypes, excludeTypes, domain, entities, epOps []string) ([]RecentFactEntry, int, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, 0, fmt.Errorf("RecentFacts search: %w", err)
	}

	results, err := si.Search(ctx, branch, SearchQuery{
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

	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
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
func (si *searchIndex) LastCommitForPath(ctx context.Context, branch, path string) (string, bool) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return "", false
	}
	var hash, action string
	// Try branch-scoped query first (entries with branch_id set).
	err = conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT commit_hash, action FROM commit_log WHERE branch_id = ? AND path = ? ORDER BY rowid DESC LIMIT 1`,
		branchID, path,
	).Scan(&hash, &action)
	if err != nil {
		// Fallback: legacy rows with NULL branch_id.
		err = conn(ctx, si.rh.db).QueryRowContext(ctx,
			`SELECT commit_hash, action FROM commit_log WHERE path = ? ORDER BY rowid DESC LIMIT 1`,
			path,
		).Scan(&hash, &action)
	}
	if err != nil || hash == "" || action == "deleted" {
		return "", false
	}
	return hash, true
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

// ── Search ────────────────────────────────────────────────────────────────────
// Search: vector similarity search over the fact index. Supports text queries
// (via embeddings), entity/domain/path/confidence filters, and cosine
// similarity thresholds.

// SearchQuery describes a hybrid search request.
type SearchQuery struct {
	Text          string
	Entities      []string
	Domain        []string
	Path          string
	MinConfidence float64
	MinSimilarity float64   // cosine similarity threshold (0–1); 0 uses default 0.40
	Limit         int
	GraphHops     int       // number of graph traversal hops to expand results (0 = disabled)
	QueryVec      []float32 // pre-computed embedding vector; if set, skips Embed(Text)
	IncludeTypes  []string  // only return facts with these types (empty = all)
	ExcludeTypes  []string  // exclude facts with these types
	EpisodeOps    []string  // filter by episode operation type (e.g. "learn", "update", "retract"); filtered post-query in Go
}

// SearchResult is a FactWithBody paired with a relevance score in [0, 100].
type SearchResult struct {
	FactWithBody
	Score float64 `json:"score"`
}

// ── factFilter ────────────────────────────────────────────────────────────────

// factFilter builds the shared WHERE-clause fragment used by both search paths.
// Each Add call appends one "AND ..." clause and its bind parameters.
// SQL() returns the concatenated fragment (empty string if no filters set).
type factFilter struct {
	clauses []string
	args    []any
}

func (f *factFilter) add(clause string, args ...any) {
	f.clauses = append(f.clauses, clause)
	f.args = append(f.args, args...)
}

func (f *factFilter) SQL() string { return strings.Join(f.clauses, "") }

func newFactFilter(q SearchQuery) *factFilter {
	f := &factFilter{}
	if q.MinConfidence > 0 {
		f.add(" AND f.confidence >= ?", q.MinConfidence)
	}
	if q.Path != "" {
		f.add(" AND f.path LIKE ?", q.Path+"%")
	}
	if len(q.IncludeTypes) > 0 {
		ph := strings.Repeat("?,", len(q.IncludeTypes))
		args := make([]any, len(q.IncludeTypes))
		for i, t := range q.IncludeTypes {
			args[i] = t
		}
		f.add(" AND f.type IN ("+ph[:len(ph)-1]+")", args...)
	}
	if len(q.ExcludeTypes) > 0 {
		ph := strings.Repeat("?,", len(q.ExcludeTypes))
		args := make([]any, len(q.ExcludeTypes))
		for i, t := range q.ExcludeTypes {
			args[i] = t
		}
		f.add(" AND f.type NOT IN ("+ph[:len(ph)-1]+")", args...)
	}
	if len(q.Entities) > 0 {
		ph := strings.Repeat("?,", len(q.Entities))
		ph = ph[:len(ph)-1]
		args := make([]any, len(q.Entities)+1)
		for i, e := range q.Entities {
			args[i] = e
		}
		args[len(q.Entities)] = len(q.Entities)
		f.add(
			" AND (SELECT COUNT(DISTINCT entity) FROM fact_entities WHERE fact_id = f.id AND entity IN ("+ph+")) >= ?",
			args...,
		)
	}
	for _, d := range q.Domain {
		f.add(
			" AND EXISTS (SELECT 1 FROM fact_domains WHERE fact_id = f.id AND (domain = ? OR domain LIKE ?))",
			d, d+"/%",
		)
	}
	return f
}

// filterByEpisodeOps removes results whose latest commit operation is not in
// the allowed set. It performs a single bulk SQL lookup of operations by
// commit_hash from commit_log (same database). If ops is empty, all results
// are kept unchanged.
func (si *searchIndex) filterByEpisodeOps(ctx context.Context, results []SearchResult, ops []string) ([]SearchResult, error) {
	if len(ops) == 0 || len(results) == 0 {
		return results, nil
	}

	// Build a set of allowed operations for O(1) lookup.
	allowed := make(map[string]bool, len(ops))
	for _, op := range ops {
		allowed[op] = true
	}

	// Collect unique commit hashes from results.
	hashes := make([]string, 0, len(results))
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		if r.CommitHash != "" && !seen[r.CommitHash] {
			hashes = append(hashes, r.CommitHash)
			seen[r.CommitHash] = true
		}
	}
	if len(hashes) == 0 {
		return nil, nil
	}

	ph := strings.Repeat("?,", len(hashes))
	args := make([]any, len(hashes))
	for i, h := range hashes {
		args[i] = h
	}

	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT commit_hash, operation FROM commit_log WHERE commit_hash IN (`+ph[:len(ph)-1]+`) GROUP BY commit_hash`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("filterByEpisodeOps: %w", err)
	}
	defer rows.Close()

	opByHash := make(map[string]string, len(hashes))
	for rows.Next() {
		var hash, op string
		if err := rows.Scan(&hash, &op); err != nil {
			return nil, fmt.Errorf("filterByEpisodeOps scan: %w", err)
		}
		opByHash[hash] = op
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := results[:0]
	for _, r := range results {
		op := opByHash[r.CommitHash]
		if allowed[op] {
			out = append(out, r)
		}
	}
	return out, nil
}

// Search performs a vector similarity search over the index.
//
// Algorithm:
//  1. If Text is present → embed query, compute cosine similarity via vec0 KNN.
//  2. Apply Entities / Domain / Path / MinConfidence filters post-retrieval.
//  3. Normalise top-N scores to [0,100].
//  4. Return sorted by score descending, capped at Limit.
//
// If Text is empty, all facts matching the non-text filters are returned with
// score 100.
func (si *searchIndex) Search(ctx context.Context, branch string, q SearchQuery) ([]SearchResult, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	flt := newFactFilter(q)

	// ── Text-less path: return all facts matching filters with score 100 ──
	if q.Text == "" {
		args := append(append([]any{BlobObjectType, branchID}, flt.args...), limit)
		rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
			`SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities,
			        f.confidence, f.sources, f.refs, f.evidence_weight,
			        bf.commit_hash, o.data
			 FROM branch_facts bf
			 JOIN facts f ON f.id = bf.fact_id
			 JOIN objects o ON o.hash = f.blob_hash AND o.type = ?
			 WHERE bf.branch_id = ?`+flt.SQL()+` LIMIT ?`,
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf("search: list all: %w", err)
		}
		defer rows.Close()

		var out []SearchResult
		for rows.Next() {
			fb, err := scanFactWithBodyFromRows(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, SearchResult{FactWithBody: *fb, Score: 100})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return si.filterByEpisodeOps(ctx, out, q.EpisodeOps)
	}

	// ── Vector (embedding) search ─────────────────────────────────────────
	type candidate struct {
		rec   FactWithBody
		score float64
	}

	vecSimByPath := make(map[string]float64)
	emb := si.getEmbedder()
	if emb == nil && len(q.QueryVec) == 0 {
		log.Debug().Msg("search: no embedder configured, skipping vec search")
	} else {
		queryVec := q.QueryVec
		if len(queryVec) == 0 {
			var embedErr error
			queryVec, embedErr = emb.Embed(q.Text)
			if embedErr != nil {
				log.Warn().Err(embedErr).Msg("search: embed query failed")
			}
		}
		if queryVec == nil {
			log.Warn().Msg("search: no query vector available")
		} else {
			vecBlob := float32SliceToBytes(queryVec)
			kLimit := limit * 5
			if q.MinSimilarity > 0.7 {
				kLimit = limit * 2
			} else if q.MinSimilarity > 0.5 {
				kLimit = limit * 3
			}
			rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
				`SELECT f.path, (1.0 - fv.distance) as similarity
				 FROM facts_vec fv
				 JOIN facts f ON f.id = fv.rowid
				 JOIN branch_facts bf ON bf.fact_id = f.id AND bf.branch_id = ?
				 WHERE fv.embedding MATCH ? AND fv.k = ?
				 ORDER BY fv.distance ASC`,
				branchID, vecBlob, kLimit,
			)
			if err != nil {
				log.Warn().Err(err).Msg("search: vec query failed")
			} else {
				for rows.Next() {
					var path string
					var sim float64
					if err := rows.Scan(&path, &sim); err != nil {
						break
					}
					vecSimByPath[path] = sim
				}
				rows.Close()
				log.Debug().Int("vec_hits", len(vecSimByPath)).Msg("vec search complete")
			}
		}
	}

	if graphHops := q.GraphHops; graphHops > 0 && len(vecSimByPath) > 0 {
		for path, score := range si.graphExpandSearch(ctx, branchID, vecSimByPath, graphHops) {
			if _, exists := vecSimByPath[path]; !exists {
				vecSimByPath[path] = score
			}
		}
	}

	if len(vecSimByPath) == 0 {
		return nil, nil
	}

	minSim := q.MinSimilarity
	if minSim <= 0 {
		minSim = 0.40
	}

	candidatePaths := make([]string, 0, len(vecSimByPath))
	for path, cosine := range vecSimByPath {
		if cosine > minSim {
			candidatePaths = append(candidatePaths, path)
		}
	}
	if len(candidatePaths) == 0 {
		return nil, nil
	}

	// ── Phase 1: fetch metadata only (no body), apply filters, sort, trim ─
	pathPH := strings.Repeat("?,", len(candidatePaths))
	pathArgs := make([]any, 0, len(candidatePaths)+1)
	pathArgs = append(pathArgs, branchID)
	for _, p := range candidatePaths {
		pathArgs = append(pathArgs, p)
	}

	metaRows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT f.path, f.title, f.blob_hash, f.type, f.domain, f.entities,
		        f.confidence, f.sources, f.refs, f.evidence_weight
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 WHERE bf.branch_id = ? AND f.path IN (`+pathPH[:len(pathPH)-1]+`)`+flt.SQL(),
		append(pathArgs, flt.args...)...,
	)
	if err != nil {
		return nil, fmt.Errorf("search: meta fetch: %w", err)
	}
	defer metaRows.Close()

	var candidates []candidate
	for metaRows.Next() {
		rec, err := scanFactRecordFromRows(metaRows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate{rec: FactWithBody{FactRecord: *rec}, score: vecSimByPath[rec.Path]})
	}
	if err := metaRows.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].score > candidates[j-1].score; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	// ── Phase 2: fetch bodies for the top-limit facts only ────────────────
	bodyPH := strings.Repeat("?,", len(candidates))
	bodyArgs := make([]any, 0, len(candidates)+1)
	bodyArgs = append(bodyArgs, BlobObjectType)
	for _, c := range candidates {
		bodyArgs = append(bodyArgs, c.rec.BlobHash)
	}
	bodyRows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT hash, data FROM objects WHERE type = ? AND hash IN (`+bodyPH[:len(bodyPH)-1]+`)`,
		bodyArgs...,
	)
	if err != nil {
		return nil, fmt.Errorf("search: body fetch: %w", err)
	}
	defer bodyRows.Close()

	bodies := make(map[string]string, len(candidates))
	for bodyRows.Next() {
		var hash string
		var data []byte
		if err := bodyRows.Scan(&hash, &data); err != nil {
			return nil, err
		}
		bodies[hash] = string(data)
	}
	if err := bodyRows.Err(); err != nil {
		return nil, err
	}

	// ── Assemble final results ────────────────────────────────────────────
	out := make([]SearchResult, 0, len(candidates))
	for _, c := range candidates {
		c.rec.Body = bodies[c.rec.BlobHash]
		out = append(out, SearchResult{FactWithBody: c.rec, Score: c.score * 100.0})
	}
	return si.filterByEpisodeOps(ctx, out, q.EpisodeOps)
}

// ── Explain ───────────────────────────────────────────────────────────────────

// RefSummary is a lightweight fact reference returned by ExplainFact.
type RefSummary struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Deleted bool   `json:"deleted,omitempty"`
}

// ExplainResult holds the incoming and outgoing reference summary for a fact.
type ExplainResult struct {
	Incoming []RefSummary `json:"incoming"`
	Outgoing []RefSummary `json:"outgoing"`
}

// ExplainFact returns the incoming and outgoing [:DERIVED_FROM] neighbours for
// the given fact path, scoped to facts visible on the given branch.
// Incoming excludes deleted referrers. Outgoing includes deleted targets
// (marked with Deleted: true) so the UI can show them distinctly.
// Self-loops are filtered out: GraphQLite creates (n)-[:DERIVED_FROM]->(n) when
// the target node is absent at edge-creation time (upstream bug).
func (si *searchIndex) ExplainFact(ctx context.Context, branch, path string) (ExplainResult, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain: %w", err)
	}

	// Resolve the blob_hash for this path on the given branch so we query
	// the specific fact version visible on this branch, not all versions.
	var blobHash string
	err = conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT f.blob_hash FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id
		 WHERE bf.branch_id = ? AND bf.path = ?`, branchID, path,
	).Scan(&blobHash)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain: resolve blob_hash: %w", err)
	}

	params, _ := json.Marshal(map[string]string{"path": path, "blob_hash": blobHash})
	pj := string(params)

	// Incoming: all non-deleted facts that reference ANY version at this path.
	// Scoped to branch-visible facts via filterByBranch.
	incoming, err := si.queryRefSummaries(ctx,
		fmt.Sprintf(`MATCH (f:%s)-[:%s]->(t:%s {path: $path}) WHERE NOT f.deleted = true RETURN f.path AS path, f.title AS title, false AS deleted`,
			NodeFact, EdgeDerivedFrom, NodeFact),
		pj,
	)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain incoming: %w", err)
	}

	// Outgoing: refs from the specific version visible on this branch.
	outgoing, err := si.queryRefSummaries(ctx,
		fmt.Sprintf(`MATCH (f:%s {path: $path})-[:%s]->(t:%s) WHERE f.blob_hash = $blob_hash RETURN t.path AS path, t.title AS title, t.deleted AS deleted`,
			NodeFact, EdgeDerivedFrom, NodeFact),
		pj,
	)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain outgoing: %w", err)
	}

	return ExplainResult{
		Incoming: si.filterByBranch(ctx, filterSelf(incoming, path), branchID),
		Outgoing: filterSelf(outgoing, path),
	}, nil
}

// filterSelf removes any RefSummary whose path equals selfPath (self-loops).
func filterSelf(refs []RefSummary, selfPath string) []RefSummary {
	out := refs[:0]
	for _, r := range refs {
		if r.Path != selfPath {
			out = append(out, r)
		}
	}
	return out
}

// filterByBranch keeps only RefSummary entries whose path is visible on the
// given branch (present in branch_facts).
func (si *searchIndex) filterByBranch(ctx context.Context, refs []RefSummary, branchID int64) []RefSummary {
	if len(refs) == 0 {
		return refs
	}
	visible := make(map[string]bool)
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, `SELECT path FROM branch_facts WHERE branch_id = ?`, branchID)
	if err != nil {
		return refs
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		rows.Scan(&p)
		visible[p] = true
	}
	out := refs[:0]
	for _, r := range refs {
		if visible[r.Path] {
			out = append(out, r)
		}
	}
	return out
}

// isDeletedVal interprets the raw value returned by json_extract for a boolean
// property. SQLite maps JSON true→int64(1), JSON false→int64(0), and Cypher
// literal false→string("0"). Returns true only for int64(1).
func isDeletedVal(v interface{}) bool {
	switch val := v.(type) {
	case bool:
		return val
	case int64:
		return val == 1
	case []byte:
		return string(val) == "1"
	}
	return false
}


// refSummariesByEdgeSource returns RefSummary entries for all target nodes
// reachable from sourceNodeID via edges of edgeType, where the target has label targetLabel.
// It reads path and title properties from the EAV tables.
func (si *searchIndex) refSummariesByEdgeSource(ctx context.Context, sourceNodeID int64, edgeType, targetLabel string) ([]RefSummary, error) {
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, `
		SELECT DISTINCT
			path_prop.value AS path,
			COALESCE(title_prop.value, '') AS title
		FROM edges e
		JOIN node_labels nl ON nl.node_id = e.target_id AND nl.label = ?
		JOIN node_props_text path_prop ON path_prop.node_id = e.target_id
			AND path_prop.key_id = (SELECT id FROM property_keys WHERE key = 'path' LIMIT 1)
		LEFT JOIN node_props_text title_prop ON title_prop.node_id = e.target_id
			AND title_prop.key_id = (SELECT id FROM property_keys WHERE key = 'title' LIMIT 1)
		WHERE e.source_id = ? AND e.type = ?
	`, targetLabel, sourceNodeID, edgeType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRefSummaryRows(rows)
}

// refSummariesByEdgeTarget returns RefSummary entries for all source nodes
// pointing to targetNodeID via edges of edgeType, where the source has label sourceLabel.
// It reads path and title properties from the EAV tables.
func (si *searchIndex) refSummariesByEdgeTarget(ctx context.Context, targetNodeID int64, edgeType, sourceLabel string) ([]RefSummary, error) {
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, `
		SELECT DISTINCT
			path_prop.value AS path,
			COALESCE(title_prop.value, '') AS title
		FROM edges e
		JOIN node_labels nl ON nl.node_id = e.source_id AND nl.label = ?
		JOIN node_props_text path_prop ON path_prop.node_id = e.source_id
			AND path_prop.key_id = (SELECT id FROM property_keys WHERE key = 'path' LIMIT 1)
		LEFT JOIN node_props_text title_prop ON title_prop.node_id = e.source_id
			AND title_prop.key_id = (SELECT id FROM property_keys WHERE key = 'title' LIMIT 1)
		WHERE e.target_id = ? AND e.type = ?
	`, sourceLabel, targetNodeID, edgeType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRefSummaryRows(rows)
}

// scanRefSummaryRows scans (path, title) rows into []RefSummary.
func scanRefSummaryRows(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]RefSummary, error) {
	var result []RefSummary
	for rows.Next() {
		var path, title string
		if err := rows.Scan(&path, &title); err != nil {
			return nil, fmt.Errorf("scan ref summary: %w", err)
		}
		if path == "" {
			continue
		}
		result = append(result, RefSummary{Path: path, Title: title})
	}
	return result, rows.Err()
}

// queryRefSummaries runs a Cypher query that returns (path, title, deleted) rows.
// cypherQuery must contain only $param placeholders (no embedded values).
// paramsJSON is the JSON-encoded parameter object passed as cypher()'s second arg.
func (si *searchIndex) queryRefSummaries(ctx context.Context, cypherQuery, paramsJSON string) ([]RefSummary, error) {
	q := `SELECT json_extract(value, '$.path'), json_extract(value, '$.title'), json_extract(value, '$.deleted') FROM json_each(cypher('` + cypherQuery + `', ?))`
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, q, paramsJSON)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RefSummary{}
	for rows.Next() {
		var path, title string
		// json_extract on a JSON boolean returns an integer in SQLite (1=true, 0=false).
		// However, Cypher literal `false AS col` may return the string "0".
		// Scan into interface{} to handle both cases uniformly.
		var deletedRaw interface{}
		if err := rows.Scan(&path, &title, &deletedRaw); err != nil {
			return nil, fmt.Errorf("scan ref summary: %w", err)
		}
		if path == "" {
			continue
		}
		deleted := isDeletedVal(deletedRaw)
		result = append(result, RefSummary{Path: path, Title: title, Deleted: deleted})
	}
	return result, rows.Err()
}

// ── Graph operations ──────────────────────────────────────────────────────────
// Graph operations: Cypher wrappers for maintaining the knowledge graph.
// All graph mutations use MERGE for idempotency.
//
// Parameterized queries (cypher('...', params)) work for read operations
// (MATCH/RETURN) but NOT for write operations (MERGE/SET/DELETE) in the
// installed GraphQLite build. Write operations embed values via string
// interpolation using escapeCypherKey/escapeCypherVal.
//
// Note: MERGE+SET in a single Cypher statement does not work in GraphQLite;
// a subsequent MATCH+SET is required to update properties reliably.

// Node labels used in GraphQLite Cypher queries.
const (
	NodeFact         = "Fact"
	NodeEntity       = "Entity"
	NodeDomain       = "Domain"
	NodeOntologyNode = "OntologyNode"
	NodeFactVersion  = "FactVersion" // historical snapshot of a Fact at a specific commit
)

// Edge types used in GraphQLite Cypher queries.
const (
	EdgeTagged          = "TAGGED"            // Fact → Entity
	EdgeInDomain        = "IN_DOMAIN"         // Fact → Domain
	EdgeUnder           = "UNDER"             // Fact → OntologyNode
	EdgeDerivedFrom     = "DERIVED_FROM"      // Fact → Fact (local ref lineage)
	EdgeSimilarTo       = "SIMILAR_TO"        // Fact ↔ Fact (KNN similarity)
	EdgeDomainChildOf   = "DOMAIN_CHILD_OF"   // Domain → Domain (hierarchy)
	EdgeOntologyChildOf = "ONTOLOGY_CHILD_OF" // OntologyNode → OntologyNode (hierarchy)
	EdgePrevVersion     = "PREV_VERSION"      // FactVersion → older FactVersion (same path)
)

// graphSyncFact creates or updates graph nodes and edges for a fact.
// This implements the Learn mutation from the spec:
//  1. MERGE Fact node
//  2. Delete old TAGGED, IN_DOMAIN, UNDER, DERIVED_FROM edges
//  3. MERGE Entity nodes + TAGGED edges
//  4. MERGE Domain hierarchy + IN_DOMAIN edges
//  5. MERGE OntologyNode hierarchy + UNDER edge
//  6. Sync DERIVED_FROM edges from local refs
func (si *searchIndex) graphSyncFact(ctx context.Context, rec FactRecord) error {
	return si.graphSyncFactTx(ctx, si.rh.db, rec)
}

// graphSyncFactTx is the transactional version of graphSyncFact.
func (si *searchIndex) graphSyncFactTx(ctx context.Context, tx execer, rec FactRecord) error {
	path := escapeCypherKey(rec.Path)
	bh := escapeCypherKey(rec.BlobHash)
	title := escapeCypherVal(rec.Title)

	// 1. MERGE Fact node keyed by {path, blob_hash} — each fact version gets
	// its own graph node (immutable once created). Then SET properties in a
	// separate statement.
	//
	// GraphQLite limitation: MATCH with multiple property predicates silently
	// fails for write operations (SET, edge MERGE, DELETE). MERGE with multiple
	// properties works for node creation, but all subsequent write operations
	// must use MATCH{path} + WHERE blob_hash = "..." to filter correctly.
	q := fmt.Sprintf(`SELECT cypher('MERGE (f:%s {path: "%s", blob_hash: "%s"})')`, NodeFact, path, bh)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph merge fact: %w", err)
	}
	q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}) WHERE f.blob_hash = "%s" SET f.title = "%s", f.user_id = "%s", f.confidence = %f, f.sources = %d, f.deleted = false, f.type = "%s"')`,
		NodeFact, path, bh, title, path, rec.Confidence, rec.Sources, escapeCypherVal(rec.Type))
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph set fact props: %w", err)
	}

	// 2. Delete old relationship edges for this fact version.
	for _, edgeType := range []string{EdgeTagged, EdgeInDomain, EdgeUnder, EdgeDerivedFrom} {
		q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"})-[r:%s]->() WHERE f.blob_hash = "%s" DELETE r')`, NodeFact, path, edgeType, bh)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph delete old %s edges: %w", edgeType, err)
		}
	}

	// 3. MERGE Entity nodes + TAGGED edges.
	// GraphQLite silently ignores the third MERGE in a multi-MERGE query, so
	// we split: first MERGE the entity node, then MATCH both and MERGE the edge.
	for _, entity := range rec.Entities {
		e := escapeCypherKey(entity)
		q = fmt.Sprintf(`SELECT cypher('MERGE (e:%s {name: "%s"})')`, NodeEntity, e)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph merge entity %s: %w", entity, err)
		}
		q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}), (e:%s {name: "%s"}) WHERE f.blob_hash = "%s" MERGE (f)-[:%s]->(e)')`, NodeFact, path, NodeEntity, e, bh, EdgeTagged)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph tagged %s: %w", entity, err)
		}
	}

	// 4. MERGE Domain hierarchy + IN_DOMAIN edges.
	for _, domain := range rec.Domain {
		if err := si.graphMergeDomainHierarchy(ctx, tx, rec.Path, rec.BlobHash, domain); err != nil {
			return err
		}
	}

	// 5. MERGE OntologyNode hierarchy + UNDER edge.
	if err := si.graphMergeOntologyHierarchy(ctx, tx, rec.Path, rec.BlobHash); err != nil {
		return err
	}

	// 6. Sync DERIVED_FROM edges from local refs (invariant: always matches rec.Refs).
	var localRefs []string
	for _, r := range rec.Refs {
		if !strings.HasPrefix(r, "http://") && !strings.HasPrefix(r, "https://") {
			localRefs = append(localRefs, r)
		}
	}
	if len(localRefs) > 0 {
		if err := si.graphAddDerivedFromTx(ctx, tx, rec.Path, rec.BlobHash, localRefs); err != nil {
			return fmt.Errorf("graph sync derived_from: %w", err)
		}
	}

	return nil
}

// graphMergeDomainHierarchy creates the full domain ancestor chain and links
// the fact to the leaf domain via IN_DOMAIN.
func (si *searchIndex) graphMergeDomainHierarchy(ctx context.Context, tx execer, factPath, factBlobHash, domain string) error {
	parts := strings.Split(domain, "/")
	for i := range parts {
		seg := strings.Join(parts[:i+1], "/")
		escaped := escapeCypherKey(seg)
		q := fmt.Sprintf(`SELECT cypher('MERGE (:%s {path: "%s"})')`, NodeDomain, escaped)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph merge domain %s: %w", seg, err)
		}
		if i > 0 {
			parent := escapeCypherKey(strings.Join(parts[:i], "/"))
			q = fmt.Sprintf(`SELECT cypher('MATCH (c:%s {path: "%s"}), (p:%s {path: "%s"}) MERGE (c)-[:%s]->(p)')`, NodeDomain, escaped, NodeDomain, parent, EdgeDomainChildOf)
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("graph domain child_of %s: %w", seg, err)
			}
		}
	}
	leaf := escapeCypherKey(domain)
	fp := escapeCypherKey(factPath)
	fbh := escapeCypherKey(factBlobHash)
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}), (d:%s {path: "%s"}) WHERE f.blob_hash = "%s" MERGE (f)-[:%s]->(d)')`, NodeFact, fp, NodeDomain, leaf, fbh, EdgeInDomain)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph in_domain %s: %w", domain, err)
	}
	return nil
}

// graphMergeOntologyHierarchy creates OntologyNode chain from the fact's file
// path and links the fact to the leaf via UNDER.
func (si *searchIndex) graphMergeOntologyHierarchy(ctx context.Context, tx execer, factPath, factBlobHash string) error {
	parts := strings.Split(factPath, "/")
	if len(parts) < 2 {
		return nil
	}
	dirParts := parts[:len(parts)-1]

	for i := range dirParts {
		seg := strings.Join(dirParts[:i+1], "/")
		escaped := escapeCypherKey(seg)
		q := fmt.Sprintf(`SELECT cypher('MERGE (:%s {path: "%s"})')`, NodeOntologyNode, escaped)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph merge ontology %s: %w", seg, err)
		}
		if i > 0 {
			parent := escapeCypherKey(strings.Join(dirParts[:i], "/"))
			q = fmt.Sprintf(`SELECT cypher('MATCH (c:%s {path: "%s"}), (p:%s {path: "%s"}) MERGE (c)-[:%s]->(p)')`, NodeOntologyNode, escaped, NodeOntologyNode, parent, EdgeOntologyChildOf)
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("graph ontology child_of %s: %w", seg, err)
			}
		}
	}
	leaf := escapeCypherKey(strings.Join(dirParts, "/"))
	fp := escapeCypherKey(factPath)
	fbh := escapeCypherKey(factBlobHash)
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}), (o:%s {path: "%s"}) WHERE f.blob_hash = "%s" MERGE (f)-[:%s]->(o)')`, NodeFact, fp, NodeOntologyNode, leaf, fbh, EdgeUnder)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph under %s: %w", factPath, err)
	}
	return nil
}

// graphDeleteFact marks a Fact node as deleted and removes its outgoing edges
// (except incoming DERIVED_FROM, which preserves lineage).
func (si *searchIndex) graphDeleteFact(ctx context.Context, path, blobHash string) error {
	return si.graphDeleteFactTx(ctx, si.rh.db, path, blobHash)
}

func (si *searchIndex) graphDeleteFactTx(ctx context.Context, tx execer, path, blobHash string) error {
	p := escapeCypherKey(path)
	bh := escapeCypherKey(blobHash)
	// Delete outgoing edges.
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"})-[r]->() WHERE f.blob_hash = "%s" DELETE r')`, NodeFact, p, bh)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph delete outgoing edges: %w", err)
	}
	// Delete incoming SIMILAR_TO edges (bidirectional cleanup).
	q = fmt.Sprintf(`SELECT cypher('MATCH ()-[r:%s]->(f:%s {path: "%s"}) WHERE f.blob_hash = "%s" DELETE r')`, EdgeSimilarTo, NodeFact, p, bh)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph delete incoming SIMILAR_TO: %w", err)
	}
	// Mark node as deleted.
	q = fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"}) WHERE f.blob_hash = "%s" SET f.deleted = true')`, NodeFact, p, bh)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graph mark deleted: %w", err)
	}
	return nil
}

// graphAddDerivedFromTx creates DERIVED_FROM edges from a new fact version to
// its source facts. The source (new fact) is matched by {path, blob_hash}; the
// target is matched by path only (any version at that path).
//
// GraphQLite bug: when the target node is absent, MATCH degenerates and MERGE
// creates a self-loop (n)-[:DERIVED_FROM]->(n). We accept this and filter
// self-loops at query time in ExplainFact instead of pre-checking (which would
// silently drop valid edges when facts are indexed in different orders during
// rebuild).
func (si *searchIndex) graphAddDerivedFromTx(ctx context.Context, tx execer, newPath, newBlobHash string, sourcePaths []string) error {
	np := escapeCypherKey(newPath)
	nbh := escapeCypherKey(newBlobHash)
	for _, src := range sourcePaths {
		sp := escapeCypherKey(src)
		q := fmt.Sprintf(`SELECT cypher('MATCH (n:%s {path: "%s"}), (s:%s {path: "%s"}) WHERE n.blob_hash = "%s" MERGE (n)-[:%s]->(s)')`, NodeFact, np, NodeFact, sp, nbh, EdgeDerivedFrom)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("graph derived_from %s→%s: %w", newPath, src, err)
		}
	}
	return nil
}

const (
	knnK         = 10   // top-K nearest neighbors per fact
	knnThreshold = 0.60 // minimum cosine similarity for SIMILAR_TO edges
)

// graphBuildSimilarityEdges creates SIMILAR_TO edges from a fact version to its
// top-K nearest neighbors (by cosine similarity via sqlite-vec KNN).
// Edges below the similarity threshold are not created.
//
// IMPORTANT: This function queries sqlite-vec (facts_vec) directly via si.db,
// so it must be called AFTER the surrounding transaction has committed.
// Calling it inside a transaction will not see uncommitted embedding writes.
func (si *searchIndex) graphBuildSimilarityEdges(ctx context.Context, path, blobHash string) error {
	emb, err := si.getEmbeddingByFact(ctx, path, blobHash)
	if err != nil || emb == nil {
		return nil
	}

	vecBlob := float32SliceToBytes(emb)

	// Collect neighbors first, then close rows before running Cypher mutations.
	// Running Exec() on the same *sql.DB while rows are open can interfere in
	// SQLite's single-writer model.
	type neighbor struct {
		path       string
		blobHash   string
		similarity float64
	}
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT f.path, f.blob_hash, (1.0 - fv.distance) as similarity
		 FROM facts_vec fv
		 JOIN facts f ON f.id = fv.rowid
		 WHERE fv.embedding MATCH ? AND fv.k = ?
		 ORDER BY fv.distance ASC`,
		vecBlob, knnK+1,
	)
	if err != nil {
		return fmt.Errorf("knn query for %s: %w", path, err)
	}
	var neighbors []neighbor
	for rows.Next() {
		var n neighbor
		if err := rows.Scan(&n.path, &n.blobHash, &n.similarity); err != nil {
			rows.Close()
			return fmt.Errorf("scan knn row: %w", err)
		}
		neighbors = append(neighbors, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("knn rows: %w", err)
	}
	rows.Close()

	// Delete old outgoing SIMILAR_TO edges for this fact version.
	p := escapeCypherKey(path)
	bh := escapeCypherKey(blobHash)
	q := fmt.Sprintf(`SELECT cypher('MATCH (f:%s {path: "%s"})-[r:%s]->() WHERE f.blob_hash = "%s" DELETE r')`, NodeFact, p, EdgeSimilarTo, bh)
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, q); err != nil {
		return fmt.Errorf("delete old SIMILAR_TO: %w", err)
	}

	for _, n := range neighbors {
		if n.path == path && n.blobHash == blobHash {
			continue
		}
		if n.similarity < knnThreshold {
			continue
		}
		np := escapeCypherKey(n.path)
		nbh := escapeCypherKey(n.blobHash)
		q = fmt.Sprintf(`SELECT cypher('MATCH (a:%s {path: "%s"}), (b:%s {path: "%s"}) WHERE a.blob_hash = "%s" AND b.blob_hash = "%s" MERGE (a)-[:%s]->(b)')`, NodeFact, p, NodeFact, np, bh, nbh, EdgeSimilarTo)
		if _, err := conn(ctx, si.rh.db).ExecContext(ctx, q); err != nil {
			return fmt.Errorf("create SIMILAR_TO %s→%s: %w", path, n.path, err)
		}
	}
	return nil
}

// ClusterResult holds the output of ClusterFacts.
type ClusterResult struct {
	Clusters map[int][]string // community ID → fact paths
	Noise    []string         // fact paths in communities below minCommunitySize
}

// ClusterFacts runs Louvain community detection on the full graph and returns
// community assignments for non-deleted Fact nodes.
//
// resolution controls Louvain granularity: higher = more, smaller communities.
// minCommunitySize: communities smaller than this are relabeled as noise.
func (si *searchIndex) ClusterFacts(ctx context.Context, branch string, resolution float64, minCommunitySize int) (ClusterResult, error) {
	if minCommunitySize <= 0 {
		minCommunitySize = 2
	}

	// GraphQLite's louvain() returns a single JSON string of the form:
	//   [{"column_0": [{"node_id": N, "user_id": null, "community": N}, ...]}]
	//
	// We use a SQL CTE to:
	//   1. Unpack the nested array via json_each.
	//   2. Join node_labels to keep only Fact nodes.
	//   3. Join node_props_text to resolve node_id → fact path.
	//
	// The property_keys table maps key names to integer IDs; we use a subquery
	// to find the key_id for "path" rather than hardcoding the integer.
	query := fmt.Sprintf(`
		WITH louvain_raw AS (
			SELECT
				CAST(json_extract(item.value, '$.node_id') AS INTEGER) AS node_id,
				CAST(json_extract(item.value, '$.community') AS INTEGER) AS community
			FROM (SELECT cypher('RETURN louvain(%f)') AS result) r,
			json_each(json_extract(r.result, '$[0].column_0')) item
		)
		SELECT lr.community, npt.value AS path
		FROM louvain_raw lr
		JOIN node_labels nl ON nl.node_id = lr.node_id AND nl.label = '%s'
		JOIN node_props_text npt ON npt.node_id = lr.node_id
			AND npt.key_id = (SELECT id FROM property_keys WHERE key = 'path' LIMIT 1)
	`, resolution, NodeFact)

	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, query)
	if err != nil {
		return ClusterResult{}, fmt.Errorf("louvain: %w", err)
	}
	defer rows.Close()

	communities := map[int][]string{}
	for rows.Next() {
		var community int
		var path string
		if err := rows.Scan(&community, &path); err != nil {
			continue
		}
		communities[community] = append(communities[community], path)
	}
	if err := rows.Err(); err != nil {
		return ClusterResult{}, fmt.Errorf("louvain rows: %w", err)
	}

	// Resolve branch to branchID for scoped filtering.
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return ClusterResult{}, fmt.Errorf("ClusterFacts: %w", err)
	}

	// Post-filter: exclude facts not visible on this branch (check existence
	// in `branch_facts` table), then apply minCommunitySize.
	allPaths := make([]string, 0)
	for _, members := range communities {
		allPaths = append(allPaths, members...)
	}
	existingPaths := make(map[string]bool, len(allPaths))
	if len(allPaths) > 0 {
		placeholders := make([]string, len(allPaths))
		args := make([]interface{}, len(allPaths))
		for i, p := range allPaths {
			placeholders[i] = "?"
			args[i] = p
		}
		qry := `SELECT path FROM branch_facts WHERE branch_id = ? AND path IN (` + strings.Join(placeholders, ",") + `)`
		args = append([]interface{}{branchID}, args...)
		eRows, err := conn(ctx, si.rh.db).QueryContext(ctx, qry, args...)
		if err == nil {
			for eRows.Next() {
				var p string
				if eRows.Scan(&p) == nil {
					existingPaths[p] = true
				}
			}
			eRows.Close()
		}
	}

	result := ClusterResult{Clusters: map[int][]string{}}
	for id, members := range communities {
		var alive []string
		for _, path := range members {
			if existingPaths[path] {
				alive = append(alive, path)
			}
		}
		if len(alive) < minCommunitySize {
			result.Noise = append(result.Noise, alive...)
		} else {
			result.Clusters[id] = alive
		}
	}

	return result, nil
}

// graphExpandSearch expands vector search seed results through graph traversal.
// Returns additional fact paths with scores. Graph-discovered facts receive a
// bonus score that decreases with hop distance but is capped below the minimum
// vector seed score to never outrank direct vector hits.
//
// Runs exactly 2 Cypher queries total (one per edge type) regardless of how
// many seeds are provided, using OR-chaining to batch all seed paths.
func (si *searchIndex) graphExpandSearch(ctx context.Context, branchID int64, seeds map[string]float64, maxHops int) map[string]float64 {
	if len(seeds) == 0 {
		return nil
	}

	expanded := map[string]float64{}

	// Find minimum seed score for capping.
	minSeedScore := 1.0
	for _, score := range seeds {
		if score < minSeedScore {
			minSeedScore = score
		}
	}
	capScore := minSeedScore - 0.01

	// Build Cypher path filter using OR-chaining. Each path is JSON-encoded to
	// produce a properly-quoted and escaped Cypher string literal. Parameterized
	// queries are not used here because they do not support variadic OR patterns.
	pathParts := make([]string, 0, len(seeds))
	for p := range seeds {
		b, _ := json.Marshal(p)
		pathParts = append(pathParts, `f.path = `+string(b))
	}
	pathFilter := strings.Join(pathParts, " OR ")

	// Batch query 1: SIMILAR_TO neighbors for all seeds.
	// Use NOT neighbor.deleted = true (instead of = false) because GraphQLite
	// stores booleans as JSON booleans which do not compare equal to Cypher
	// literal false. Nodes that are not deleted have deleted=false (set in
	// graphSyncFact) so this filter correctly excludes soft-deleted nodes.
	q := fmt.Sprintf(
		`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:%s)-[:%s]-(neighbor:%s) WHERE (%s) AND NOT neighbor.deleted = true RETURN DISTINCT neighbor.path AS path'))`,
		NodeFact, EdgeSimilarTo, NodeFact, pathFilter,
	)
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, q)
	if err == nil {
		for rows.Next() {
			var neighborPath string
			rows.Scan(&neighborPath)
			if neighborPath == "" {
				continue
			}
			if _, isSeed := seeds[neighborPath]; !isSeed {
				if existing, ok := expanded[neighborPath]; !ok || capScore > existing {
					expanded[neighborPath] = capScore
				}
			}
		}
		rows.Close()
	}

	// Batch query 2: shared-entity neighbors for all seeds.
	q = fmt.Sprintf(
		`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:%s)-[:%s]->(e:%s)<-[:%s]-(neighbor:%s) WHERE (%s) AND NOT neighbor.deleted = true RETURN DISTINCT neighbor.path AS path'))`,
		NodeFact, EdgeTagged, NodeEntity, EdgeTagged, NodeFact,
		pathFilter,
	)
	rows, err = conn(ctx, si.rh.db).QueryContext(ctx, q)
	if err == nil {
		for rows.Next() {
			var neighborPath string
			rows.Scan(&neighborPath)
			if neighborPath == "" {
				continue
			}
			if _, isSeed := seeds[neighborPath]; !isSeed {
				score := capScore - 0.01
				if existing, ok := expanded[neighborPath]; !ok || score > existing {
					expanded[neighborPath] = score
				}
			}
		}
		rows.Close()
	}

	// Post-filter: keep only paths visible on this branch.
	if len(expanded) > 0 {
		placeholders := make([]string, 0, len(expanded))
		args := make([]interface{}, 0, len(expanded)+1)
		args = append(args, branchID)
		for p := range expanded {
			placeholders = append(placeholders, "?")
			args = append(args, p)
		}
		visible := make(map[string]bool, len(expanded))
		rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
			`SELECT path FROM branch_facts WHERE branch_id = ? AND path IN (`+strings.Join(placeholders, ",")+`)`,
			args...,
		)
		if err == nil {
			for rows.Next() {
				var p string
				rows.Scan(&p)
				visible[p] = true
			}
			rows.Close()
		}
		for p := range expanded {
			if !visible[p] {
				delete(expanded, p)
			}
		}
	}

	return expanded
}

// execer abstracts *sql.DB and *sql.Tx for transactional graph operations.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

// jsonParams encodes a single key-value pair as a JSON object string, for use
// as the second argument to cypher() in parameterized read queries.
// For multiple params, use json.Marshal directly.
func jsonParams(key, value string) string {
	b, _ := json.Marshal(map[string]string{key: value})
	return string(b)
}

// escapeCypherKey escapes a string for use in Cypher MATCH/MERGE property
// patterns (e.g. {path: "value"}) that appear inside a SQL single-quoted string.
// GraphQLite's MATCH parser does not support unicode escapes or SQL '' escaping
// inside property patterns, so single quotes are stripped to avoid breaking the
// SQL string wrapper. Null bytes are stripped as they break the SQL parser.
//
// Note: parameterized queries (cypher('...', params)) work for reads (MATCH)
// but not for writes (MERGE/SET/DELETE) in the installed GraphQLite build, so
// write operations must use this escape approach.
func escapeCypherKey(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, `'`, ``)
	return s
}

// escapeCypherVal escapes a string for use in Cypher SET values
// (e.g. SET f.title = "value"). These are more lenient than MATCH patterns
// and support \u unicode escapes, so single quotes become \u0027.
func escapeCypherVal(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, `'`, `\u0027`)
	return s
}

// ── History graph phase ───────────────────────────────────────────────────────
// History graph phase: creates FactVersion nodes from commit_log entries,
// linking them with PREV_VERSION chains and DERIVED_FROM edges.
//
// GraphQLite limitation: MATCH (a:L {p1: "x"}), (b:L {p1: "y"}) does not
// correctly find two distinct nodes of the same label by property values — it
// degenerates into a self-loop (a)-[:R]->(a). To work around this, PREV_VERSION
// and DERIVED_FROM edges are created via direct SQL INSERT INTO edges after
// looking up node IDs through the EAV property tables.

// graphNodeIDByProp returns the node ID for a node with the given label, where
// the property named propKey equals propVal. Returns 0 if not found.
func (si *searchIndex) graphNodeIDByProp(ctx context.Context, label, propKey, propVal string) (int64, error) {
	var nodeID int64
	err := conn(ctx, si.rh.db).QueryRowContext(ctx, `
		SELECT np.node_id
		FROM node_props_text np
		JOIN property_keys pk ON pk.id = np.key_id
		JOIN node_labels nl ON nl.node_id = np.node_id
		WHERE pk.key = ? AND np.value = ? AND nl.label = ?
		LIMIT 1
	`, propKey, propVal, label).Scan(&nodeID)
	if err != nil {
		return 0, err
	}
	return nodeID, nil
}

// graphInsertEdge inserts an edge directly into the edges table, bypassing
// the GraphQLite Cypher layer. This avoids the two-node MATCH self-loop bug.
func (si *searchIndex) graphInsertEdge(ctx context.Context, sourceID, targetID int64, edgeType string) error {
	_, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`INSERT OR IGNORE INTO edges (source_id, target_id, type) VALUES (?, ?, ?)`,
		sourceID, targetID, edgeType,
	)
	return err
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

// graphSyncFactVersionTx creates a FactVersion node (MERGE only) within the
// given transaction. Properties (title, committed_at) must be set after the
// transaction commits via graphSetFactVersionProps, because GraphQLite's
// MATCH+SET does not persist to EAV tables when executed inside a *sql.Tx.
func (si *searchIndex) graphSyncFactVersionTx(ctx context.Context, tx execer, commitHash string, rec FactRecord, committedAt int64) error {
	p := escapeCypherKey(rec.Path)
	ch := escapeCypherKey(commitHash)

	// MERGE the FactVersion node (identity props only).
	q := fmt.Sprintf(`SELECT cypher('MERGE (v:%s {path: "%s", commit_hash: "%s"})')`,
		NodeFactVersion, p, ch)
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("graphSyncFactVersionTx: merge node: %w", err)
	}
	return nil
}

// graphSetFactVersionProps sets title and committed_at on an existing FactVersion
// node via direct SQL INSERTs into the EAV tables. GraphQLite's MATCH+SET
// silently drops property writes (confirmed: title/committed_at never appear
// in node_props_text or node_props_real after a Cypher SET), so we bypass
// Cypher entirely and INSERT directly into the EAV tables.
//
// Must be called after the transaction that created the node has committed,
// because node IDs are only visible post-commit.
func (si *searchIndex) graphSetFactVersionProps(ctx context.Context, commitHash string, rec FactRecord, committedAt int64) error {
	nodeID, err := si.graphNodeIDByProp(ctx, NodeFactVersion, "commit_hash", commitHash)
	if err != nil || nodeID == 0 {
		return fmt.Errorf("graphSetFactVersionProps: node not found for commit_hash=%s: %w", commitHash, err)
	}

	// Ensure property key IDs exist, then upsert values into EAV tables.
	type textProp struct {
		key   string
		value string
	}
	for _, p := range []textProp{
		{"title", rec.Title},
	} {
		// Ensure property_key row exists.
		if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `INSERT OR IGNORE INTO property_keys(key) VALUES (?)`, p.key); err != nil {
			return fmt.Errorf("graphSetFactVersionProps: ensure key %s: %w", p.key, err)
		}
		var keyID int64
		if err := conn(ctx, si.rh.db).QueryRowContext(ctx, `SELECT id FROM property_keys WHERE key = ?`, p.key).Scan(&keyID); err != nil {
			return fmt.Errorf("graphSetFactVersionProps: get key_id for %s: %w", p.key, err)
		}
		if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
			`INSERT OR REPLACE INTO node_props_text(node_id, key_id, value) VALUES (?, ?, ?)`,
			nodeID, keyID, p.value,
		); err != nil {
			return fmt.Errorf("graphSetFactVersionProps: set text prop %s: %w", p.key, err)
		}
	}

	// committed_at is an integer; store in node_props_real (GraphQLite uses REAL for numbers).
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `INSERT OR IGNORE INTO property_keys(key) VALUES (?)`, "committed_at"); err != nil {
		return fmt.Errorf("graphSetFactVersionProps: ensure key committed_at: %w", err)
	}
	var caKeyID int64
	if err := conn(ctx, si.rh.db).QueryRowContext(ctx, `SELECT id FROM property_keys WHERE key = 'committed_at'`).Scan(&caKeyID); err != nil {
		return fmt.Errorf("graphSetFactVersionProps: get key_id for committed_at: %w", err)
	}
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`INSERT OR REPLACE INTO node_props_real(node_id, key_id, value) VALUES (?, ?, ?)`,
		nodeID, caKeyID, committedAt,
	); err != nil {
		return fmt.Errorf("graphSetFactVersionProps: set committed_at: %w", err)
	}

	return nil
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
	`, BlobObjectType)
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
	emb := si.getEmbedder()
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
		qargs := append([]any{BlobObjectType}, args...)

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
	if si.getEmbedder() != nil {
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

	last, err := si.GetLastCommit(ctx, branch)
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
			if err := si.Delete(ctx, branch, path); err != nil {
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

	return si.Upsert(ctx, branch, commitHash, rec)
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
