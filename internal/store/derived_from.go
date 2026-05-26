package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/rs/zerolog/log"
)

// resolveTargetCommit walks first-parent ancestry of sourceCommit on the
// given branch looking for the most recent commit where refPath was
// **added or modified**, and returns (commit_hash, ok=true). Deletions
// are write events too: the walk passes through them to find the prior
// valid version, so refs to retracted targets resolve to the last live
// commit. Returns ("", false, nil) only when refPath has never been
// added/modified anywhere in the source's ancestry (forward-broken).
//
// Why walk past deletions: knomit stores a HISTORICAL graph. A ref written
// at source-commit C declares "this fact derives from that target". If
// the target was retracted before C, the lineage is still meaningful —
// the historical record at C must point at the target's last valid blob.
// This mirrors the fact-read fallback=before semantic. See the
// project_historical_graph_invariant memory note.
//
// First-parent ancestry (NOT wall-clock committed_at ordering) is the
// correct semantic for "the active version of refPath on this branch at
// sourceCommit". On a branch containing merge commits from sibling
// branches, wall-clock ordering can pick a sibling-branch commit as the
// "most recent" touch even when the local first-parent line carries a
// later authoritative version (e.g. when a merge resolved a conflict in
// the local side's favour, leaving no commit_log row at the merge commit).
//
// The walk stops at: the first row whose action is added/modified for
// refPath; the root commit (no parent); or the first parent that leaves
// the branch (parent ∉ branch_commits(B)). "deleted" rows do not stop
// the walk — they're stepped over.
//
// Performance: each step does one git CommitObject lookup + one indexed
// SQL query against commit_log. For branches where most refs target
// frequently-modified facts the walk is O(1) per ref. For long-stable
// targets it's O(commits-since-last-touch). An in-process cache keyed by
// (refPath, sourceCommit) per ingest call is a possible optimisation if
// this becomes hot.
func (si *searchIndex) resolveTargetCommit(ctx context.Context, branch, sourcePath, refPath, sourceCommit string) (string, bool, error) {
	cur := sourceCommit
	// Self-ref ("this fact derives from the previous version of this path"):
	// the source commit is where the current version is being written, so it
	// trivially "touches" refPath. Skip past it so the walk lands on the
	// genuine prior version (or runs off-branch if there is none).
	if refPath == sourcePath {
		parent, err := si.rh.firstParentCommit(ctx, cur)
		if err != nil {
			return "", false, fmt.Errorf("resolveTargetCommit: firstParentCommit (self-ref): %w", err)
		}
		cur = parent
	}
	return si.resolveActiveCommitForPath(ctx, branch, refPath, cur)
}

