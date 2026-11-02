package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"knomit/internal/engine/store/model"

	"github.com/rs/zerolog/log"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/facts"
	"knomit/internal/engine/store/internal/vcs"
)

// GraphSchemaVersion is the expected version of the property-graph layout.
// Incremented when the graph schema changes in a way that requires a forced
// rebuild on existing deployments. Persisted in
// meta.graph_schema_version:<branch> after every successful Rebuild OF THAT
// BRANCH, and deleted after a failed one; checked per branch on Open. See
// schemaVersionKey for why the key is per-branch rather than global.
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
// `deleted` as INTEGER 0/1 in node_props_bool (the extension declared it
// `value INTEGER NOT NULL CHECK (value IN (0, 1))`), which the direct-SQL
// readers cannot see; 000014 converts it to TEXT 'true'/'false' before the
// drop, so liveness survives the migration itself. The bump is what regenerates
// everything the conversion does not carry over — confidence and sources, which
// lived in the dropped _int/_real tables and are rewritten as TEXT. Rebuild
// reads git (the only source of truth) and preserves embeddings, so this costs
// a graph rewrite, not a re-embed.
// Version 6: the fact_expires side table (F03).
// Version 7: the fact_context side table (F22, migration 000034). The forced
// rebuild fills it from every blob, including hand-written `context:` lines a
// pre-F22 parser dropped.
// Version 8: reference edges are per BLOB, written once (claimBlobRefs). The
// forced rebuild drops every DERIVED_FROM edge ONCE per store
// (resetBlobRefsOnce), and phase B re-resolves each blob at its earliest
// introduction, for the rebuilt branch and then every other row of
// `branches` (reResolvePendingBranches), so the edges the old per-commit
// writes accumulated (the same ref re-pinned at every commit a blob arrived
// at) are gone and no branch is left without edges.
const GraphSchemaVersion = "8"

type Index struct {
	rh *vcs.Repo

	// danglingRefsSkipped counts reference edges not written because a blob
	// was not in the object store (DanglingRefsSkipped).
	danglingRefsSkipped atomic.Int64

	// This repo's 12-hex identity (root-commit prefix), cached once resolved
	// — it is immutable for the life of the repo. Used to tell a
	// kb://<own-id>/… ref (a local edge) from a foreign one. A failed
	// resolution is NOT cached: the next call retries (repoIDMu).
	repoIDMu sync.Mutex
	repoID   string
	// rootCommit resolves a branch's root commit (rh.RootCommit; a test seam
	// replaces it to stand in for a failing lookup).
	rootCommit func(ctx context.Context, branch string) (string, error)

	// syncSuspended, when true, makes Sync a no-op. It is toggled around a bulk
	// Replay into a TRANSIENT store (the origin clone) that a single full
	// Rebuild reconstructs afterward, so the ~N per-commit incremental syncs are
	// pure waste. Instance-scoped: only the specific Service whose si carries
	// this flag is affected — every other store (notably the live repo) indexes
	// normally through its own index.Index. NEVER leave a live store suspended:
	// its per-branch tables would drift from the git tree and trip Verify's
	// facts-coherence check. See ReplayConfig.SkipIndexSync.
	syncSuspended atomic.Bool

	// Test-only interleaving hooks, nil in production. The similarity rewrite
	// races concurrent cross-branch writers by construction (the graph is
	// shared, LockBranch is per-branch), and that race cannot be exercised with
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
	// beforeGraphTx fires in rebuildGraph after the git-read cache is built
	// and before its write transaction opens: the window in which a
	// cross-branch writer could change `facts` under a snapshot taken
	// outside the transaction.
	beforeGraphTx func()
	// beforeSyncCAS fires in Sync after every row of the sync is written and
	// before the watermark moves: the window in which branch_facts is ahead
	// of meta.last_commit.
	beforeSyncCAS func()
	// beforeRefEdgesWrite fires inside applyBlobRefs's transaction, after the
	// claim is written and before the edges are: an error from it aborts the
	// transaction, which is how a test stands in for a crash between the two.
	beforeRefEdgesWrite func() error
	// beforeRefRowsWrite fires inside applyBlobRefs's transaction, after the
	// claim and the edges are written and before the blob's ref_edges rows
	// are: an error from it aborts the transaction (a test's crash between
	// the edges and the rows).
	beforeRefRowsWrite func() error
}

// simEdgeKey identifies a fact version for node-id caching during the
// similarity rewrite.
type simEdgeKey struct{ path, blobHash string }

// schemaState classifies a branch's persisted graph schema version relative to
// the version this binary expects. It lets Sync distinguish a fresh/empty DB (no
// row yet — normal on first index) from a genuine version mismatch (a row
// written by an older binary), so the "run knomit rebuild" warning only fires
// in the case the operator can actually act on.
type schemaState int

const (
	schemaCurrent schemaState = iota // row present and equal to GraphSchemaVersion
	schemaMissing                    // no row (fresh/empty or never-indexed branch)
	schemaStale                      // row present but != GraphSchemaVersion
)

// schemaVersionKey returns the meta key holding the index schema version for a
// branch. Per-branch, mirroring last_commit:<branch>, because the state it
// describes IS per-branch: Rebuild regenerates one branch's derived rows.
//
// It used to be one global key, and that is what made a partial multi-branch
// heal unrepresentable — a successful branch's bump marked the failed branch
// current too. The workaround, deleting the key on any partial heal, then made a
// PERMANENT single-branch failure (a stored upstream naming a ref that does not
// exist locally, say) re-arm a full-repo rebuild on every boot, forever, while
// the repo still reported a healthy index. Per-branch keys keep the re-arm — see
// clearSchemaVersion, which Rebuild calls on failure — but make it proportional:
// the failed branch retries, its siblings stay done.
// Migration 000016 carried the old global row onto each known branch.
func schemaVersionKey(branch string) string { return "graph_schema_version:" + branch }

// schemaVersionState reads meta.graph_schema_version:<branch> and classifies it.
func (si *Index) schemaVersionState(ctx context.Context, branch string) (schemaState, error) {
	var persistedVer string
	err := conn(ctx, si.rh.DB()).QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, schemaVersionKey(branch),
	).Scan(&persistedVer)
	if err == sql.ErrNoRows {
		return schemaMissing, nil
	}
	if err != nil {
		return schemaCurrent, fmt.Errorf("read %s: %w", schemaVersionKey(branch), err)
	}
	if persistedVer != GraphSchemaVersion {
		return schemaStale, nil
	}
	return schemaCurrent, nil
}

// clearSchemaVersion drops a branch's version row, so schemaVersionState reports
// it missing and NeedsRebuild returns stale for that branch ALONE. Rebuild calls
// it on every failure path; nothing else should.
//
// It is what makes "a failed rebuild is retried" true rather than nearly true.
// Rebuild only ever WRITES the row, on success, so a branch that failed keeps
// whatever row it had — and a branch whose row was already current (it was not
// the schema that made it stale) would sail through the version gate next boot.
// NeedsRebuild's second gate, the embedding identity, cannot catch it either:
// that one is global by design, so a sibling branch's successful rebuild
// persists the new identity and answers the gate on the failed branch's behalf.
// The branch then reports healthy forever while its derived state — commit_log
// and the similarity layer, which Sync's no-watermark path does not rebuild —
// stays as the failed rebuild left it.
//
// The context is detached: the common failure IS cancellation at shutdown, and
// clearing the row is exactly what must survive it.
func (si *Index) clearSchemaVersion(ctx context.Context, branch string) {
	ctx = context.WithoutCancel(ctx)
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
		`DELETE FROM meta WHERE key = ?`, schemaVersionKey(branch),
	); err != nil {
		// Best-effort by necessity: we are already on an error path. Log loudly,
		// because the cost is a branch that silently reports healthy.
		log.Error().Err(err).Str("repo", si.rh.Name()).Str("branch", branch).
			Msg("could not clear the index schema version after a failed rebuild; " +
				"this branch may not be retried on the next startup")
	}
}

