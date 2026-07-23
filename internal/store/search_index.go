package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// GraphSchemaVersion is the expected version of the property-graph layout.
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
//
// Version 4: facts_vec is code-managed (ensureFactsVec) at the active model's
// dimension rather than a static FLOAT[768] migration, and NeedsRebuild now
// ALSO gates on the embedding identity (meta.embed_model_id / meta.embed_dim).
// A model id or dim change invalidates every stored vector, forcing a rebuild
// that recreates facts_vec empty and re-embeds the whole corpus.
// Version 5: the GraphQLite extension is gone. Node and edge properties are
// stored as TEXT in {node,edge}_props_text only — the extension used to route
// values into typed sibling tables (_int/_real/_bool/_json) by storage class,
// and those are dropped in migration 000014. Existing deployments hold
// `deleted` as a JSON boolean in node_props_bool, which the direct-SQL readers
// cannot see, so every graph node would read as live until the derived state is
// regenerated. The bump forces that rebuild on startup. Rebuild reads git (the
// only source of truth) and preserves embeddings, so this costs a graph rewrite,
// not a re-embed.
const GraphSchemaVersion = "5"

type searchIndex struct {
	rh *repoHandler

	// syncSuspended, when true, makes Sync a no-op. It is toggled around a bulk
	// Replay into a TRANSIENT store (the origin clone) that a single full
	// Rebuild reconstructs afterward, so the ~N per-commit incremental syncs are
	// pure waste. Instance-scoped: only the specific Service whose si carries
	// this flag is affected — every other store (notably the live repo) indexes
	// normally through its own searchIndex. NEVER leave a live store suspended:
	// its per-branch tables would drift from the git tree and trip Verify's
	// facts-coherence check. See ReplayConfig.SkipIndexSync.
	syncSuspended atomic.Bool

	// Test-only interleaving hooks, nil in production. The similarity rewrite
	// races concurrent cross-branch writers by construction (the graph is
	// shared, lockBranch is per-branch), and that race cannot be exercised with
	// sleeps — it needs the two sides to rendezvous at an exact point.
	// beforeSimTx fires after the KNN pass and before the transaction opens;
	// inSimTx fires after the prunes and before the merge loop.
	//
	// inSimEdgeTx is the same observation point on the incremental path
	// (graphBuildSimilarityEdges): after that version's delete-outgoing, before
	// its re-merge. Both in-tx hooks exist so a test can prove the window is
	// invisible to other connections rather than inferring it from timing.
	beforeSimTx func()
	inSimTx     func()
	inSimEdgeTx func()
}

// simEdgeKey identifies a fact version for node-id caching during the
// similarity rewrite.
type simEdgeKey struct{ path, blobHash string }

// schemaState classifies the persisted graph_schema_version relative to the
// version this binary expects. It lets Sync distinguish a fresh/empty DB (no
// row yet — normal on first index) from a genuine version mismatch (a row
// written by an older binary), so the "run knomit rebuild" warning only fires
// in the case the operator can actually act on.
type schemaState int

const (
	schemaCurrent schemaState = iota // row present and equal to GraphSchemaVersion
	schemaMissing                    // no row (fresh/empty or pre-versioning DB)
	schemaStale                      // row present but != GraphSchemaVersion
)