// FactExistsAt reports whether `path` has any valid (added/modified)
// version reachable from `commit` on `branch`, walking past retractions.
// Pass commit == "" for a HEAD-anchored check (uses branch_facts).
//
// This is the historical-graph existence predicate used by the ref-kind
// resolver: a ref is `fact` (not `broken`) when the target has any version
// the user can navigate to via fallback-before. A target retracted long
// before the source's commit still has a navigable last-valid blob, so
// the ref is not broken from the user's perspective.
func (si *searchIndex) FactExistsAt(ctx context.Context, branch, path, commit string) (bool, error) {
	if commit == "" {
		// HEAD anchor: a fact is "live on the branch" iff there's a
		// branch_facts row for (branch, path). branch_facts is the live
		// view of which paths are currently un-retracted on the branch.
		branchID, err := si.rh.branchID(ctx, branch)
		if err != nil {
			return false, fmt.Errorf("FactExistsAt: branchID: %w", err)
		}
		var n int
		err = conn(ctx, si.rh.db).QueryRowContext(ctx,
			`SELECT 1 FROM branch_facts WHERE branch_id = ? AND path = ?`,
			branchID, path,
		).Scan(&n)
		if err == sql.ErrNoRows {
			// Live row absent — but the fact may still be historically
			// reachable via fallback-before. Walk back from the branch
			// HEAD to find any prior add/modify.
			head, herr := si.rh.HeadCommit(ctx, branch)
			if herr != nil {
				return false, fmt.Errorf("FactExistsAt: HeadCommit: %w", herr)
			}
			_, ok, werr := si.resolveActiveCommitForPath(ctx, branch, path, head)
			if werr != nil {
				return false, fmt.Errorf("FactExistsAt: walk-back at HEAD: %w", werr)
			}
			return ok, nil
		}
		if err != nil {
			return false, fmt.Errorf("FactExistsAt: branch_facts lookup: %w", err)
		}
		return true, nil
	}
	_, ok, err := si.resolveActiveCommitForPath(ctx, branch, path, commit)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// resolveActiveCommitForPath walks first-parent ancestry of fromCommit on
// `branch`, returning the most recent commit where `path` was added or
// modified. Deletions are stepped over: they're write events in the
// sparse history, not stop conditions.
//
// Implementation: a single recursive CTE over commit_parents (parent_order
// = 0 = first parent), JOINed with commit_log and capped by LIMIT 1.
// SQLite's recursive-CTE planner is pull-based: with the outer LIMIT 1
// satisfied, the recursion stops walking — same streaming + short-circuit
// semantics as the previous first_parent_chain virtual table, without the
// Go-side cursor callback that could re-enter the *sql.DB pool mid-scan.
//
// SCHEMA INVARIANT: branch_commits is populated by full-reachability walk
// from the branch tip (populateCommitLog uses gogit.LogOrderDefault), so
// any first-parent ancestor of an on-branch commit is itself on-branch.
// The commit_log JOIN further restricts to indexed commits, so off-branch
// commits cannot appear in the result set even though the walk itself is
// branch-agnostic.
//
// Returns ("", false, nil) when no add/modify ancestor exists. Errors
// propagate.
//
// First-parent (not wall-clock) ancestry is the correct semantic — see
// resolveTargetCommit's doc-comment for the merge-branch rationale.
func (si *searchIndex) resolveActiveCommitForPath(ctx context.Context, branch, path, fromCommit string) (string, bool, error) {
	if fromCommit == "" {
		return "", false, nil
	}

	var hash string
	err := conn(ctx, si.rh.db).QueryRowContext(ctx, `
		WITH RECURSIVE fpc(commit_hash, depth) AS (
		    SELECT ?, 0
		    UNION ALL
		    SELECT cp.parent_hash, fpc.depth + 1
		      FROM commit_parents cp
		      JOIN fpc ON cp.commit_hash = fpc.commit_hash AND cp.parent_order = 0
		)
		SELECT cl.commit_hash
		  FROM fpc
		  JOIN commit_log cl ON cl.commit_hash = fpc.commit_hash
		 WHERE cl.path = ? AND cl.action IN ('added','modified')
		 ORDER BY fpc.depth ASC
		 LIMIT 1
	`, fromCommit, path).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolveActiveCommitForPath: %w", err)
	}
	_ = branch // See SCHEMA INVARIANT above — branch is implicit.
	return hash, true, nil
}