// casLastCommit atomically updates the last-commit watermark for a branch,
// but only if it currently equals prev. Returns true if the update succeeded.
func (si *Index) casLastCommit(ctx context.Context, branch, prev, next string) (bool, error) {
	key := "last_commit:" + branch
	db := conn(ctx, si.rh.DB())

	if prev == "" {
		// First sync: insert only if key doesn't exist.
		result, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)`, key, next)
		if err != nil {
			return false, err
		}
		n, _ := result.RowsAffected()
		if n > 0 {
			return true, si.markSettledOnce(ctx, branch, next)
		}
		return false, nil
	}

	result, err := db.ExecContext(ctx,
		`UPDATE meta SET value = ? WHERE key = ? AND value = ?`, next, key, prev)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		return true, si.markSettledOnce(ctx, branch, next)
	}
	return false, nil
}

// SyncPendingKey is the meta key of a branch's in-flight sync marker: the
// head a sync is writing rows for, set before its first row and removed once
// the watermark reaches that head.
func SyncPendingKey(branch string) string { return "sync_pending:" + branch }

// RebuildGenKey is the meta key of a branch's rebuild generation: a counter
// every Index.Rebuild of the branch bumps, so a reader can tell a Rebuild
// that ran to completion at the same head between two of its reads (the
// watermark reads the same before and after; review R3-2).
func RebuildGenKey(branch string) string { return "rebuild_gen:" + branch }

// SettledOnceKey is the meta key that records that a branch's index watermark
// has been set at least once: written ('1') whenever the watermark is set to a
// commit (casLastCommit, setLastCommit), and by Index.Rebuild for a branch
// that already had a watermark before it clears it (a store indexed before
// the key existed). Nothing but dropping the branch removes it; a branch
// created from another inherits it with the watermark. Absent means the
// branch has NEVER had a settled watermark: a first build in flight, a first
// build that failed, a store whose index was wiped, or no such branch
// (HistoryQuery.SettledOnce).
func SettledOnceKey(branch string) string { return "settled_once:" + branch }

// markSettledOnce records SettledOnceKey once a watermark is set to a commit.
func (si *Index) markSettledOnce(ctx context.Context, branch, hash string) error {
	if hash == "" {
		return nil
	}
	_, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
		`INSERT OR IGNORE INTO meta(key, value) VALUES (?, '1')`, SettledOnceKey(branch))
	return err
}

func (si *Index) setSyncPending(ctx context.Context, branch, head string) error {
	_, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, SyncPendingKey(branch), head)
	return err
}

// clearStaleSyncPending removes a marker that names another head than head,
// the watermark the branch's rows are at.
func (si *Index) clearStaleSyncPending(ctx context.Context, branch, head string) error {
	_, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
		`DELETE FROM meta WHERE key = ? AND value != ?`, SyncPendingKey(branch), head)
	return err
}

// clearSyncPending removes the marker only while it still names head: a
// newer sync that set its own marker keeps it.
func (si *Index) clearSyncPending(ctx context.Context, branch, head string) error {
	_, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
		`DELETE FROM meta WHERE key = ? AND value = ?`, SyncPendingKey(branch), head)
	return err
}

// setLastCommit stores the last processed commit hash in the meta table,
// scoped to the given branch.
func (si *Index) setLastCommit(ctx context.Context, branch, hash string) error {
	key := "last_commit:" + branch
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`,
		key, hash,
	); err != nil {
		return err
	}
	return si.markSettledOnce(ctx, branch, hash)
}

// SyncWatermark returns the last processed commit hash for the given branch,
// or "" if not set.
func (si *Index) SyncWatermark(ctx context.Context, branch string) (string, error) {
	key := "last_commit:" + branch
	var hash string
	err := conn(ctx, si.rh.DB()).QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&hash)
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

