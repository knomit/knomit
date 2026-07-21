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

// donatedVec returns the caller-donated embedding for path, if the context
// carries a usable one. Single definition so the pre-tx "do we need to embed?"
// decision and the in-tx "which vector do we store?" decision cannot drift.
func donatedVec(ctx context.Context, path string) ([]float32, bool) {
	vec, ok := precomputedEmbedding(ctx, path)
	return vec, ok && len(vec) > 0
}

// validateDonatedVec checks a caller-supplied embedding against the active
// embedder's dimension and converts it to storage bytes. When no embedder is
// configured we cannot know the expected dim, so the vector is stored as-is.
func validateDonatedVec(emb Embedder, path string, vec []float32) ([]byte, error) {
	if emb != nil && len(vec) != emb.Dim() {
		return nil, fmt.Errorf("upsert: donated embedding for %q has %d dims, expected %d", path, len(vec), emb.Dim())
	}
	log.Debug().Str("path", path).Msg("upsert: using donated embedding")
	return float32SliceToBytes(vec), nil
}

// cowProbe reports whether (rec.Path, rec.BlobHash) is ALREADY fully indexed —
// i.e. whether upsert's in-tx COW check is expected to take the hit path and
// skip facts_vec entirely. It mirrors that check exactly:
//
//	junction rows exist, OR (the record has no entities AND some branch already
//	points at this fact)
//
// It is a HINT, not a decision. It runs outside the write transaction, so a
// concurrent writer can invalidate the answer before the caller takes the lock;
// the authoritative check is still the in-tx one. Its only job is to decide
// whether to pay for an embedding, and both wrong answers are safe: a false
// "indexed" costs an in-tx embed on the miss path, a false "not indexed" costs
// a wasted embedding that the COW-hit path discards.
func (si *searchIndex) cowProbe(ctx context.Context, rec FactRecord) (bool, error) {
	db := conn(ctx, si.rh.db)

	var factID int64
	err := db.QueryRowContext(ctx,
		`SELECT id FROM facts WHERE path = ? AND blob_hash = ?`,
		rec.Path, rec.BlobHash,
	).Scan(&factID)
	if err == sql.ErrNoRows {
		return false, nil // never seen this content — certainly a miss
	}
	if err != nil {
		return false, fmt.Errorf("upsert cow probe: %w", err)
	}

	var junctionExists int
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM fact_entities WHERE fact_id = ? LIMIT 1`, factID,
	).Scan(&junctionExists); err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("upsert cow probe entities: %w", err)
	}
	if junctionExists > 0 {
		return true, nil
	}
	if len(rec.Entities) > 0 {
		return false, nil
	}
	anyBranch, err := hasAnyBranchFact(ctx, db, factID)
	if err != nil {
		return false, fmt.Errorf("upsert cow probe branch: %w", err)
	}
	return anyBranch, nil
}

// embedRecord loads the record's blob and runs the configured embedder over it,
// returning the vector in storage form. Caller must have checked that an
// embedder is configured.
//
// Call this OUTSIDE a write transaction whenever possible: inference takes
// seconds and _txlock=immediate means an open tx holds SQLite's process-wide
// write lock for the duration.
func (si *searchIndex) embedRecord(ctx context.Context, rec FactRecord) ([]byte, error) {
	emb := si.rh.getEmbedder()
	var data []byte
	if err := conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT data FROM objects WHERE hash = ? AND type = ?`,
		rec.BlobHash, blobObjectType,
	).Scan(&data); err != nil {
		return nil, fmt.Errorf("upsert: blob %s not found: %w", rec.BlobHash, err)
	}

	vec, err := emb.EmbedDocument(rec.Title, extractBody(data))
	switch {
	case err != nil:
		return nil, fmt.Errorf("upsert: embed %q failed (embeddings are required): %w", rec.Path, err)
	case len(vec) == 0:
		return nil, fmt.Errorf("upsert: embedder returned empty vector for %q (embeddings are required)", rec.Path)
	case len(vec) != emb.Dim():
		return nil, fmt.Errorf("upsert: embedder produced %d-dim vector for %q, expected %d", len(vec), rec.Path, emb.Dim())
	}
	return float32SliceToBytes(vec), nil
}

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
	factKind := rec.Kind
	if factKind == "" {
		factKind = "epistemic"
	}
	factOrigin := rec.Origin
	if factOrigin == "" {
		factOrigin = "authored"
	}

	// Embedding is resolved BEFORE the transaction opens.
	//
	// The DB runs with _txlock=immediate (see Open), so every BeginTx takes
	// SQLite's process-wide write lock. ONNX inference takes seconds; running it
	// inside the tx held that global lock for the whole inference and blocked
	// every writer on every branch. rebuildEmbeddings has always embedded
	// outside its tx — this is the same rule applied to the incremental path.
	//
	// We still don't want to pay inference on a COW hit, so a read-only probe
	// (outside the tx) decides whether an embedding will be needed. The probe
	// can race a concurrent writer; the authoritative COW check inside the tx is
	// unchanged, and the miss-after-probe-said-hit case falls back to embedding
	// in-tx. That fallback is rare by construction and keeps the "no fact is
	// ever indexed without a vector" invariant absolute.
	//
	// Only INFERENCE is hoisted. A caller-donated vector (mcp/learn's dedup
	// pass) costs nothing to apply, so it stays on the in-tx miss path where it
	// has always been — that keeps a malformed donation from failing an upsert
	// that would have taken the COW-hit path and never looked at it.
	var vecData []byte
	if _, donated := donatedVec(ctx, rec.Path); !donated && si.rh.getEmbedder() != nil {
		indexed, err := si.cowProbe(ctx, rec)
		if err != nil {
			return err
		}
		if !indexed {
			if vecData, err = si.embedRecord(ctx, rec); err != nil {
				return err
			}
		}
	}

	// Begin transaction for atomic COW check + insert.
	ctx, tx, _, err := beginTxIfNeeded(ctx, si.rh.db)
	if err != nil {
		return fmt.Errorf("upsert begin tx: %w", err)
	}
	defer tx.Rollback()
	db := conn(ctx, si.rh.db)

	// Atomic: insert fact if it doesn't exist yet (no TOCTOU race).
	_, err = db.ExecContext(ctx,
		`INSERT OR IGNORE INTO facts(path, blob_hash, title, kind, type, domain, entities, confidence, sources, refs, evidence_weight, origin)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Path, rec.BlobHash, rec.Title, factKind, factType,
		string(domainJSON), string(entitiesJSON),
		rec.Confidence, rec.Sources,
		string(refsJSON), rec.EvidenceWeight, factOrigin,
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
		// Post-commit work uses ctx without the (now-closed) tx so conn(ctx,db)
		// resolves to the bare *sql.DB rather than the committed tx.
		ctx = storegit.WithoutTx(ctx)
		// Write time-aware DERIVED_FROM edges for the new (path, commit) ref-event.
		// Each new commit asserting the same content is its own ref-event.
		si.writePostCommitDerivedFrom(ctx, branch, rec.Path, rec.BlobHash, commitHash, rec.Refs)
		return nil
	}

	// COW miss: populate junction tables, embeddings, branch_facts.
	//
	// vecData was normally resolved before the tx opened. It is still nil here
	// in exactly two cases:
	//   1. NO embedder is configured (read-only tooling / tests) — legitimate;
	//      the running service always has one (app.New makes embeddings
	//      mandatory), so the corpus never gains a vectorless fact this way.
	//   2. The pre-tx probe saw this (path, blob_hash) as already indexed and a
	//      concurrent writer removed it before we took the write lock. Rare, and
	//      we must not proceed without a vector, so we embed here — paying the
	//      cost this fix exists to avoid, but only on a genuine race.
	// INVARIANT: when an embedder is configured, no fact is ever indexed
	// without a vector — any embed failure FAILS the upsert (rolls back the
	// whole write) rather than silently producing a vectorless fact, which
	// would be invisible to vector search and corrupt the corpus's retrieval
	// guarantees.
	if vecData == nil {
		emb := si.rh.getEmbedder()
		if vec, ok := donatedVec(ctx, rec.Path); ok {
			if vecData, err = validateDonatedVec(emb, rec.Path, vec); err != nil {
				return err
			}
		} else if emb != nil {
			log.Debug().Str("path", rec.Path).Str("blob_hash", rec.BlobHash).
				Msg("upsert: pre-tx probe raced a concurrent writer; embedding inside the write tx")
			if vecData, err = si.embedRecord(ctx, rec); err != nil {
				return err
			}
		}
	}

	for _, entity := range rec.Entities {
		if entity == "" {
			continue
		}
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO fact_entities(fact_id, entity) VALUES (?, ?)`,
			factID, entity,
		); err != nil {
			return fmt.Errorf("upsert fact_entities: %w", err)
		}
	}
	for _, domain := range rec.Domain {
		canon := canonicalizeDomain(domain)
		if canon == "" {
			continue
		}
		// Store the canonical form (NFC + fold + de-hyphenize) so case/space/
		// hyphen variants unify for filtering.
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO fact_domains(fact_id, domain) VALUES (?, ?)`,
			factID, canon,
		); err != nil {
			return fmt.Errorf("upsert fact_domains: %w", err)
		}
		// Token containment index: one row per stemmed token of this domain.
		for _, tok := range domainTokens(canon) {
			if _, err := db.ExecContext(ctx,
				`INSERT OR IGNORE INTO fact_domain_tokens(fact_id, domain, token) VALUES (?, ?, ?)`,
				factID, canon, tok,
			); err != nil {
				return fmt.Errorf("upsert fact_domain_tokens: %w", err)
			}
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
	//
	// DERIVED_FROM edges are an exception — they run post-commit via
	// writePostCommitDerivedFrom because graphAddDerivedFromAtCommitTx
	// requires node IDs that are only visible after the source Fact's
	// MERGE has committed.
	if err := si.graphSyncFactTx(ctx, tx, rec); err != nil {
		return fmt.Errorf("upsert graph sync: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Post-commit work uses ctx without the (now-closed) tx so conn(ctx,db)
	// resolves to the bare *sql.DB rather than the committed tx.
	ctx = storegit.WithoutTx(ctx)

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
		`SELECT f.path, f.title, f.blob_hash, f.kind, f.type, f.domain, f.entities,
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
// Expected column order: path, title, blob_hash, kind, type, domain, entities,
// confidence, sources, refs, evidence_weight, commit_hash, data, committed_at.
func scanFactWithBody(row *sql.Row) (*FactWithBody, error) {
	var f FactWithBody
	var domainJSON, entitiesJSON, refsJSON string
	var rawData []byte
	var committedAt sql.NullInt64
	err := row.Scan(
		&f.Path, &f.Title, &f.BlobHash, &f.Kind, &f.Type,
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
	logFactJSONUnmarshal("scanFactWithBody", f.Path, domainJSON, entitiesJSON, refsJSON, &f.Domain, &f.Entities, &f.Refs)
	f.Body = extractBody(rawData)
	if committedAt.Valid {
		f.CommittedAt = committedAt.Int64
	}
	return &f, nil
}

// scanFactRecordFromRowsWithCommittedAt scans a *FactWithBody from *sql.Rows,
// including commit_hash and committed_at (fields absent from FactRecord).
// Expected column order: path, title, blob_hash, kind, type, domain, entities,
// confidence, sources, refs, evidence_weight, commit_hash, committed_at.
func scanFactRecordFromRowsWithCommittedAt(rows *sql.Rows) (*FactWithBody, error) {
	var f FactWithBody
	var domainJSON, entitiesJSON, refsJSON string
	err := rows.Scan(
		&f.Path, &f.Title, &f.BlobHash, &f.Kind, &f.Type,
		&domainJSON, &entitiesJSON,
		&f.Confidence, &f.Sources,
		&refsJSON, &f.EvidenceWeight, &f.CommitHash, &f.CommittedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scanFactRecordFromRowsWithCommittedAt: %w", err)
	}
	logFactJSONUnmarshal("scanFactRecordFromRowsWithCommittedAt", f.Path, domainJSON, entitiesJSON, refsJSON, &f.Domain, &f.Entities, &f.Refs)
	return &f, nil
}

// scanFactWithBodyFromRowsWithCommittedAt scans a *FactWithBody from *sql.Rows,
// including the body (raw object data) and committed_at timestamp.
// Expected column order: path, title, blob_hash, kind, type, domain, entities,
// confidence, sources, refs, evidence_weight, commit_hash, data, committed_at.
func scanFactWithBodyFromRowsWithCommittedAt(rows *sql.Rows) (*FactWithBody, error) {
	var f FactWithBody
	var domainJSON, entitiesJSON, refsJSON string
	var rawData []byte
	err := rows.Scan(
		&f.Path, &f.Title, &f.BlobHash, &f.Kind, &f.Type,
		&domainJSON, &entitiesJSON,
		&f.Confidence, &f.Sources,
		&refsJSON, &f.EvidenceWeight, &f.CommitHash, &rawData, &f.CommittedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scanFactWithBodyFromRowsWithCommittedAt: %w", err)
	}
	logFactJSONUnmarshal("scanFactWithBodyFromRowsWithCommittedAt", f.Path, domainJSON, entitiesJSON, refsJSON, &f.Domain, &f.Entities, &f.Refs)
	f.Body = extractBody(rawData)
	return &f, nil
}

// logFactJSONUnmarshal decodes the three JSON columns (domain, entities, refs)
// on a fact row and logs at Warn for any column that fails to parse.
//
// We don't propagate the error: a malformed JSON column on one fact shouldn't
// fail the whole query, and the columns are written from typed Go slices on
// upsert (search_crud.go:upsert), so a parse error means storage was corrupted
// out-of-band. Logging surfaces that corruption in the server log instead of
// silently returning empty slices to callers — combined with the
// `INSERT OR IGNORE` dedup at fact_entities/fact_domains, a corrupt entities
// column would otherwise make `?entity=…` queries return nothing for a fact
// that should match.
func logFactJSONUnmarshal(scanner, path, domainJSON, entitiesJSON, refsJSON string, domain *[]string, entities *[]string, refs *[]string) {
	if err := json.Unmarshal([]byte(domainJSON), domain); err != nil {
		log.Warn().Err(err).Str("scanner", scanner).Str("path", path).Str("column", "domain").Msg("fact JSON column unmarshal failed; field empty")
	}
	if err := json.Unmarshal([]byte(entitiesJSON), entities); err != nil {
		log.Warn().Err(err).Str("scanner", scanner).Str("path", path).Str("column", "entities").Msg("fact JSON column unmarshal failed; field empty")
	}
	if err := json.Unmarshal([]byte(refsJSON), refs); err != nil {
		log.Warn().Err(err).Str("scanner", scanner).Str("path", path).Str("column", "refs").Msg("fact JSON column unmarshal failed; field empty")
	}
}
