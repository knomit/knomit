package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"

	storegit "knomit/internal/store/git"
)

// ── CRUD operations ───────────────────────────────────────────────────────────
// CRUD operations on the facts table: insert/update, delete, get by path,
// embedding retrieval, and meta key-value storage (last_commit tracking).
// All mutations keep the vec0 index in sync within transactions.

// factsVecDim must match the FLOAT[N] dimension declared on the
// facts_vec virtual table in 000002_facts_vec.up.sql. Any donated or
// freshly computed embedding vector with a different length cannot be
// stored — the schema is a hard invariant.
const factsVecDim = 768

// upsert inserts or replaces a FactRecord on the given branch, keeping the
// vec0 index in sync. COW dedup: if (path, blob_hash) already exists in the
// facts table, only the branch_facts pointer is updated.
func (si *searchIndex) upsert(ctx context.Context, branch, commitHash string, rec FactRecord) error {
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

	// Embedding is computed below, AFTER the COW check, so we don't pay
	// ONNX inference cost for facts whose (path, blob_hash) is already
	// indexed (the COW-hit path returns without touching facts_vec).
	// vecData stays nil here and is populated only on the COW-miss path.
	var vecData []byte

	// Begin transaction for atomic COW check + insert.
	ctx, tx, _, err := beginTxIfNeeded(ctx, si.rh.db)
	if err != nil {
		return fmt.Errorf("upsert begin tx: %w", err)
	}
	defer tx.Rollback()
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
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM fact_entities WHERE fact_id = ? LIMIT 1`, factID,
	).Scan(&junctionExists); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("upsert cow check: %w", err)
	}
	anyBranch, err := hasAnyBranchFact(ctx, db, factID)
	if err != nil {
		return fmt.Errorf("upsert cow check branch: %w", err)
	}
	if junctionExists > 0 || (len(rec.Entities) == 0 && anyBranch) {
		// COW hit: fact fully indexed (junction tables + facts_vec
		// already populated under this fact_id), just update branch
		// pointer. No embedding needed — the existing facts_vec row
		// is the right vector for this (path, blob_hash).
		_, err = db.ExecContext(ctx,
			`INSERT OR REPLACE INTO branch_facts(branch_id, path, fact_id, commit_hash) VALUES (?, ?, ?, ?)`,
			branchID, rec.Path, factID, commitHash)
		if err != nil {
			return fmt.Errorf("upsert branch_facts (cow hit): %w", err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		// Write time-aware DERIVED_FROM edges for the new (path, commit) ref-event.
		// Each new commit asserting the same content is its own ref-event.
		si.writePostCommitDerivedFrom(ctx, branch, rec.Path, rec.BlobHash, commitHash, rec.Refs)
		return nil
	}

	// COW miss: populate junction tables, embeddings, branch_facts.
	//
	// Embedding source preference (cheapest first):
	//   1. context-donated vector (caller already embedded this content,
	//      e.g. mcp/learn during its dedup pass);
	//   2. fresh ONNX inference via the configured embedder.
	// Either source produces a 768-dim []float32; vecData stays nil if
	// neither is available, in which case the fact is indexed without a
	// vector (search-by-text on this fact will still work via the
	// keyword path — sqlite-vec just won't return it).
	if vec, ok := precomputedEmbedding(ctx, rec.Path); ok && len(vec) > 0 {
		if len(vec) != factsVecDim {
			return fmt.Errorf("upsert: donated embedding for %q has %d dims, expected %d", rec.Path, len(vec), factsVecDim)
		}
		vecData = float32SliceToBytes(vec)
		log.Debug().Str("path", rec.Path).Str("blob_hash", rec.BlobHash).Msg("upsert: using donated embedding")
	} else if emb := si.rh.getEmbedder(); emb != nil {
		var data []byte
		err := db.QueryRowContext(ctx,
			`SELECT data FROM objects WHERE hash = ? AND type = ?`,
			rec.BlobHash, blobObjectType,
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

	// Sync graph: create/update nodes and edges for this fact. Runs INSIDE
	// the transaction so the facts row and its matching live Fact node are
	// committed atomically. Previously this ran post-commit with the error
	// swallowed by log.Warn — which produced graph-coherence holes under
	// concurrent writes (one writer's facts row committed while another's
	// graph sync races or fails). The graph-coherence Verify check
	// catches those holes, so any failure here must propagate.
	if err := si.graphSyncFactTx(ctx, tx, rec); err != nil {
		return fmt.Errorf("upsert graph sync: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Write time-aware DERIVED_FROM edges. Runs AFTER commit because direct-SQL
	// reads against the GraphQLite EAV tables cannot see nodes MERGE'd via
	// Cypher inside the same *sql.Tx — node IDs only become visible
	// post-commit. Edge-write failure is non-fatal; logged so verify can flag
	// missing edges later.
	si.writePostCommitDerivedFrom(ctx, branch, rec.Path, rec.BlobHash, commitHash, rec.Refs)

	// Build similarity edges if embeddings are available. This runs AFTER
	// commit because it queries facts_vec rows that must be visible outside
	// the tx; a failure here is non-fatal because similarity edges are not
	// covered by Verify.
	if si.rh.getEmbedder() != nil {
		if err := si.graphBuildSimilarityEdges(ctx, rec.Path, rec.BlobHash); err != nil {
			log.Warn().Err(err).Str("path", rec.Path).Msg("graph similarity edges failed")
		}
	}

	return nil
}

// writePostCommitDerivedFrom writes time-aware DERIVED_FROM edges for the
// given (path, blob_hash) at commitHash. Filters refs to local-only.
// Logged on failure rather than returned: edges are recoverable via
// rebuild and verify will flag any holes.
func (si *searchIndex) writePostCommitDerivedFrom(ctx context.Context, branch, path, blobHash, commitHash string, refs []string) {
	var localRefs []string
	for _, r := range refs {
		if !strings.HasPrefix(r, "http://") && !strings.HasPrefix(r, "https://") {
			localRefs = append(localRefs, r)
		}
	}
	if len(localRefs) == 0 {
		return
	}
	if err := si.graphAddDerivedFromAtCommitTx(ctx, si.rh.db, branch, path, blobHash, commitHash, localRefs); err != nil {
		log.Warn().Err(err).Str("path", path).Str("commit", commitHash).Msg("post-commit DERIVED_FROM edge write failed")
	}
}

// hasAnyBranchFact checks if any branch_facts row exists for the given fact_id.
func hasAnyBranchFact(ctx context.Context, db storegit.CtxExecer, factID int64) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM branch_facts WHERE fact_id = ? LIMIT 1`, factID).Scan(&n)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("hasAnyBranchFact: %w", err)
	}
	return n > 0, nil
}

// delete removes a fact from the given branch. If no other branch references
// the fact, the underlying facts row (and its vec/graph data) is also deleted.
func (si *searchIndex) delete(ctx context.Context, branch, path string) error {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}

	ctx, tx, _, err := beginTxIfNeeded(ctx, si.rh.db)
	if err != nil {
		return fmt.Errorf("delete begin tx: %w", err)
	}
	defer tx.Rollback()
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
		return tx.Commit()
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
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM branch_facts WHERE fact_id = ?`, factID,
	).Scan(&refCount); err != nil {
		return fmt.Errorf("delete refcount: %w", err)
	}

	if refCount == 0 {
		// Orphaned: delete the fact (cascades to junction tables, triggers facts_vec).
		if _, err := db.ExecContext(ctx,
			`DELETE FROM facts WHERE id = ?`, factID,
		); err != nil {
			return fmt.Errorf("delete fact: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
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
		 WHERE bf.branch_id = ? AND bf.path = ?`, blobObjectType, branchID, path,
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