// NeedsRebuild reports whether this BRANCH's persisted index schema version
// differs from the current GraphSchemaVersion — i.e. its derived state (graph
// layout, canonical domains, fact_domain_tokens) was written by an older version
// and a full Rebuild is required to regenerate it. The version is
// meta.graph_schema_version:<branch>; a missing row (fresh DB, or a branch never
// indexed) counts as stale. Rebuild bumps it for the branch it rebuilt, so one
// branch's answer never speaks for another's.
//
// The embedding-identity half below is deliberately still global: facts_vec is
// corpus-wide (facts rows are shared, branch_facts is only the junction), so one
// branch's re-embed genuinely covers every branch.
//
// That globality is also why the version gate above must come first AND must be
// the one thing a failed rebuild resets. A failed branch is stale for reasons
// the embedding gate cannot express — its commit_log and similarity layer are
// half-written — and the gate would in fact answer "fresh" for it, because the
// sibling branch that rebuilt successfully persisted the new identity for the
// whole corpus. Rebuild's clearSchemaVersion is what keeps the version gate
// honest here: a branch that failed has no row, so it reports stale on the row
// alone and never reaches the embedding check.
func (si *Index) NeedsRebuild(ctx context.Context, branch string) (bool, error) {
	state, err := si.schemaVersionState(ctx, branch)
	if err != nil {
		return false, err
	}
	// Both missing (fresh/pre-versioning) and stale (older binary) require a
	// full rebuild to lay out derived state for this binary.
	if state != schemaCurrent {
		return true, nil
	}

	// Embedding identity: a model/dim change invalidates every stored vector.
	if emb := si.rh.Embedder(); emb != nil {
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

// Sync brings the index up to date with the git store.
//
// Algorithm:
//  1. Read meta.last_commit.
//  2. If missing → full rebuild (ListAll, index everything).
//  3. If last_commit == HEAD → no-op.
//  4. Else → DiffFiles(last_commit), upsert added+modified, delete removed.
//  5. Update meta.last_commit = HEAD.
func (si *Index) Sync(ctx context.Context, branch string) error {
	// Suspended: the caller (a bulk Replay into a transient store) will run a
	// single Rebuild afterward, so per-commit maintenance here is wasted work.
	// NotifyCommit still appends to commit_log before reaching us, so history
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
	if state, err := si.schemaVersionState(ctx, branch); err != nil {
		return fmt.Errorf("sync: %w", err)
	} else if state == schemaStale {
		log.Warn().
			Str("repo", si.rh.Name()).
			Str("branch", branch).
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
		// A marker naming another head is a failed sync's: the watermark
		// reached head some other way (a Rebuild after the branch moved
		// without a sync), and the rows are at it. Sync and Rebuild hold the
		// branch lock, so no sync of this branch is in flight to own it
		// (review R2-1).
		if err := si.clearStaleSyncPending(ctx, branch, head); err != nil {
			return fmt.Errorf("sync: clear stale pending marker: %w", err)
		}
		return nil
	}

	// branch_facts is rewritten file by file below and the watermark moves
	// only at the end, so between the two the rows are AHEAD of
	// meta.last_commit. The pending marker, committed before the first row,
	// tells a reader that the rows no longer match the watermark beside them
	// (HistoryQuery.LiveAt's fast path, review N1). A sync that fails leaves
	// it set, which only sends readers to the derived history until the next
	// sync of the branch overwrites it.
	if err := si.setSyncPending(ctx, branch, head); err != nil {
		return fmt.Errorf("sync: pending marker: %w", err)
	}

	// indexed collects every FactRecord successfully upserted in this sync
	// pass. After all fact nodes are committed (pass 1), a second pass writes
	// DERIVED_FROM edges for all of them. This two-pass order is required for
	// intra-commit refs: when A.md and B.md land in the same git commit and
	// A.md refs B.md, B.md's Fact node must exist before A.md's outgoing edge
	// can be wired. Pass 1 commits all nodes; pass 2 retries all edges (with
	// the dedup guard in graphAddDerivedFromAtCommitTx preventing duplicates
	// for refs that were already wired successfully in pass 1).
	var indexed []facts.FactRecord

	if last == "" {
		// Full rebuild: no previous commit recorded, so index every file.
		log.Info().Str("head", head[:8]).Msg("index sync: full rebuild (no previous commit)")
		paths, err := si.rh.ListAll(ctx, branch)
		if err != nil {
			return fmt.Errorf("sync: list all: %w", err)
		}
		for _, path := range paths {
			// Facts live under the ontology root; nothing else is one, whatever
			// it parses as (IsFactPath). Skipping here also spares the blob read
			// and the per-path git log indexFile would do before rejecting it.
			if !si.rh.IsFactPath(path) {
				continue
			}
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
			if !si.rh.IsFactPath(path) {
				continue
			}
			rec, err := si.indexFile(ctx, branch, path, head)
			if err != nil {
				return err
			}
			if rec != nil {
				indexed = append(indexed, *rec)
			}
		}
		// Deletions are deliberately NOT scope-filtered. The filter guards what
		// goes IN; a row admitted by an older build (or under a previous
		// ontology root) must still be evictable when its file disappears, and
		// deleting a path that was never indexed is a no-op anyway.
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
		// large corpus while LockBranch is held.
		if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
			`PRAGMA analysis_limit=1000; PRAGMA optimize=0x10002`); err != nil {
			log.Debug().Err(err).Msg("sync: optimize failed (query plans may drift)")
		}
	}

	if si.beforeSyncCAS != nil {
		si.beforeSyncCAS()
	}
	ok, err := si.casLastCommit(ctx, branch, last, head)
	if err != nil {
		return fmt.Errorf("sync cas: %w", err)
	}
	if !ok {
		// The marker stays: this sync's rows were written and the watermark
		// is another sync's, so readers keep to the derived history until a
		// sync completes.
		log.Debug().Str("branch", branch).Msg("sync: CAS failed, another sync won")
		return nil
	}
	if err := si.clearSyncPending(ctx, branch, head); err != nil {
		return fmt.Errorf("sync: clear pending marker: %w", err)
	}
	return nil
}

// SyncLocked runs Sync while holding the per-branch write lock.
//
// Bare Sync is lock-FREE because its sole legitimate caller, NotifyCommit,
// already holds LockBranch(branch) (every write path — WriteFact, DeleteFact,
// BatchWriteFacts, MergeBranch, remote reconcile — syncs the index under the
// lock so no reader observes a torn git-HEAD/SQL-index state). Out-of-band
// callers that are NOT inside the lock — the commit observer and the startup
// heal's incremental-sync path — MUST use SyncLocked instead, so their index
// mutation is mutually exclusive with both inline writes and a concurrent
// Rebuild on the same branch. Calling bare Sync out-of-band races the
// watermark and branch_facts/facts_vec rows (see Rebuild's self-lock).
func (si *Index) SyncLocked(ctx context.Context, branch string) error {
	defer si.rh.LockBranch(branch)()
	return si.Sync(ctx, branch)
}

// Rebuild clears the last-commit marker and re-indexes every file from HEAD
// using three phases: facts, embeddings, graph.
//
// Rebuild holds LockBranch(branch) for its entire duration. It clears the index
// watermark (setLastCommit "") and rewrites every derived row, so it MUST be
// mutually exclusive with any other index mutation on the branch — an inline
// write's NotifyCommit→Sync, the commit observer's SyncLocked, or a second
// Rebuild. Without the lock a concurrent Sync would observe the cleared
// watermark, launch its own full re-index, and double-insert into facts_vec /
// corrupt branch_facts (the PR #82 background-index race). No current caller
// holds LockBranch before calling Rebuild, so self-locking cannot deadlock;
// normal reads don't take branchMu, so they remain non-blocking.
func (si *Index) Rebuild(ctx context.Context, branch string, progress model.RebuildProgress) (err error) {
	defer si.rh.LockBranch(branch)()

	// Any failure below leaves this branch half-rebuilt — last_commit is cleared
	// on the next line, before the first phase runs. Drop its version row so the
	// next startup retries THIS branch and no other. Registered after the unlock
	// defer, so it runs while the branch lock is still held.
	defer func() {
		if err != nil {
			si.clearSchemaVersion(ctx, branch)
		}
	}()

	// Bump the generation BEFORE clearing the watermark: a reader whose
	// first read saw the old generation and the watermark either sees ""
	// (unsettled) or, once the Rebuild is done, the new generation.
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, '1')
		 ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)`, RebuildGenKey(branch)); err != nil {
		return fmt.Errorf("rebuild: bump generation: %w", err)
	}
	// A branch that had a watermark before this Rebuild has been settled: keep
	// that fact across the cleared watermark (a store indexed before
	// SettledOnceKey existed gets it here, before its watermark is cleared).
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
		`INSERT OR IGNORE INTO meta(key, value) SELECT ?, '1' FROM meta WHERE key = ? AND value != ''`,
		SettledOnceKey(branch), "last_commit:"+branch); err != nil {
		return fmt.Errorf("rebuild: keep settled marker: %w", err)
	}
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
	if emb := si.rh.Embedder(); emb != nil {
		if err := si.ensureFactsVec(ctx, emb.ID(), emb.Dim()); err != nil {
			return fmt.Errorf("rebuild: ensure facts_vec: %w", err)
		}
	}

	// Phase 0: commit history. Rewrite commit_log from git BEFORE facts, because
	// fact indexing resolves each fact's source commit from commit_log. This is
	// what makes :rebuild actually rebuild the commit history (incl. author
	// identity) instead of leaving stale rows that a plain re-walk would skip.
	start := time.Now()
	if err := si.rh.RebuildCommitLog(ctx, branch); err != nil {
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
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
		`PRAGMA analysis_limit=1000; ANALYZE`); err != nil {
		log.Warn().Err(err).Msg("rebuild: analyze failed (query plans may be degraded)")
	}

	// Bump THIS branch's version only. A branch whose rebuild failed keeps its
	// old (or absent) row and is retried on the next startup on its own, without
	// dragging every other branch through another full rebuild.
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`,
		schemaVersionKey(branch), GraphSchemaVersion,
	); err != nil {
		return fmt.Errorf("rebuild: bump graph schema version: %w", err)
	}

	// Persist the embedding identity alongside the schema version so a later
	// model/dim change is detected by NeedsRebuild. Written on success only,
	// after facts_vec was (re)created and the corpus re-embedded at this dim.
	if emb := si.rh.Embedder(); emb != nil {
		if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
			`INSERT OR REPLACE INTO meta(key, value) VALUES ('embed_model_id', ?)`, emb.ID()); err != nil {
			return fmt.Errorf("rebuild: persist embed_model_id: %w", err)
		}
		if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
			`INSERT OR REPLACE INTO meta(key, value) VALUES ('embed_dim', ?)`, emb.Dim()); err != nil {
			return fmt.Errorf("rebuild: persist embed_dim: %w", err)
		}
	}

	// Every derived row of the branch was just rewritten at head, so a
	// marker a failed sync left (naming any head) is void: clear it with
	// the watermark, or the rebuilt branch stays unsettled while idle
	// (review R2-1).
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx,
		`DELETE FROM meta WHERE key = ?`, SyncPendingKey(branch)); err != nil {
		return fmt.Errorf("rebuild: clear pending marker: %w", err)
	}
	return si.setLastCommit(ctx, branch, head)
}

