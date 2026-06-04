package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/sync/singleflight"
)

// GraphSchemaVersion is the expected version of the GraphQLite graph layout.
// Incremented when the graph schema changes in a way that requires a forced
// rebuild on existing deployments. Persisted in meta.graph_schema_version
// after every successful Rebuild; checked on Open.
//
// Version 2: DERIVED_FROM edges carry source_commit + target_commit
// properties (commit-anchored /incoming + /outgoing); FactVersion subsystem
// retired.
//
// Version 3: domain tags are stored CANONICAL (NFC + case-fold + de-hyphenize)
// in fact_domains and tokenised into fact_domain_tokens. Existing deployments
// indexed under v2 hold RAW domains and an empty token table, so domain search
// silently returns nothing until the derived state is regenerated — the version
// bump forces that rebuild on startup (see repoBuilder.setupIndex). Despite the
// "Graph" name this constant gates ALL forced-rebuild-on-upgrade derived state,
// graph or not; bump it whenever an indexing-logic change needs an existing
// deployment to reindex.
const GraphSchemaVersion = "3"

type searchIndex struct {
	rh *repoHandler

	// clusterSF deduplicates concurrent CachedClusterFacts compute paths
	// keyed by branch|resolution|minCommunitySize. Two concurrent reviews
	// (or a review + the background checker) on the same key collapse to
	// one Louvain run; both wait on the singleflight result.
	clusterSF singleflight.Group
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

// SyncWatermark returns the last processed commit hash for the given branch,
// or "" if not set.
func (si *searchIndex) SyncWatermark(ctx context.Context, branch string) (string, error) {
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

// ── Sync ──────────────────────────────────────────────────────────────────────
// Git sync: keeps the search index up to date with the git-backed fact store.
// Supports both full rebuilds (first run) and incremental updates (diffing
// since the last indexed commit).

// NeedsRebuild reports whether the persisted index schema version differs from
// the current GraphSchemaVersion — i.e. derived state (graph layout, canonical
// domains, fact_domain_tokens) was written by an older version and a full
// Rebuild is required to regenerate it. The version is meta.graph_schema_version,
// global to the DB (not per-branch); a missing row (fresh or pre-versioning DB)
// counts as stale. Rebuild bumps it on success.
func (si *searchIndex) NeedsRebuild(ctx context.Context) (bool, error) {
	var persistedVer string
	err := conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'graph_schema_version'`,
	).Scan(&persistedVer)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read graph_schema_version: %w", err)
	}
	return persistedVer != GraphSchemaVersion, nil
}

// MarkRebuildNeeded clears the persisted schema version so the next
// NeedsRebuild reports stale. It exists to undo a premature version bump after
// a partially-failed multi-branch heal: Rebuild bumps the GLOBAL
// meta.graph_schema_version on each branch it completes, so an earlier branch's
// success would otherwise mask a later branch's failure (the version reads
// current, suppressing the retry), leaving that branch's canonical domains /
// tokens stale permanently. Re-marking forces the next startup to heal again.
func (si *searchIndex) MarkRebuildNeeded(ctx context.Context) error {
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`DELETE FROM meta WHERE key = 'graph_schema_version'`,
	); err != nil {
		return fmt.Errorf("mark rebuild needed: %w", err)
	}
	return nil
}

// Sync brings the index up to date with the git store.
//
// Algorithm:
//  1. Read meta.last_commit.
//  2. If missing → full rebuild (ListAll, index everything).
//  3. If last_commit == HEAD → no-op.
//  4. Else → DiffFiles(last_commit), upsert added+modified, delete removed.
//  5. Update meta.last_commit = HEAD.
func (si *searchIndex) Sync(ctx context.Context, branch string) error {
	// Detect schema mismatch on entry. An older deployment may have derived
	// state (graph layout, canonical domains, token table) laid out by a
	// previous version. Callers that orchestrate startup (repoBuilder) call
	// NeedsRebuild and Rebuild to heal it; Sync only warns, because it cannot
	// safely escalate to a full rebuild for every branch from here.
	if stale, err := si.NeedsRebuild(ctx); err != nil {
		return fmt.Errorf("sync: %w", err)
	} else if stale {
		log.Warn().
			Str("repo", si.rh.name).
			Str("expected", GraphSchemaVersion).
			Msg("index schema version mismatch — run `knomit rebuild` to regenerate derived state")
	}

	// Ensure the branch exists in the branches table.
	if _, err := si.rh.EnsureBranch(ctx, branch, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("sync: ensure branch: %w", err)
	}

	head, err := si.rh.HeadCommit(ctx, branch)
	if err != nil {
		return fmt.Errorf("sync: head commit: %w", err)
	}

	last, err := si.SyncWatermark(ctx, branch)
	if err != nil {
		return fmt.Errorf("sync: get last commit: %w", err)
	}

	if last == head {
		log.Debug().Str("head", head[:8]).Msg("index sync: already at HEAD, skipping")
		return nil
	}

	// indexed collects every FactRecord successfully upserted in this sync
	// pass. After all fact nodes are committed (pass 1), a second pass writes
	// DERIVED_FROM edges for all of them. This two-pass order is required for
	// intra-commit refs: when A.md and B.md land in the same git commit and
	// A.md refs B.md, B.md's Fact node must exist before A.md's outgoing edge
	// can be wired. Pass 1 commits all nodes; pass 2 retries all edges (with
	// the dedup guard in graphAddDerivedFromAtCommitTx preventing duplicates
	// for refs that were already wired successfully in pass 1).
	var indexed []FactRecord

	if last == "" {
		// Full rebuild: no previous commit recorded, so index every file.
		log.Info().Str("head", head[:8]).Msg("index sync: full rebuild (no previous commit)")
		paths, err := si.rh.ListAll(ctx, branch)
		if err != nil {
			return fmt.Errorf("sync: list all: %w", err)
		}
		for _, path := range paths {
			rec, err := si.indexFile(ctx, branch, path, head)
			if err != nil {
				return err
			}
			if rec != nil {
				indexed = append(indexed, *rec)
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
			rec, err := si.indexFile(ctx, branch, path, head)
			if err != nil {
				return err
			}
			if rec != nil {
				indexed = append(indexed, *rec)
			}
		}
		for _, path := range deleted {
			if err := si.delete(ctx, branch, path); err != nil {
				return fmt.Errorf("sync: delete %q: %w", path, err)
			}
		}
	}

	// Pass 2: retry DERIVED_FROM edge writes for every fact indexed above.
	// This resolves intra-commit refs that were skipped in pass 1 because the
	// target Fact node had not been committed yet at that point. The dedup
	// guard in graphAddDerivedFromAtCommitTx makes these calls idempotent —
	// edges already written in pass 1 are not duplicated.
	for _, rec := range indexed {
		si.writePostCommitDerivedFrom(ctx, branch, rec.Path, rec.BlobHash, rec.SourceCommit, rec.Refs)
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
	graphed, err := si.rebuildGraph(ctx, branch, progress)
	if err != nil {
		return fmt.Errorf("rebuild: graph: %w", err)
	}
	log.Info().Int("graphed", graphed).Str("elapsed", fmt.Sprintf("%.1fs", time.Since(start).Seconds())).Msg("rebuild: phase 3 (graph) complete")

	if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('graph_schema_version', ?)`,
		GraphSchemaVersion,
	); err != nil {
		return fmt.Errorf("rebuild: bump graph schema version: %w", err)
	}

	return si.setLastCommit(ctx, branch, head)
}

// indexFile reads a single file from git, parses it as a fact, and upserts
// it into the index. Files that fail to parse (e.g. kb.md manifest) are
// silently skipped.
//
// commitHash is the fallback; if commit_log has a more specific last-touch
// commit for this path, that is used instead.
//
// Returns the FactRecord that was upserted, or nil if the file was skipped
// (not a fact file). The commit stored in FactRecord.SourceCommit is the
// resolved per-path commit, NOT the fallback passed in.
func (si *searchIndex) indexFile(ctx context.Context, branch, path, commitHash string) (*FactRecord, error) {
	content, blobHash, err := si.rh.readFileWithHash(ctx, branch, path)
	if err != nil {
		return nil, fmt.Errorf("indexFile: read %s: %w", path, err)
	}

	// Use the most recent non-merge commit that touched this file.
	if last, lerr := si.rh.LastCommitForPath(ctx, branch, path); lerr == nil && last != "" {
		commitHash = last
	}

	rec, err := parseFact(path, content)
	if err != nil {
		return nil, nil // not a fact file (e.g. kb.md manifest, ontology.yaml)
	}
	rec.BlobHash = blobHash
	rec.SourceCommit = commitHash

	return &rec, si.upsert(ctx, branch, commitHash, rec)
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
				return fmt.Errorf("gc: graph delete fact %q: %w", o.path, err)
			}
		}
	}

	// 3. Clean up orphaned Entity nodes (no TAGGED edges from any living Fact).
	if err := si.gcOrphanedGraphNodes(ctx, NodeEntity, EdgeTagged); err != nil {
		return fmt.Errorf("gc: orphaned entities: %w", err)
	}

	// 4. Clean up orphaned Domain nodes (no IN_DOMAIN edges from any living Fact).
	if err := si.gcOrphanedGraphNodes(ctx, NodeDomain, EdgeInDomain); err != nil {
		return fmt.Errorf("gc: orphaned domains: %w", err)
	}

	// 5. Clean up orphaned OntologyNode nodes (no UNDER edges from any living Fact).
	if err := si.gcOrphanedGraphNodes(ctx, NodeOntologyNode, EdgeUnder); err != nil {
		return fmt.Errorf("gc: orphaned ontology nodes: %w", err)
	}

	// 6. Branch-scoped commit_log cleanup is handled automatically by
	// branch_commits ON DELETE CASCADE when a branch row is dropped.

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
func (si *searchIndex) gcOrphanedGraphNodes(ctx context.Context, label, edgeType string) error {
	q := fmt.Sprintf(
		`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (n:%s) WHERE NOT (:%s)-[:%s]->(n) RETURN n.path AS path'))`,
		label, NodeFact, edgeType,
	)
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("gc orphaned %s query: %w", label, err)
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
			return fmt.Errorf("gc orphaned %s delete %q: %w", label, path, err)
		}
	}
	return rows.Err()
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

	// The whole of phase 1 — facts upsert + derived junction/token regeneration
	// + branch_facts — runs in ONE transaction. This is load-bearing: the
	// derived-state regeneration below clears tables before repopulating them
	// (e.g. the global fact_domain_tokens rebuild), so without an enclosing tx a
	// mid-rebuild failure or a concurrent WAL reader would observe an empty
	// table. beginTxIfNeeded composes with an outer tx if a caller ever supplies
	// one; otherwise we own and commit it here.
	ctx, tx, ownTx, err := beginTxIfNeeded(ctx, si.rh.db)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: begin tx: %w", err)
	}
	if ownTx {
		defer tx.Rollback()
	}

	// Create temp table for the rebuild entries.
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS _rebuild_entries (path TEXT PRIMARY KEY, blob_hash TEXT NOT NULL)`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: create temp table: %w", err)
	}
	// Ensure it's empty (in case of prior incomplete rebuild).
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `DELETE FROM _rebuild_entries`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear temp table: %w", err)
	}

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

	// Drop stale branch_facts pointers for paths no longer present at HEAD; the
	// INSERT OR REPLACE below repopulates the live ones. Rows are NOT cleared to
	// dodge an FK violation (facts rowids are preserved — see below), only to
	// purge paths that vanished from the branch.
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `DELETE FROM branch_facts WHERE branch_id = ?`, branchID); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear branch_facts: %w", err)
	}

	// Upsert facts from parsed blob data via ON CONFLICT(path, blob_hash) DO
	// UPDATE — NOT INSERT OR REPLACE. facts rows are content-addressed by
	// UNIQUE(path, blob_hash); an unchanged blob refreshes its parsed columns
	// IN PLACE, preserving the rowid. That matters: INSERT OR REPLACE does
	// DELETE+INSERT, which mints a new rowid and fires facts_after_delete (wiping
	// the facts_vec row), so rebuildEmbeddings would re-embed the ENTIRE corpus
	// on every rebuild. Preserving the rowid keeps the embedding (immutable for a
	// given blob — see the COW invariant) and re-embeds only genuinely new/changed
	// facts. A changed blob is a fresh (path, new_hash) row and embeds normally.
	res, err := conn(ctx, si.rh.db).ExecContext(ctx, `
		WITH parsed_entries AS (
			SELECT e.path, e.blob_hash, knomit_parse_fact(o.data) AS parsed
			FROM _rebuild_entries e
			JOIN objects o ON o.hash = e.blob_hash AND o.type = ?
		)
		INSERT INTO facts (path, blob_hash, title, kind, type, domain, entities, confidence, sources, refs, evidence_weight)
		SELECT
			pe.path,
			pe.blob_hash,
			json_extract(pe.parsed, '$.title'),
			COALESCE(json_extract(pe.parsed, '$.kind'), 'epistemic'),
			json_extract(pe.parsed, '$.type'),
			json_extract(pe.parsed, '$.domain'),
			json_extract(pe.parsed, '$.entities'),
			json_extract(pe.parsed, '$.confidence'),
			json_extract(pe.parsed, '$.sources'),
			json_extract(pe.parsed, '$.refs'),
			COALESCE(json_extract(pe.parsed, '$.evidence_weight'), 0)
		FROM parsed_entries pe
		WHERE pe.parsed IS NOT NULL
		ON CONFLICT(path, blob_hash) DO UPDATE SET
			title           = excluded.title,
			kind            = excluded.kind,
			type            = excluded.type,
			domain          = excluded.domain,
			entities        = excluded.entities,
			confidence      = excluded.confidence,
			sources         = excluded.sources,
			refs            = excluded.refs,
			evidence_weight = excluded.evidence_weight
	`, blobObjectType)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: upsert facts: %w", err)
	}

	affected, _ := res.RowsAffected()
	n := int(affected)

	// Re-derive the fact_domains / fact_entities junction tables for the rebuilt
	// facts. Because the upsert above preserves rowids, no cascade-delete fires,
	// so we explicitly clear the rebuilt facts' junction rows first (scoped by
	// _rebuild_entries — NOT a global wipe, so other branches' shared facts keep
	// their rows). The search filter path reads from these junctions; if they
	// drift from the JSON columns, domain/entity searches silently return zero
	// hits. Store the CANONICAL domain (knomit_canon_domain) so case/space/hyphen
	// variants unify; OR IGNORE + DISTINCT because two authored variants can
	// canonicalise to the same value (e.g. "AI Governance" / "ai-governance").
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `
		DELETE FROM fact_domains WHERE fact_id IN (
			SELECT f.id FROM facts f
			JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		)
	`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear fact_domains: %w", err)
	}
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `
		INSERT OR IGNORE INTO fact_domains(fact_id, domain)
		SELECT DISTINCT f.id, knomit_canon_domain(j.value)
		FROM facts f
		JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		JOIN json_each(f.domain) j
		WHERE j.value IS NOT NULL AND j.value != '' AND knomit_canon_domain(j.value) != ''
	`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: repopulate fact_domains: %w", err)
	}
	// Populate the token containment table from the canonical authored domains of
	// EVERY fact version (one-to-many tokenisation can't be expressed in the bulk
	// SQL, so cursor in Go — cheap). Runs inside this transaction, so the global
	// clear-then-repopulate it performs is atomic to concurrent readers.
	if err := si.repopulateDomainTokens(ctx); err != nil {
		return 0, err
	}
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `
		DELETE FROM fact_entities WHERE fact_id IN (
			SELECT f.id FROM facts f
			JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		)
	`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear fact_entities: %w", err)
	}
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `
		INSERT OR IGNORE INTO fact_entities(fact_id, entity)
		SELECT DISTINCT f.id, j.value
		FROM facts f
		JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		JOIN json_each(f.entities) j
		WHERE j.value IS NOT NULL AND j.value != ''
	`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: repopulate fact_entities: %w", err)
	}

	// Populate branch_facts: link each fact to this branch with its commit_hash.
	// We pick the most recent commit_log row per path whose commit is visible on
	// this branch (via branch_commits).
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `
		INSERT OR REPLACE INTO branch_facts (branch_id, path, fact_id, commit_hash)
		SELECT ?, f.path, f.id, COALESCE(cl.commit_hash, '')
		FROM facts f
		JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		LEFT JOIN (
			SELECT cl.path, cl.commit_hash,
			       ROW_NUMBER() OVER (PARTITION BY cl.path ORDER BY cl.committed_at DESC) AS rn
			FROM commit_log cl
			JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
			WHERE bc.branch_id = ?
		) cl ON cl.path = f.path AND cl.rn = 1
	`, branchID, branchID); err != nil {
		return 0, fmt.Errorf("rebuildFacts: populate branch_facts: %w", err)
	}

	// Clean up temp table.
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `DROP TABLE IF EXISTS _rebuild_entries`); err != nil {
		log.Warn().Err(err).Msg("rebuildFacts: drop temp table")
	}

	if ownTx {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("rebuildFacts: commit: %w", err)
		}
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
	emb := si.rh.getEmbedder()
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
		qargs := append([]any{blobObjectType}, args...)

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
func (si *searchIndex) rebuildGraph(ctx context.Context, branch string, progress RebuildProgress) (int, error) {
	// Read all facts ordered by oldest commit first so that when a fact's
	// DERIVED_FROM edges are created, its ref targets are already graph nodes.
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, `
		SELECT f.path, f.title, f.blob_hash, f.kind, f.type, f.domain, f.entities, f.confidence, f.sources, f.refs, f.evidence_weight
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
		if err := rows.Scan(&rec.Path, &rec.Title, &rec.BlobHash, &rec.Kind, &rec.Type,
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

	// Phase A.5: ensure every historical (path, blob_hash) ever indexed on
	// this branch has a graph Fact node, even if the underlying `facts`
	// row was GC'd after the fact was updated/retracted. Without this,
	// Phase B below cannot write DERIVED_FROM edges from orphaned source
	// blobs, leaving the temporal graph internally inconsistent (the edges
	// it CAN write are missing endpoints for the edges it CANNOT).
	//
	// We dedup by (path, blob_hash) since the same blob can appear in
	// multiple commits (no-op recommits, merges that don't change content).
	// Versions still in `facts` are skipped — Phase A above already gave
	// them live nodes.
	currentSet := make(map[string]struct{}, len(facts))
	for _, f := range facts {
		currentSet[f.Path+"|"+f.BlobHash] = struct{}{}
	}
	histRows, err := conn(ctx, si.rh.db).QueryContext(ctx, `
		SELECT cl.commit_hash, cl.path
		FROM commit_log cl
		JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
		WHERE bc.branch_id = (SELECT id FROM branches WHERE name = ?)
		  AND cl.action != 'deleted'
		ORDER BY cl.committed_at ASC, cl.rowid ASC`,
		branch)
	if err != nil {
		return 0, fmt.Errorf("rebuildGraph phaseA.5: query commit_log: %w", err)
	}
	type histKey struct {
		commitHash string
		path       string
	}
	var histEntries []histKey
	for histRows.Next() {
		var h histKey
		if err := histRows.Scan(&h.commitHash, &h.path); err != nil {
			histRows.Close()
			return 0, fmt.Errorf("rebuildGraph phaseA.5: scan: %w", err)
		}
		histEntries = append(histEntries, h)
	}
	histRows.Close()

	seenHistorical := make(map[string]struct{})
	historicalSynced := 0
	for _, h := range histEntries {
		blobHash, err := si.rh.readBlobHashAtCommit(ctx, h.path, h.commitHash)
		if err != nil {
			log.Debug().Err(err).Str("path", h.path).Str("commit", h.commitHash[:8]).
				Msg("rebuildGraph phaseA.5: skip (blob_hash lookup failed)")
			continue
		}
		key := h.path + "|" + blobHash
		if _, ok := currentSet[key]; ok {
			continue // current version, Phase A already handled
		}
		if _, ok := seenHistorical[key]; ok {
			continue // already MERGE'd this historical version
		}
		seenHistorical[key] = struct{}{}

		content, err := si.rh.readFileAtCommit(ctx, h.path, h.commitHash)
		if err != nil {
			log.Debug().Err(err).Str("path", h.path).Str("commit", h.commitHash[:8]).
				Msg("rebuildGraph phaseA.5: skip (file not at commit)")
			continue
		}
		rec, err := parseFact(h.path, content)
		if err != nil {
			log.Debug().Err(err).Str("path", h.path).Msg("rebuildGraph phaseA.5: skip (parse failed)")
			continue
		}
		rec.BlobHash = blobHash
		if err := si.graphSyncHistoricalFactTx(ctx, tx, rec); err != nil {
			log.Warn().Err(err).Str("path", h.path).Str("blob", blobHash[:8]).
				Msg("rebuildGraph phaseA.5: historical sync failed")
			continue
		}
		historicalSynced++
	}
	if historicalSynced > 0 {
		log.Info().Int("historical_facts", historicalSynced).Msg("rebuildGraph phaseA.5: restored historical Fact nodes")
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rebuildGraph: commit: %w", err)
	}

	// Phase B: walk commit_log to write DERIVED_FROM edges per ref-event.
	// Each (commit_hash, path, action != 'deleted') tuple is one ref-event:
	// the version of `path` committed at `commit_hash` had its blob's refs
	// asserted at that time. We resolve each ref's target via
	// resolveTargetCommit (called from inside graphAddDerivedFromAtCommitTx).
	//
	// Runs POST-commit because graphAddDerivedFromAtCommitTx uses direct-SQL
	// reads against the GraphQLite EAV tables, which cannot see Fact nodes
	// MERGE'd via Cypher inside the same *sql.Tx.
	//
	// We pass si.rh.db as the execer to satisfy the helper's signature; the
	// helper's tx parameter is currently inert (see its doc comment).
	//
	// Outer ordering by committed_at is for write-order convenience only;
	// each ref's target_commit is resolved independently by
	// resolveTargetCommit's first-parent topological walk, so the outer
	// order does not affect correctness on branches with merge commits.
	clRows, err := conn(ctx, si.rh.db).QueryContext(ctx, `
	    SELECT cl.commit_hash, cl.path
	    FROM commit_log cl
	    JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
	    WHERE bc.branch_id = (SELECT id FROM branches WHERE name = ?)
	      AND cl.action != 'deleted'
	    ORDER BY cl.committed_at ASC, cl.rowid ASC
	`, branch)
	if err != nil {
		return total, fmt.Errorf("rebuildGraph phaseB: query commit_log: %w", err)
	}
	type historicalRow struct {
		commitHash string
		path       string
	}
	var rows2 []historicalRow
	for clRows.Next() {
		var r historicalRow
		if err := clRows.Scan(&r.commitHash, &r.path); err != nil {
			clRows.Close()
			return total, fmt.Errorf("rebuildGraph phaseB: scan: %w", err)
		}
		rows2 = append(rows2, r)
	}
	clRows.Close()

	for _, r := range rows2 {
		content, err := si.rh.readFileAtCommit(ctx, r.path, r.commitHash)
		if err != nil {
			log.Debug().Err(err).Str("path", r.path).Str("commit", r.commitHash[:8]).Msg("rebuildGraph phaseB: skip (file not at commit)")
			continue
		}
		rec, err := parseFact(r.path, content)
		if err != nil {
			log.Debug().Err(err).Str("path", r.path).Msg("rebuildGraph phaseB: skip (parse failed)")
			continue
		}
		blobHash, err := si.rh.readBlobHashAtCommit(ctx, r.path, r.commitHash)
		if err != nil {
			log.Warn().Err(err).Str("path", r.path).Str("commit", r.commitHash[:8]).Msg("rebuildGraph phaseB: blob_hash lookup failed")
			continue
		}

		var localRefs []string
		for _, ref := range rec.Refs {
			if !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
				localRefs = append(localRefs, ref)
			}
		}
		if len(localRefs) == 0 {
			continue
		}

		// Skip when the source blob version was orphaned (no graph node).
		// Phase A iterates the current `facts` table, which is COW-deduped
		// by (path, blob_hash) AND garbage-collected of rows that no
		// branch_facts row points at. After a fact is updated/retracted,
		// older blob versions can be GC'd out of `facts` while their
		// commit_log entries (and historical refs) survive. Phase B sees
		// those historical (commit, path) tuples and tries to write
		// edges from the orphaned source — there's no graph node to
		// write from, so we silently skip. Mirrors the symmetric
		// missing-target handling inside graphAddDerivedFromAtCommitTx.
		srcID, err := si.graphNodeIDByBlob(ctx, r.path, blobHash)
		if err != nil {
			log.Warn().Err(err).Str("path", r.path).Str("commit", r.commitHash[:8]).Msg("rebuildGraph phaseB: source node lookup failed")
			continue
		}
		if srcID == 0 {
			log.Debug().Str("path", r.path).Str("commit", r.commitHash[:8]).Str("blob", blobHash[:8]).
				Msg("rebuildGraph phaseB: skip (source blob orphaned out of facts; no graph node)")
			continue
		}

		if err := si.graphAddDerivedFromAtCommitTx(ctx, si.rh.db, branch, r.path, blobHash, r.commitHash, localRefs); err != nil {
			log.Warn().Err(err).Str("path", r.path).Str("commit", r.commitHash[:8]).Msg("rebuildGraph phaseB: edge write failed")
		}
	}

	// Build similarity edges after commit (needs committed data for KNN).
	// Batch: collect all neighbors first, then write all edges in one transaction.
	if si.rh.getEmbedder() != nil {
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