// graphAddDerivedFromAtCommitTx writes one DERIVED_FROM edge per ref-event
// from Fact(sourcePath, sourceBlobHash) to Fact(refPath, target_blob_hash),
// with edge text properties source_commit and target_commit. Refs that
// resolve to "skip" via resolveTargetCommit (forward-broken or first
// ancestor is a deletion) are silently skipped. The branch parameter is
// the ingestion branch (the branch on which sourceCommit was committed).
//
// The target Fact node already exists if resolveTargetCommit succeeds — it
// was upserted when refPath was originally indexed at target_commit. The
// target's blob_hash is the blob in the target_commit's tree for refPath,
// resolved via repoHandler.readBlobHashAtCommit.
//
// IMPORTANT: must be called AFTER the source Fact node's MERGE has been
// committed. Direct-SQL reads against the GraphQLite EAV tables cannot see
// nodes MERGE'd via Cypher inside the same *sql.Tx — node IDs only become
// visible post-commit. The tx parameter is currently unused for this reason;
// it is retained in the signature for symmetry with graphSyncFactTx and
// future call-site flexibility.
func (si *searchIndex) graphAddDerivedFromAtCommitTx(
	ctx context.Context,
	tx execer,
	branch, sourcePath, sourceBlobHash, sourceCommit string,
	refs []string,
) error {
	srcID, err := si.graphNodeIDByBlob(ctx, sourcePath, sourceBlobHash)
	if err != nil {
		return fmt.Errorf("graphAddDerivedFromAtCommitTx: source node id: %w", err)
	}
	if srcID == 0 {
		return fmt.Errorf("graphAddDerivedFromAtCommitTx: source Fact(%s,%s) not found", sourcePath, sourceBlobHash)
	}

	for _, refPath := range refs {
		targetCommit, ok, err := si.resolveTargetCommit(ctx, branch, sourcePath, refPath, sourceCommit)
		if err != nil {
			return fmt.Errorf("graphAddDerivedFromAtCommitTx: resolve %s: %w", refPath, err)
		}
		if !ok {
			continue // forward-broken or deleted-target — skip
		}

		targetBlobHash, err := si.rh.readBlobHashAtCommit(ctx, refPath, targetCommit)
		if err != nil {
			return fmt.Errorf("graphAddDerivedFromAtCommitTx: blob_hash for %s@%s: %w", refPath, targetCommit, err)
		}
		tgtID, err := si.graphNodeIDByBlob(ctx, refPath, targetBlobHash)
		if err != nil {
			return fmt.Errorf("graphAddDerivedFromAtCommitTx: target node id: %w", err)
		}
		if tgtID == 0 {
			// Target Fact node missing despite resolveTargetCommit succeeding.
			// Indicates an indexing race or stale state; skip rather than fail.
			log.Debug().Str("branch", branch).Str("ref", refPath).Str("target_commit", targetCommit).
				Msg("graphAddDerivedFromAtCommitTx: target Fact node not found, skipping (likely intra-commit ordering — Sync pass 2 will retry)")
			continue
		}

		// Dedup guard: skip if this exact (src→tgt, source_commit, target_commit)
		// edge already exists. Prevents duplicates when the two-pass sync in
		// Sync() calls writePostCommitDerivedFrom a second time for intra-batch
		// refs that succeeded on the first attempt.
		exists, err := si.graphDerivedFromEdgeExists(ctx, srcID, tgtID, sourceCommit, targetCommit)
		if err != nil {
			return fmt.Errorf("graphAddDerivedFromAtCommitTx: dedup check: %w", err)
		}
		if exists {
			continue
		}

		edgeID, err := si.graphInsertEdgeReturningID(ctx, srcID, tgtID, EdgeDerivedFrom)
		if err != nil {
			return fmt.Errorf("graphAddDerivedFromAtCommitTx: insert edge: %w", err)
		}
		if err := si.graphSetEdgeProps(ctx, edgeID, map[string]string{
			"source_commit": sourceCommit,
			"target_commit": targetCommit,
		}); err != nil {
			return fmt.Errorf("graphAddDerivedFromAtCommitTx: set props: %w", err)
		}
	}
	return nil
}

// graphDerivedFromEdgeExists reports whether a DERIVED_FROM edge with the
// given source_id → target_id and (source_commit, target_commit) properties
// already exists. Used by graphAddDerivedFromAtCommitTx as a dedup guard to
// prevent duplicate edges when the two-pass sync retries edge writes for
// refs that were already successfully wired in pass 1.
func (si *searchIndex) graphDerivedFromEdgeExists(ctx context.Context, srcID, tgtID int64, sourceCommit, targetCommit string) (bool, error) {
	var n int
	err := conn(ctx, si.rh.db).QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM edges e
		JOIN edge_props_text sc ON sc.edge_id = e.id
		JOIN property_keys ksc ON ksc.id = sc.key_id AND ksc.key = 'source_commit'
		JOIN edge_props_text tc ON tc.edge_id = e.id
		JOIN property_keys ktc ON ktc.id = tc.key_id AND ktc.key = 'target_commit'
		WHERE e.source_id = ? AND e.target_id = ? AND e.type = ?
		  AND sc.value = ? AND tc.value = ?
		LIMIT 1
	`, srcID, tgtID, EdgeDerivedFrom, sourceCommit, targetCommit).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// graphNodeIDByBlob looks up the Fact node id for (path, blob_hash). Both
// must match because Fact identity is per-version. Returns 0 if no such
// node exists (caller decides whether that's an error).
func (si *searchIndex) graphNodeIDByBlob(ctx context.Context, path, blobHash string) (int64, error) {
	var nodeID int64
	err := conn(ctx, si.rh.db).QueryRowContext(ctx, `
		SELECT npp.node_id
		FROM node_props_text npp
		JOIN property_keys kp ON kp.id = npp.key_id AND kp.key = 'path'
		JOIN node_props_text npb ON npb.node_id = npp.node_id
		JOIN property_keys kb ON kb.id = npb.key_id AND kb.key = 'blob_hash'
		JOIN node_labels nl ON nl.node_id = npp.node_id AND nl.label = ?
		WHERE npp.value = ? AND npb.value = ?
		LIMIT 1
	`, NodeFact, path, blobHash).Scan(&nodeID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return nodeID, nil
}