// reResolvePendingBranches re-resolves the reference edges of every branch
// the one-time per-blob reset marked pending (resetBlobRefsOnce), and clears
// each mark once its branch is done. rebuilt is the branch whose phase B just
// ran: its mark is cleared without another pass, and done holds the (commit,
// path) events it resolved, which the other branches skip.
//
// A branch where an edge write FAILED keeps its mark (rebuiltFailed for the
// rebuilt branch): the next rebuild drains it again, as it does after a
// cancellation. The rebuild itself still succeeds; failing it would turn a
// permanent per-blob failure into a full re-index on every boot (the PR #73
// lesson in kb/architecture/store/graph-schema-version).
//
// It does not take the other branches' locks (a nested LockBranch could
// deadlock against a writer taking them in another order). That is safe
// because phase B only adds reference edges and claims, and each blob's claim
// and edges are written in one transaction that re-checks the claim
// (applyBlobRefs); a branch being written concurrently gets its own blobs
// resolved by its own index sync either way.
func (si *Index) reResolvePendingBranches(ctx context.Context, rebuilt string, done map[string]struct{}, rebuiltFailed bool) error {
	pending, err := si.pendingRefBranches(ctx)
	if err != nil {
		return err
	}
	for _, b := range pending {
		failed := rebuiltFailed
		if b != rebuilt {
			if failed, err = si.reResolveBranchRefs(ctx, b, done); err != nil {
				return fmt.Errorf("re-resolve reference edges of %s: %w", b, err)
			}
		}
		if failed {
			log.Warn().Str("branch", b).Msg("re-resolve reference edges: some edge writes failed; the branch stays pending for the next rebuild")
			continue
		}
		if err := si.clearPendingRefBranch(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

// reResolveBranchRefs is phase B for one branch: the reference edges of every
// commit_log event on it, oldest first, skipping the events in done (and
// adding the ones it resolves). A branch no longer in `branches` has no
// events and is done. failed reports whether any edge write failed (the
// event is then not marked done, so a later pass retries it).
func (si *Index) reResolveBranchRefs(ctx context.Context, branch string, done map[string]struct{}) (failed bool, err error) {
	if err := si.repairRecordedCommits(ctx, branch); err != nil {
		return false, err
	}
	rows, err := conn(ctx, si.rh.DB()).QueryContext(ctx, `
		SELECT cl.commit_hash, cl.path
		FROM commit_log cl
		JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
		WHERE bc.branch_id = (SELECT id FROM branches WHERE name = ?)
		  AND cl.action != 'deleted'
		ORDER BY cl.committed_at ASC, cl.rowid ASC`, branch)
	if err != nil {
		return false, fmt.Errorf("query commit_log: %w", err)
	}
	type commitPath struct{ commitHash, path string }
	var events []commitPath
	for rows.Next() {
		var e commitPath
		if err := rows.Scan(&e.commitHash, &e.path); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan commit_log: %w", err)
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("commit_log rows: %w", err)
	}
	repoID := si.localRepoID(ctx, branch)
	for _, e := range events {
		key := e.commitHash + "|" + e.path
		if _, ok := done[key]; ok {
			continue
		}
		done[key] = struct{}{}
		blobHash, err := si.rh.ReadBlobHashAtCommit(ctx, e.path, e.commitHash)
		if err != nil {
			continue
		}
		content, err := si.rh.ReadFileAtCommit(ctx, e.path, e.commitHash)
		if err != nil {
			continue
		}
		rec, err := facts.ParseFactRecord(e.path, content)
		if err != nil {
			continue
		}
		specs := refSpecsOf(rec.Refs, repoID)
		if len(specs) == 0 {
			continue
		}
		srcID, err := si.graphNodeIDByBlob(ctx, e.path, blobHash)
		if err != nil {
			return false, fmt.Errorf("source node lookup: %w", err)
		}
		if srcID == 0 {
			continue // the version has no graph node (orphaned): as phase B
		}
		if err := si.graphAddDerivedFromAtCommitTx(ctx, si.rh.DB(), branch, e.path, blobHash, e.commitHash, specs, repoID); err != nil {
			failed = true
			delete(done, key)
			log.Warn().Err(err).Str("branch", branch).Str("path", e.path).Str("commit", e.commitHash[:8]).
				Msg("re-resolve reference edges: edge write failed")
		}
	}
	return failed, nil
}

// recordedCommit is the commit recorded for (path, blob) on a branch whose
// tip is head: branch_facts.commit_hash, the anchor of HEAD explain /
// Connections (kb/conventions/store/active-commit-via-branch-facts). It must
// HOLD blob at path, and the incremental index and a rebuild must record the
// same one, so both call this:
//   - the LIVE introduction of the path's content at head: path_changes'
//     change live on head's first-parent line, as the entry it resolves to
//     (a merge carrying the content resolves to the commit that wrote it),
//     when that entry's blob is blob;
//   - else LastCommitForPath (the latest non-merge commit touching the path;
//     it can name a commit without the file after a merge,
//     kb/gotchas/store/git/last-commit-for-path) only if it holds blob;
//   - else head, which holds it (the file was read at head).
//
// It reads git only on the fallbacks, so callers keep it outside any write
// transaction.
func (si *Index) recordedCommit(ctx context.Context, branch, path, blob, head string) string {
	if head != "" {
		if entry, _, err := vcs.LiveChange(ctx, conn(ctx, si.rh.DB()), vcs.ActiveTables, path, head, 0); err == nil && entry != "" {
			var b string
			if err := conn(ctx, si.rh.DB()).QueryRowContext(ctx,
				`SELECT blob FROM path_changes WHERE path = ? AND commit_hash = ?`, path, entry).Scan(&b); err == nil && b == blob {
				return entry
			}
		}
	}
	holds := func(c string) bool {
		got, err := si.rh.ReadBlobHashAtCommit(ctx, path, c)
		return err == nil && got == blob
	}
	if last, err := si.rh.LastCommitForPath(ctx, branch, path); err == nil && last != "" && holds(last) {
		return last
	}
	return head
}

// repairRecordedCommits rewrites the branch's branch_facts.commit_hash values
// to recordedCommit's, for a branch the upgrade drains but does not rebuild
// (rebuildFacts already records them for a rebuilt branch). Older indexes
// recorded LastCommitForPath's commit, which after a merge can be a commit
// that does not hold the file, and then HEAD explain on that branch is empty.
//
// Each row is updated only if it still has the fact and commit it was read
// with, so a concurrent index sync of that branch (no lock is held here) is
// never overwritten with an answer computed from an older tip. Idempotent: the
// drain clears the branch's pending mark only after this and the edges ran.
func (si *Index) repairRecordedCommits(ctx context.Context, branch string) error {
	head, err := si.rh.HeadCommit(ctx, branch)
	if err != nil {
		return nil // the branch is gone: nothing to repair
	}
	rows, err := conn(ctx, si.rh.DB()).QueryContext(ctx, `
		SELECT bf.branch_id, bf.path, bf.fact_id, bf.commit_hash, f.blob_hash
		  FROM branch_facts bf
		  JOIN facts f ON f.id = bf.fact_id
		 WHERE bf.branch_id = (SELECT id FROM branches WHERE name = ?)`, branch)
	if err != nil {
		return fmt.Errorf("repair recorded commits: %w", err)
	}
	type row struct {
		branchID, factID   int64
		path, commit, blob string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.branchID, &r.path, &r.factID, &r.commit, &r.blob); err != nil {
			rows.Close()
			return fmt.Errorf("repair recorded commits: %w", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("repair recorded commits: %w", err)
	}
	fixed := 0
	for _, r := range all {
		want := si.recordedCommit(ctx, branch, r.path, r.blob, head)
		if want == r.commit {
			continue
		}
		res, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
			UPDATE branch_facts SET commit_hash = ?
			 WHERE branch_id = ? AND path = ? AND fact_id = ? AND commit_hash = ?`,
			want, r.branchID, r.path, r.factID, r.commit)
		if err != nil {
			return fmt.Errorf("repair recorded commits: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fixed++
		}
	}
	if fixed > 0 {
		log.Info().Str("branch", branch).Int("rows", fixed).Msg("repaired branch_facts commits that did not hold their blob")
	}
	return nil
}

// indexFile reads a single file from git, parses it as a fact, and upserts
// it into the index. Files that fail to parse (e.g. README.md manifest) are
// silently skipped.
//
// commitHash is the branch tip; the commit recorded for the path is
// recordedCommit's (a commit that holds this blob), not the tip.
//
// Returns the FactRecord that was upserted, or nil if the file was skipped
// (not a fact file). The commit stored in FactRecord.SourceCommit is the
// resolved per-path commit, NOT the fallback passed in.
func (si *Index) indexFile(ctx context.Context, branch, path, commitHash string) (*facts.FactRecord, error) {
	content, blobHash, err := si.rh.ReadFileWithHash(ctx, branch, path)
	if err != nil {
		return nil, fmt.Errorf("indexFile: read %s: %w", path, err)
	}

	commitHash = si.recordedCommit(ctx, branch, path, blobHash, commitHash)

	rec, err := facts.ParseFactRecord(path, content)
	if err != nil {
		return nil, nil // not a fact file (e.g. README.md manifest, the ontology file)
	}
	rec.BlobHash = blobHash
	rec.SourceCommit = commitHash

	return &rec, si.upsert(ctx, branch, commitHash, rec)
}

// ── GC ────────────────────────────────────────────────────────────────────────

// GC removes orphaned data: facts not referenced by any branch, their graph
// nodes, orphaned Entity/Domain/OntologyNode graph nodes, and commit_log
// entries for deleted branches.
func (si *Index) GC(ctx context.Context) error {
	// 1. Collect orphaned facts before deleting (needed for graph cleanup).
	type orphanFact struct {
		id       int64
		path     string
		blobHash string
	}
	rows, err := conn(ctx, si.rh.DB()).QueryContext(ctx,
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
			if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `DELETE FROM facts WHERE id = ?`, o.id); err != nil {
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
func (si *Index) gcOrphanedGraphNodes(ctx context.Context, label, edgeType string) error {
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
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
func (si *Index) rebuildFacts(ctx context.Context, branch, head string, progress model.RebuildProgress) (int, error) {
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

	// The commit recorded per path, the same function the incremental index
	// uses (recordedCommit), so rebuild == incremental for branch_facts. It is
	// computed BEFORE the transaction: its fallbacks read git, and git object
	// reads take a pool connection of their own.
	recorded := make([]string, len(paths))
	for i := range paths {
		if si.rh.IsFactPath(paths[i]) {
			recorded[i] = si.recordedCommit(ctx, branch, paths[i], hashes[i], head)
		}
	}

	// The whole of phase 1 — facts upsert + derived junction/token regeneration
	// + branch_facts — runs in ONE transaction. This is load-bearing: the
	// derived-state regeneration below clears tables before repopulating them
	// (e.g. the global fact_domain_tokens rebuild), so without an enclosing tx a
	// mid-rebuild failure or a concurrent WAL reader would observe an empty
	// table. beginTxIfNeeded composes with an outer tx if a caller ever supplies
	// one; otherwise we own and commit it here.
	ctx, tx, ownTx, err := beginTxIfNeeded(ctx, si.rh.DB())
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: begin tx: %w", err)
	}
	if ownTx {
		defer tx.Rollback()
	}

	// Create temp table for the rebuild entries.
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS _rebuild_entries (path TEXT PRIMARY KEY, blob_hash TEXT NOT NULL, commit_hash TEXT NOT NULL DEFAULT '')`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: create temp table: %w", err)
	}
	// Ensure it's empty (in case of prior incomplete rebuild).
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `DELETE FROM _rebuild_entries`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear temp table: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO _rebuild_entries(path, blob_hash, commit_hash) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: prepare insert: %w", err)
	}
	defer stmt.Close()

	// Only the ontology root is a candidate (IsFactPath). _rebuild_entries is
	// also the set the branch_facts repopulate below is driven from, so scoping
	// it here is what EVICTS a stray admitted by an older build — a rebuild is
	// the repair path for an index that outran the rule.
	for i := range paths {
		if !si.rh.IsFactPath(paths[i]) {
			continue
		}
		if _, err := stmt.ExecContext(ctx, paths[i], hashes[i], recorded[i]); err != nil {
			return 0, fmt.Errorf("rebuildFacts: insert entry %s: %w", paths[i], err)
		}
	}

	// Drop stale branch_facts pointers for paths no longer present at HEAD; the
	// INSERT OR REPLACE below repopulates the live ones. Rows are NOT cleared to
	// dodge an FK violation (facts rowids are preserved — see below), only to
	// purge paths that vanished from the branch.
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `DELETE FROM branch_facts WHERE branch_id = ?`, branchID); err != nil {
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
	res, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
		WITH parsed_entries AS (
			SELECT e.path, e.blob_hash, knomit_parse_fact(o.data) AS parsed
			FROM _rebuild_entries e
			JOIN objects o ON o.hash = e.blob_hash AND o.type = ?
		)
		INSERT INTO facts (path, blob_hash, title, kind, type, domain, entities, motifs, confidence, sources, refs, evidence_weight, origin)
		SELECT
			pe.path,
			pe.blob_hash,
			json_extract(pe.parsed, '$.title'),
			COALESCE(json_extract(pe.parsed, '$.kind'), 'epistemic'),
			json_extract(pe.parsed, '$.type'),
			json_extract(pe.parsed, '$.domain'),
			json_extract(pe.parsed, '$.entities'),
			COALESCE(json_extract(pe.parsed, '$.motifs'), '[]'),
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
			motifs          = excluded.motifs,
			confidence      = excluded.confidence,
			sources         = excluded.sources,
			refs            = excluded.refs,
			evidence_weight = excluded.evidence_weight,
			origin          = excluded.origin
	`, vcs.BlobObjectType)
	if err != nil {
		return 0, fmt.Errorf("rebuildFacts: upsert facts: %w", err)
	}

	// Repopulate the fact_expires side table (migration 000026) for the
	// rebuilt entries. Only blobs that contain the token "expires" are
	// re-parsed — the instr() prefilter keeps this second pass from parsing the
	// whole corpus again. The token, NOT "expires:": YAML also accepts
	// `expires : …`, which the incremental path indexes, and the repair path
	// must not disagree with it. A blob that merely mentions the word parses to
	// a NULL expires and is skipped by the WHERE. REPLACE, not IGNORE: a rebuild is
	// the repair path, so it must overwrite a row an older build got wrong.
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
		WITH dated AS (
			SELECT f.id AS fact_id, knomit_parse_fact(o.data) AS parsed
			FROM _rebuild_entries e
			JOIN facts f ON f.path = e.path AND f.blob_hash = e.blob_hash
			JOIN objects o ON o.hash = e.blob_hash AND o.type = ?
			WHERE instr(o.data, 'expires') > 0
		)
		INSERT OR REPLACE INTO fact_expires (fact_id, expires, expires_at)
		SELECT fact_id, json_extract(parsed, '$.expires'), json_extract(parsed, '$.expires_at')
		FROM dated
		WHERE json_extract(parsed, '$.expires_at') IS NOT NULL
	`, vcs.BlobObjectType); err != nil {
		return 0, fmt.Errorf("rebuildFacts: fact_expires: %w", err)
	}

	// Repopulate the fact_context side table (migration 000034, F22) the same
	// way. The rebuilt facts' rows are DELETED first (scoped by
	// _rebuild_entries, so other branches' shared rows stay): a rebuild is the
	// repair path, and a key an older build wrongly kept must go. Then only
	// blobs containing the token "context" are re-parsed (the instr prefilter,
	// as for expires); a malformed map parses to no context and writes no row.
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
		DELETE FROM fact_context WHERE fact_id IN (
			SELECT f.id FROM facts f
			JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		)
	`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear fact_context: %w", err)
	}
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
		WITH labelled AS (
			SELECT f.id AS fact_id, knomit_parse_fact(o.data) AS parsed
			FROM _rebuild_entries e
			JOIN facts f ON f.path = e.path AND f.blob_hash = e.blob_hash
			JOIN objects o ON o.hash = e.blob_hash AND o.type = ?
			WHERE instr(o.data, 'context') > 0
		)
		INSERT OR REPLACE INTO fact_context (fact_id, key, value, num)
		SELECT l.fact_id, je.key, je.value, json_extract(l.parsed, '$.context_num."' || je.key || '"')
		FROM labelled l, json_each(l.parsed, '$.context') je
		WHERE json_extract(l.parsed, '$.context') IS NOT NULL
	`, vcs.BlobObjectType); err != nil {
		return 0, fmt.Errorf("rebuildFacts: fact_context: %w", err)
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
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
		DELETE FROM fact_domains WHERE fact_id IN (
			SELECT f.id FROM facts f
			JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		)
	`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear fact_domains: %w", err)
	}
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
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
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
		DELETE FROM fact_entities WHERE fact_id IN (
			SELECT f.id FROM facts f
			JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		)
	`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear fact_entities: %w", err)
	}
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
		INSERT OR IGNORE INTO fact_entities(fact_id, entity)
		SELECT DISTINCT f.id, j.value
		FROM facts f
		JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		JOIN json_each(f.entities) j
		WHERE j.value IS NOT NULL AND j.value != ''
	`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: repopulate fact_entities: %w", err)
	}
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
		DELETE FROM fact_motifs WHERE fact_id IN (
			SELECT f.id FROM facts f
			JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		)
	`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: clear fact_motifs: %w", err)
	}
	// No knomit_canon_* wrapper, unlike fact_domains above: motifs are stored
	// AS WRITTEN (MN3). DISTINCT + OR IGNORE anyway, because the primary key
	// is (fact_id, motif) and the column is COLLATE NOCASE — two spellings
	// that differ only in case collide there even though neither is rewritten.
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
		INSERT OR IGNORE INTO fact_motifs(fact_id, motif)
		SELECT DISTINCT f.id, j.value
		FROM facts f
		JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
		JOIN json_each(f.motifs) j
		WHERE j.value IS NOT NULL AND j.value != ''
	`); err != nil {
		return 0, fmt.Errorf("rebuildFacts: repopulate fact_motifs: %w", err)
	}

	// Populate branch_facts: link each fact to this branch with the commit
	// recordedCommit chose for it above (a commit that holds the blob; the
	// incremental index records the same one).
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `
		INSERT OR REPLACE INTO branch_facts (branch_id, path, fact_id, commit_hash)
		SELECT ?, f.path, f.id, e.commit_hash
		FROM facts f
		JOIN _rebuild_entries e ON e.path = f.path AND e.blob_hash = f.blob_hash
	`, branchID); err != nil {
		return 0, fmt.Errorf("rebuildFacts: populate branch_facts: %w", err)
	}

	// Clean up temp table.
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `DROP TABLE IF EXISTS _rebuild_entries`); err != nil {
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
//
// batchSize bounds a DOCUMENT COUNT — the number of rowids in one IN clause and
// so the number of bodies held at once — not any byte total. It is no longer
// the inference safety rail: the embedder now splits whatever it is handed into
// batches bounded by a padded-token budget, so a chunk of long facts becomes
// several session.Runs rather than one oversized one.
//
// Raising it is therefore safe for INFERENCE memory specifically. It is not
// free: raw bodies, the tokenized rows encodeAll retains, and the result slice
// all scale linearly with it. It stays at 32 until a raise is measured.
func (si *Index) rebuildEmbeddings(ctx context.Context, progress model.RebuildProgress) (int, error) {
	emb := si.rh.Embedder()
	if emb == nil {
		return 0, nil
	}

	batcher, hasBatch := emb.(model.BatchEmbedder)

	var total int
	if err := conn(ctx, si.rh.DB()).QueryRowContext(ctx, `SELECT COUNT(*) FROM facts WHERE rowid NOT IN (SELECT rowid FROM facts_vec)`).Scan(&total); err != nil {
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
	rows, err := conn(ctx, si.rh.DB()).QueryContext(ctx, `SELECT rowid, path FROM facts WHERE rowid NOT IN (SELECT rowid FROM facts_vec)`)
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
		qargs := append([]any{vcs.BlobObjectType}, args...)

		bodyRows, err := conn(ctx, si.rh.DB()).QueryContext(ctx, q, qargs...)
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

		tx, err := si.rh.DB().BeginTx(ctx, nil)
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
func (si *Index) rebuildGraph(ctx context.Context, branch string, progress model.RebuildProgress) (int, error) {
	// P2 git-read dedupe: Phase A.5 (historical Fact-node restore) and Phase B
	// (DERIVED_FROM edges) both walk the identical commit_log ref-event list,
	// and each historically re-read every (commit, path) from git
	// independently (~3 reads per event). Read each unique event ONCE here —
	// blob hash + content, parsed once — and let both phases consume the
	// cache. Git reads (go-git) touch no DB connection, so building the cache
	// here (before the write-locked tx) also keeps this I/O off the
	// _txlock=immediate critical section.
	// commit_log ref-events on this branch, oldest first — the shared walk
	// order for both phases. Queried before the tx so its connection is
	// released before BEGIN IMMEDIATE (no pool straddle under
	// _txlock=immediate). commit_log/branch_commits are regular tables.
	evRows, err := conn(ctx, si.rh.DB()).QueryContext(ctx, `
		SELECT cl.commit_hash, cl.path
		FROM commit_log cl
		JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
		WHERE bc.branch_id = (SELECT id FROM branches WHERE name = ?)
		  AND cl.action != 'deleted'
		ORDER BY cl.committed_at ASC, cl.rowid ASC`, branch)
	if err != nil {
		return 0, fmt.Errorf("rebuildGraph: query commit_log: %w", err)
	}
	type commitPath struct{ commitHash, path string }
	var events []commitPath
	for evRows.Next() {
		var e commitPath
		if err := evRows.Scan(&e.commitHash, &e.path); err != nil {
			evRows.Close()
			return 0, fmt.Errorf("rebuildGraph: scan commit_log: %w", err)
		}
		events = append(events, e)
	}
	evRows.Close()

	// One git read per unique (commit, path): blob hash + content, parsed
	// once. ok=true means all three succeeded — the gate both phases need.
	type commitFact struct {
		blobHash string
		rec      facts.FactRecord
		ok       bool
	}
	factAtCommit := make(map[string]commitFact, len(events))
	for _, e := range events {
		key := e.commitHash + "|" + e.path
		if _, seen := factAtCommit[key]; seen {
			continue
		}
		var cf commitFact
		blobHash, err := si.rh.ReadBlobHashAtCommit(ctx, e.path, e.commitHash)
		if err != nil {
			log.Debug().Err(err).Str("path", e.path).Str("commit", e.commitHash[:8]).
				Msg("rebuildGraph: skip (blob_hash lookup failed)")
			factAtCommit[key] = cf
			continue
		}
		content, err := si.rh.ReadFileAtCommit(ctx, e.path, e.commitHash)
		if err != nil {
			log.Debug().Err(err).Str("path", e.path).Str("commit", e.commitHash[:8]).
				Msg("rebuildGraph: skip (file not at commit)")
			factAtCommit[key] = cf
			continue
		}
		rec, err := facts.ParseFactRecord(e.path, content)
		if err != nil {
			log.Debug().Err(err).Str("path", e.path).Msg("rebuildGraph: skip (parse failed)")
			factAtCommit[key] = cf
			continue
		}
		rec.BlobHash = blobHash
		factAtCommit[key] = commitFact{blobHash: blobHash, rec: rec, ok: true}
	}

	if si.beforeGraphTx != nil {
		si.beforeGraphTx()
	}

	// Single transaction for all graph sync operations.
	tx, err := si.rh.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rebuildGraph: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := resetBlobRefsOnce(ctx, tx); err != nil {
		return 0, fmt.Errorf("rebuildGraph: %w", err)
	}
	if n, err := dropDanglingRefEdges(ctx, tx); err != nil {
		return 0, fmt.Errorf("rebuildGraph: %w", err)
	} else if n > 0 {
		log.Warn().Int64("edges", n).Msg("rebuildGraph: dropped reference edges whose blob is not in the object store")
	}

	// The facts snapshot is read INSIDE the write transaction. `facts` rows and
	// graph Fact nodes are keyed by version and shared by every branch, while
	// LockBranch is per branch: a delete on another branch runs concurrently
	// with this rebuild. Read outside the transaction, a delete landing between
	// the snapshot and BEGIN would remove the facts row and soft-delete the
	// node, and the sync below would re-merge it live from the stale snapshot.
	// Under _txlock=immediate the transaction holds the write lock from BEGIN,
	// so any other writer lands wholly before this read or wholly after commit.
	// The git-read cache above stays outside: it reads no table this
	// transaction writes, and keeps git I/O off the write lock.
	// Read all facts ordered by oldest commit first so that when a fact's
	// DERIVED_FROM edges are created, its ref targets are already graph nodes.
	rows, err := tx.QueryContext(ctx, `
		SELECT f.path, f.title, f.blob_hash, f.kind, f.type, f.domain, f.entities, f.confidence, f.sources, f.refs, f.evidence_weight, f.origin
		FROM facts f
		LEFT JOIN (
			SELECT path, MIN(committed_at) AS first_committed FROM commit_log GROUP BY path
		) cl ON cl.path = f.path
		ORDER BY cl.first_committed ASC`)
	if err != nil {
		return 0, fmt.Errorf("rebuildGraph: query facts: %w", err)
	}

	var recs []facts.FactRecord
	for rows.Next() {
		var rec facts.FactRecord
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
		recs = append(recs, rec)
	}
	rows.Close()

	total := len(recs)
	if progress != nil {
		progress("graph", 0, total)
	}

	currentSet := make(map[string]struct{}, len(recs))
	for _, f := range recs {
		currentSet[f.Path+"|"+f.BlobHash] = struct{}{}
	}

	for i, rec := range recs {
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
	// them live nodes. Consumes the shared git-read cache built above.
	seenHistorical := make(map[string]struct{})
	historicalSynced := 0
	for _, e := range events {
		cf := factAtCommit[e.commitHash+"|"+e.path]
		if !cf.ok {
			continue
		}
		key := e.path + "|" + cf.blobHash
		if _, ok := currentSet[key]; ok {
			continue // current version, Phase A already handled
		}
		if _, ok := seenHistorical[key]; ok {
			continue // already MERGE'd this historical version
		}
		seenHistorical[key] = struct{}{}

		if err := si.graphSyncHistoricalFactTx(ctx, tx, cf.rec); err != nil {
			log.Warn().Err(err).Str("path", e.path).Str("blob", cf.blobHash[:8]).
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

	// Phase B: walk commit_log to write DERIVED_FROM edges. Each
	// (commit_hash, path, action != 'deleted') tuple is an arrival of the
	// blob at that commit; graphAddDerivedFromAtCommitTx writes a blob's
	// edges once, at its earliest introduction (claimBlobRefs), so later
	// arrivals add nothing. Each ref's target is resolved via
	// resolveTargetCommit.
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
	// Consumes the shared git-read cache built above (same `events` order).
	// done records the (commit, path) events resolved here, so re-resolving
	// the pending branches below skips what this branch already did (the
	// resolution does not depend on the branch).
	done := make(map[string]struct{}, len(events))
	ownFailed := false
	for _, e := range events {
		cf := factAtCommit[e.commitHash+"|"+e.path]
		if !cf.ok {
			continue
		}
		done[e.commitHash+"|"+e.path] = struct{}{}

		repoID := si.localRepoID(ctx, branch)
		specs := refSpecsOf(cf.rec.Refs, repoID)
		if len(specs) == 0 {
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
		srcID, err := si.graphNodeIDByBlob(ctx, e.path, cf.blobHash)
		if err != nil {
			log.Warn().Err(err).Str("path", e.path).Str("commit", e.commitHash[:8]).Msg("rebuildGraph phaseB: source node lookup failed")
			continue
		}
		if srcID == 0 {
			log.Debug().Str("path", e.path).Str("commit", e.commitHash[:8]).Str("blob", cf.blobHash[:8]).
				Msg("rebuildGraph phaseB: skip (source blob orphaned out of facts; no graph node)")
			continue
		}

		if err := si.graphAddDerivedFromAtCommitTx(ctx, si.rh.DB(), branch, e.path, cf.blobHash, e.commitHash, specs, repoID); err != nil {
			ownFailed = true
			delete(done, e.commitHash+"|"+e.path)
			log.Warn().Err(err).Str("path", e.path).Str("commit", e.commitHash[:8]).Msg("rebuildGraph phaseB: edge write failed")
		}
	}

	// Phase B': the GraphSchemaVersion 8 reset dropped the reference edges of
	// EVERY branch, not only this one; re-resolve every branch it marked
	// pending (open experiments included), then clear the mark. Runs before
	// the caller bumps this branch's version, so an interrupted drain is
	// re-run by the next rebuild.
	if err := si.reResolvePendingBranches(ctx, branch, done, ownFailed); err != nil {
		return 0, fmt.Errorf("rebuildGraph: %w", err)
	}

	// ref_edges mirrors the DERIVED_FROM edges. The write path keeps the two
	// in step, but a store can drift (a crash before PR B's backfill, a
	// hand edit, a bug): rewrite the rows of every blob whose rows disagree
	// with its edges, so drift Verify reports does not outlive a rebuild. A
	// failure is logged, not fatal (the next rebuild retries; failing it
	// would turn a permanent per-blob failure into a re-index on every boot).
	if _, err := si.repairRefEdges(ctx, branch); err != nil {
		log.Warn().Err(err).Str("branch", branch).Msg("rebuildGraph: ref_edges repair failed; the next rebuild retries")
	}
	// The v8 reset (resetBlobRefsOnce, above) clears the backfill sentinel,
	// and nothing else would set it again until the next heal: an explicit
	// full Rebuild (spec.full) never runs the heal's backfill, so the readers
	// would stay on the edge fallback indefinitely (PR #694 review round 2).
	// Every rebuild therefore finishes the job when the sentinel is unset.
	// Not fatal: on failure the readers keep falling back and the next heal
	// or rebuild retries.
	if !refEdgesReady(ctx, conn(ctx, si.rh.DB())) {
		if _, err := si.BackfillRefEdges(ctx, branch); err != nil {
			log.Warn().Err(err).Str("branch", branch).Msg("rebuildGraph: ref_edges backfill failed; reads fall back to the edges until it runs")
		}
	}

	// Build similarity edges after commit (needs committed data for KNN).
	// Batch: collect all neighbors first, then write all edges in one transaction.
	if si.rh.Embedder() != nil {
		type simEdge struct{ fromPath, fromBH, toPath, toBH string }
		var edges []simEdge
		simFloor := model.EmbedderThresholds(si.rh.Embedder()).SimilarTo

		for _, rec := range recs {
			emb, err := si.getEmbeddingByFact(ctx, rec.Path, rec.BlobHash)
			if err != nil || emb == nil {
				continue
			}
			vecBlob := float32SliceToBytes(emb)
			rows, err := conn(ctx, si.rh.DB()).QueryContext(ctx,
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
		// LockBranch, so it can commit a new fact version and its similarity
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
		simTx, err := si.rh.DB().BeginTx(ctx, nil)
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
		rewritten := make([]int64, 0, len(recs))
		for _, rec := range recs {
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

// localFactRefs returns the refs naming a fact in THIS repo, as repo-relative
// paths — the only refs that can become DERIVED_FROM edges.
//
// A kb://<own-id>/<path> ref resolves to a local edge: kb://<id>/… is the
// documented canonical form, so writing a ref in canonical form must not
// silently produce nothing. The filter this replaced kept "anything not
// http(s)", which handed resolveTargetCommit literal kb:// and src:// strings
// as paths — they never matched, so the edge was dropped without a trace.
//
// Foreign kb:// refs, src:// refs (knomit holds no source objects), external
// URLs, and malformed refs are all excluded. It is the local half of
// refSpecsOf (which the edge writers use, for the order and the cross-KB
// refs of ref_edges): a path cited twice is listed once.
func localFactRefs(refs []string, localRepoID string) []string {
	return localPaths(refSpecsOf(refs, localRepoID))
}

// localRepoID returns this repo's 12-hex identity, resolving it once.
//
// Empty when unresolvable: ClassifyRef then treats every kb:// ref as foreign,
// so self-qualified refs stop forming edges. That under-reports rather than
// inventing edges, which is the safe direction.
//
// CALL CONSTRAINT — this reads git, which for knomit means reading SQLite:
// RootCommit walks first-parent ancestry through the SQLite-backed storer.
// Two consequences a future caller must preserve:
//
//  1. Call it OUTSIDE an open write transaction. The DB runs with
//     _txlock=immediate, so an open tx holds the process-wide writer lock, and
//     a commit walk under it stalls every other writer — the same failure mode
//     as running embeddings inside a write tx (see the embed-outside-write-tx
//     invariant). Every current call site satisfies this: rebuildGraph's phase
//     B runs after its tx.Commit, and all three writePostCommitDerivedFrom
//     callers are post-commit, as the name says.
//  2. Never call it from a SQLite virtual-table cursor callback. knomit
//     registers no vtables today (the ConnectHook installs scalar UDFs only),
//     so this is preventative — but a cursor callback that re-enters the same
//     *sql.DB deadlocks the pool under concurrency. See
//     kb/invariants/store/no-reentrant-sql-from-vtab-cursors.
//
// The cache keeps the cost to one walk per index.Index once resolved, not one
// per ref. The branch argument is therefore only read until the id resolves —
// safe because every branch of a repo shares the same root commit (see
// RootCommit). A FAILED walk is not cached: the next call retries, so one
// transient RootCommit error does not leave kb:// self-refs edgeless, and the
// ref_edges repair and backfill disabled, for the life of the process (PR
// #694 review F1).
func (si *Index) localRepoID(ctx context.Context, branch string) string {
	si.repoIDMu.Lock()
	defer si.repoIDMu.Unlock()
	if si.repoID != "" {
		return si.repoID
	}
	lookup := si.rootCommit
	if lookup == nil {
		lookup = si.rh.RootCommit
	}
	root, err := lookup(ctx, branch)
	if err != nil {
		log.Warn().Err(err).Str("branch", branch).
			Msg("searchIndex: repo id unresolved; kb:// self-refs will not form edges (retried on the next call)")
		return ""
	}
	si.repoID = fact.ID12(root)
	return si.repoID
}