// schemaVersionState reads meta.graph_schema_version and classifies it.
func (si *searchIndex) schemaVersionState(ctx context.Context) (schemaState, error) {
	var persistedVer string
	err := conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'graph_schema_version'`,
	).Scan(&persistedVer)
	if err == sql.ErrNoRows {
		return schemaMissing, nil
	}
	if err != nil {
		return schemaCurrent, fmt.Errorf("read graph_schema_version: %w", err)
	}
	if persistedVer != GraphSchemaVersion {
		return schemaStale, nil
	}
	return schemaCurrent, nil
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
	state, err := si.schemaVersionState(ctx)
	if err != nil {
		return false, err
	}
	// Both missing (fresh/pre-versioning) and stale (older binary) require a
	// full rebuild to lay out derived state for this binary.
	if state != schemaCurrent {
		return true, nil
	}

	// Embedding identity: a model/dim change invalidates every stored vector.
	if emb := si.rh.getEmbedder(); emb != nil {
		curID, err := si.persistedEmbedModelID(ctx)
		if err != nil {
			return false, err
		}
		curDim, err := si.persistedEmbedDim(ctx)
		if err != nil {
			return false, err
		}
		if curID != emb.ID() || curDim != emb.Dim() {
			return true, nil
		}
	}
	return false, nil
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
	// Suspended: the caller (a bulk Replay into a transient store) will run a
	// single Rebuild afterward, so per-commit maintenance here is wasted work.
	// notifyCommit still appends to commit_log before reaching us, so history
	// is preserved for that Rebuild to reconstruct derived state from.
	if si.syncSuspended.Load() {
		return nil
	}

	// Detect schema mismatch on entry. An older deployment may have derived
	// state (graph layout, canonical domains, token table) laid out by a
	// previous version. Callers that orchestrate startup (repoBuilder) call
	// NeedsRebuild and Rebuild to heal it; Sync only warns, because it cannot
	// safely escalate to a full rebuild for every branch from here. Warn only on
	// a genuine version mismatch (schemaStale) — a missing row is a fresh/empty
	// DB on its first index, not something `knomit rebuild` can help with.
	if state, err := si.schemaVersionState(ctx); err != nil {
		return fmt.Errorf("sync: %w", err)
	} else if state == schemaStale {
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

	// Let SQLite refresh its statistics as the corpus grows, so a long-lived
	// instance does not drift back into the blind-planner state Rebuild's
	// ANALYZE fixed. This is cheap and self-limiting: optimize only re-analyzes
	// on missing stats or large size drift, so steady-state syncs pay nothing.
	// The 0x10000 bit is required — without it optimize considers only tables
	// touched on this pooled connection, which under database/sql is arbitrary.
	if len(indexed) > 0 {
		// analysis_limit is per-connection and must ride the same Exec: without
		// it, 0x10000 (consider ALL tables) can trigger an unbounded ANALYZE on a
		// large corpus while lockBranch is held.
		if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
			`PRAGMA analysis_limit=1000; PRAGMA optimize=0x10002`); err != nil {
			log.Debug().Err(err).Msg("sync: optimize failed (query plans may drift)")
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

// SyncLocked runs Sync while holding the per-branch write lock.
//
// Bare Sync is lock-FREE because its sole legitimate caller, notifyCommit,
// already holds lockBranch(branch) (every write path — WriteFact, DeleteFact,
// BatchWriteFacts, MergeBranch, remote reconcile — syncs the index under the
// lock so no reader observes a torn git-HEAD/SQL-index state). Out-of-band
// callers that are NOT inside the lock — the commit observer and the startup
// heal's incremental-sync path — MUST use SyncLocked instead, so their index
// mutation is mutually exclusive with both inline writes and a concurrent
// Rebuild on the same branch. Calling bare Sync out-of-band races the
// watermark and branch_facts/facts_vec rows (see Rebuild's self-lock).
func (si *searchIndex) SyncLocked(ctx context.Context, branch string) error {
	defer si.rh.lockBranch(branch)()
	return si.Sync(ctx, branch)
}

// RebuildProgress is called during Rebuild to report progress.
type RebuildProgress func(phase string, done, total int)

// Rebuild clears the last-commit marker and re-indexes every file from HEAD
// using three phases: facts, embeddings, graph.
//
// Rebuild holds lockBranch(branch) for its entire duration. It clears the index
// watermark (setLastCommit "") and rewrites every derived row, so it MUST be
// mutually exclusive with any other index mutation on the branch — an inline
// write's notifyCommit→Sync, the commit observer's SyncLocked, or a second
// Rebuild. Without the lock a concurrent Sync would observe the cleared
// watermark, launch its own full re-index, and double-insert into facts_vec /
// corrupt branch_facts (the PR #82 background-index race). No current caller
// holds lockBranch before calling Rebuild, so self-locking cannot deadlock;
// normal reads don't take branchMu, so they remain non-blocking.
func (si *searchIndex) Rebuild(ctx context.Context, branch string, progress RebuildProgress) error {
	defer si.rh.lockBranch(branch)()

	if err := si.setLastCommit(ctx, branch, ""); err != nil {
		return fmt.Errorf("rebuild: clear last commit: %w", err)
	}

	head, err := si.rh.HeadCommit(ctx, branch)
	if err != nil {
		return fmt.Errorf("rebuild: head commit: %w", err)
	}

	// Ensure facts_vec exists at the active model's dimension BEFORE phase 1.
	// Phase 1 (rebuildFacts) can DELETE orphaned branch_facts/facts rows, which
	// fires the facts_after_delete trigger that references facts_vec; the table
	// must exist first. ensureFactsVec also recreates it empty when the model
	// identity changed, so phase 2 re-embeds the whole corpus.
	if emb := si.rh.getEmbedder(); emb != nil {
		if err := si.ensureFactsVec(ctx, emb.ID(), emb.Dim()); err != nil {
			return fmt.Errorf("rebuild: ensure facts_vec: %w", err)
		}
	}

	// Phase 0: commit history. Rewrite commit_log from git BEFORE facts, because
	// fact indexing resolves each fact's source commit from commit_log. This is
	// what makes :rebuild actually rebuild the commit history (incl. author
	// identity) instead of leaving stale rows that a plain re-walk would skip.
	start := time.Now()
	if err := si.rh.rebuildCommitLog(ctx, branch); err != nil {
		return fmt.Errorf("rebuild: commit log: %w", err)
	}
	log.Info().Str("elapsed", fmt.Sprintf("%.1fs", time.Since(start).Seconds())).Msg("rebuild: phase 0 (commit log) complete")

	// Phase 1: facts
	start = time.Now()
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

	// Refresh planner statistics now that the tables exist at their real size.
	//
	// Without this the graph tables carry no sqlite_stat1 rows at all on a fresh
	// clone: the only ANALYZE is the `PRAGMA optimize` at DB open (service.go),
	// which runs BEFORE Rebuild populates them. Planning blind, SQLite drives the
	// anchor-driven SIMILAR_TO read off idx_edges_type — a near-scan of every
	// edge of that type per anchor — instead of idx_edges_source/idx_edges_target.
	// Measured on a 23.9k-edge corpus: 1722ms vs 11ms for a 400-anchor read, and
	// 4.25ms vs 0.20ms for the blast-radius CTE.
	//
	// analysis_limit bounds the scan per index so this stays milliseconds even on
	// a large corpus; both statements go in one Exec because analysis_limit is
	// per-connection and the pool would otherwise hand ANALYZE a different one.
	// Runs outside any transaction — under _txlock=immediate an open tx is a
	// process-wide write lock, and ANALYZE needs only its own brief one.
	// Best-effort: degraded query plans must never fail a rebuild.
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`PRAGMA analysis_limit=1000; ANALYZE`); err != nil {
		log.Warn().Err(err).Msg("rebuild: analyze failed (query plans may be degraded)")
	}

	if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('graph_schema_version', ?)`,
		GraphSchemaVersion,
	); err != nil {
		return fmt.Errorf("rebuild: bump graph schema version: %w", err)
	}

	// Persist the embedding identity alongside the schema version so a later
	// model/dim change is detected by NeedsRebuild. Written on success only,
	// after facts_vec was (re)created and the corpus re-embedded at this dim.
	if emb := si.rh.getEmbedder(); emb != nil {
		if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
			`INSERT OR REPLACE INTO meta(key, value) VALUES ('embed_model_id', ?)`, emb.ID()); err != nil {
			return fmt.Errorf("rebuild: persist embed_model_id: %w", err)
		}
		if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
			`INSERT OR REPLACE INTO meta(key, value) VALUES ('embed_dim', ?)`, emb.Dim()); err != nil {
			return fmt.Errorf("rebuild: persist embed_dim: %w", err)
		}
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

	// Tool/pipeline sessions are no longer in this DB — they live in the
	// ephemeral session DB and are reaped there by an idle-TTL sweep
	// (Service.ReapIdleSessions), not by this branch-drop GC.

	return nil
}

// gcOrphanedGraphNodes removes graph nodes of the given label that have no
// incoming edges of edgeType from any Fact node.
//
// One set-based DELETE rather than a query plus one round-trip per orphan.
// Labels, properties and incident edges cascade (ON DELETE CASCADE with
// _foreign_keys=1), which is what Cypher's DETACH DELETE did.
//
// Nodes are matched by id, not by a `path` property. The Cypher version
// projected n.path and skipped rows where it was empty, so Entity orphans —
// which are keyed by `name`, and therefore have no `path` — were never
// actually collected.
func (si *searchIndex) gcOrphanedGraphNodes(ctx context.Context, label, edgeType string) error {
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `
		DELETE FROM nodes
		WHERE id IN (
			SELECT nl.node_id
			FROM node_labels nl
			WHERE nl.label = ?
			  AND NOT EXISTS (
				SELECT 1
				FROM edges e
				JOIN node_labels sl ON sl.node_id = e.source_id AND sl.label = ?
				WHERE e.target_id = nl.node_id AND e.type = ?
			  )
		)`, label, NodeFact, edgeType); err != nil {
		return fmt.Errorf("gc orphaned %s: %w", label, err)
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
		INSERT INTO facts (path, blob_hash, title, kind, type, domain, entities, confidence, sources, refs, evidence_weight, origin)
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
			COALESCE(json_extract(pe.parsed, '$.evidence_weight'), 0),
			COALESCE(json_extract(pe.parsed, '$.origin'), 'authored')
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
			evidence_weight = excluded.evidence_weight,
			origin          = excluded.origin
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
		title string
		body  string
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
			e.title = title
			e.body = extractBody(data)
			entries = append(entries, e)
		}
		bodyRows.Close()

		if len(entries) == 0 {
			continue
		}

		// Compute embeddings OUTSIDE any transaction. The ONNX inference is the
		// slow part (seconds per batch); holding the SQLite write lock across it
		// would starve concurrent writers (cluster cache, MCP learns, sync) for
		// the whole rebuild — they'd hit "database is locked". Open a short write
		// tx only to insert the finished vectors. vecs[j] aligns with entries[j].
		var vecs [][]float32
		if hasBatch {
			titles := make([]string, len(entries))
			bodies := make([]string, len(entries))
			for j, e := range entries {
				titles[j] = e.title
				bodies[j] = e.body
			}
			var embErr error
			vecs, embErr = batcher.EmbedDocuments(ctx, titles, bodies)
			if embErr != nil {
				return done, fmt.Errorf("rebuildEmbeddings: embed batch: %w", embErr)
			}
			// The insert loop indexes entries[j] by vecs position, so a short
			// (or long) return would read out of bounds. Fail cleanly instead
			// of panicking if the embedder breaks the 1:1 contract.
			if len(vecs) != len(entries) {
				return done, fmt.Errorf("rebuildEmbeddings: embedder returned %d vectors for %d entries", len(vecs), len(entries))
			}
		} else {
			vecs = make([][]float32, len(entries))
			for j, e := range entries {
				vec, embErr := emb.EmbedDocument(ctx, e.title, e.body)
				if embErr != nil {
					// A per-fact embed failure is survivable — skip it and let
					// the rest of the chunk through. Cancellation is not: the
					// BeginTx below would fail on the same context anyway, so
					// continuing would emit one WARN per remaining fact to
					// describe a single "the rebuild was cancelled".
					if ctx.Err() != nil {
						return done, fmt.Errorf("rebuildEmbeddings: embed %s: %w", e.path, embErr)
					}
					log.Warn().Err(embErr).Str("path", e.path).Msg("rebuildEmbeddings: embed failed, skipping")
					continue
				}
				vecs[j] = vec
			}
		}

		tx, err := si.rh.db.BeginTx(ctx, nil)
		if err != nil {
			return done, fmt.Errorf("rebuildEmbeddings: begin tx: %w", err)
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
		SELECT f.path, f.title, f.blob_hash, f.kind, f.type, f.domain, f.entities, f.confidence, f.sources, f.refs, f.evidence_weight, f.origin
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
			&refsJSON, &rec.EvidenceWeight, &rec.Origin); err != nil {
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
	// Read on the tx's own connection — NOT conn(ctx, db), which would fall
	// through to the bare pool and need a SECOND connection while this tx holds
	// the write lock. Under _txlock=immediate that straddle can starve the pool
	// (the held write lock blocks other connections' BEGIN IMMEDIATE) during a
	// rebuild that races concurrent writers. commit_log/branch_commits are
	// regular tables (not GraphQLite EAV), so reading them inside the tx is safe.
	histRows, err := tx.QueryContext(ctx, `
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
	// Runs POST-commit. The original reason was that direct-SQL reads could
	// not see nodes MERGE'd through Cypher inside the same *sql.Tx, so node
	// IDs only became visible post-commit. Node writes are direct SQL too now,
	// so that constraint is gone and this could in principle move into the
	// transaction — left as-is to keep the GraphQLite removal behaviour-neutral
	// (see the matching notes in derived_from.go and search_crud.go).
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
		simFloor := EmbedderThresholds(si.rh.getEmbedder()).SimilarTo

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
				var sim sql.NullFloat64
				if err := rows.Scan(&neighborPath, &neighborBH, &sim); err != nil {
					break
				}
				// Skip degenerate (zero-norm) neighbors with a NULL similarity;
				// see usableKNNSimilarity for the invariant.
				s, ok := usableKNNSimilarity(sim)
				if !ok {
					continue
				}
				if (neighborPath != rec.Path || neighborBH != rec.BlobHash) && s >= simFloor {
					edges = append(edges, simEdge{fromPath: rec.Path, fromBH: rec.BlobHash, toPath: neighborPath, toBH: neighborBH})
				}
			}
			rows.Close()
		}

		// Replace the similarity layer for the versions this rebuild is
		// authoritative for, in one transaction.
		//
		// Rebuild must REPLACE rather than add: without a delete it only merges,
		// so edges from an earlier corpus state survive alongside the fresh
		// top-K, out-degree drifts past knnK, and the cohesion reader (which
		// anchors by path, not by version) feeds an inflated graph to Louvain.
		//
		// But the delete MUST NOT be a blanket `DELETE ... WHERE type=SIMILAR_TO`.
		// `edges` was computed by the KNN loop above, before this transaction
		// opened. A writer on another branch shares the graph but NOT
		// lockBranch, so it can commit a new fact version and its similarity
		// edges inside that window. A blanket wipe deletes them and the re-merge
		// cannot restore them — they are not in this rebuild's snapshot — and
		// nothing ever regenerates them: that branch's Sync sees last_commit ==
		// HEAD and no-ops forever. The edges are lost permanently.
		//
		// So delete exactly two sets, both safe against a concurrent writer:
		//
		//  1. Outgoing edges of the versions this pass is about to rewrite.
		//     A concurrently written version is not in that set.
		//  2. Outgoing edges of versions that are no longer current — the
		//     superseded nodes a per-source prune can never reach, since they
		//     are absent from `facts`. Evaluated as SQL inside the transaction,
		//     so a row a concurrent writer just committed counts as current and
		//     its edges survive.
		//
		// Safe because SIMILAR_TO is pure derived data, recomputed in full from
		// facts_vec. DERIVED_FROM is the immutable temporal assertion and is
		// deliberately NOT touched here.
		//
		// Entered whenever an embedder is set, even with zero edges to write: a
		// corpus whose similarities all fall below the floor must still shed its
		// stale edges. Reaching here with an embedder implies vectors exist —
		// rebuildEmbeddings hard-fails the Rebuild before phase 3 otherwise — so
		// this cannot silently wipe the layer during an embedding outage.
		if si.beforeSimTx != nil {
			si.beforeSimTx()
		}
		simTx, err := si.rh.db.BeginTx(ctx, nil)
		if err != nil {
			// Fatal, not a warning: falling through would let Rebuild bump
			// graph_schema_version and last_commit while the similarity layer is
			// stale, so NeedsRebuild would report healthy and nothing would retry.
			return total, fmt.Errorf("rebuildGraph: begin similarity tx: %w", err)
		}
		defer simTx.Rollback()

		// Resolve every node id once. The merge loop below would otherwise
		// re-resolve each source once per its ~knnK edges, multiplying the work
		// done while holding the process-wide write lock (_txlock=immediate).
		nodeIDs := make(map[simEdgeKey]int64, len(edges)*2)
		resolve := func(path, bh string) (int64, error) {
			k := simEdgeKey{path, bh}
			if id, ok := nodeIDs[k]; ok {
				return id, nil
			}
			id, err := graphNodeIDByProps(ctx, simTx, NodeFact,
				map[string]string{"path": path, "blob_hash": bh})
			if err != nil {
				return 0, err
			}
			nodeIDs[k] = id
			return id, nil
		}

		// (1) sources this pass rewrites — every enumerated fact version, not
		// just those that produced edges, so a fact whose neighbours all fell
		// below the floor still sheds its old ones.
		rewritten := make([]int64, 0, len(facts))
		for _, rec := range facts {
			id, err := resolve(rec.Path, rec.BlobHash)
			if err != nil {
				return total, fmt.Errorf("rebuildGraph: resolve %s: %w", rec.Path, err)
			}
			if id != 0 {
				rewritten = append(rewritten, id)
			}
		}
		if err := graphDeleteOutgoingEdgesOfType(ctx, simTx, rewritten, EdgeSimilarTo); err != nil {
			return total, fmt.Errorf("rebuildGraph: prune rewritten similarity edges: %w", err)
		}

		// (2) superseded versions.
		if err := graphDeleteSimilarToOfSupersededVersions(ctx, simTx); err != nil {
			return total, fmt.Errorf("rebuildGraph: prune superseded similarity edges: %w", err)
		}

		if si.inSimTx != nil {
			si.inSimTx()
		}

		for _, e := range edges {
			srcID, err := resolve(e.fromPath, e.fromBH)
			if err != nil || srcID == 0 {
				log.Warn().Err(err).Str("from", e.fromPath).Str("to", e.toPath).Msg("rebuildGraph: similarity source node missing")
				continue
			}
			tgtID, err := resolve(e.toPath, e.toBH)
			if err != nil || tgtID == 0 {
				log.Warn().Err(err).Str("from", e.fromPath).Str("to", e.toPath).Msg("rebuildGraph: similarity target node missing")
				continue
			}
			if err := graphMergeEdge(ctx, simTx, srcID, tgtID, EdgeSimilarTo); err != nil {
				log.Warn().Err(err).Str("from", e.fromPath).Str("to", e.toPath).Msg("rebuildGraph: similarity edge failed")
			}
		}
		if err := simTx.Commit(); err != nil {
			return total, fmt.Errorf("rebuildGraph: commit similarity tx: %w", err)
		}
	}

	return total, nil
}
