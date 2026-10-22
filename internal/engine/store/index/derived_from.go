package index

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"

	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/internal/vcs"
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
func (si *Index) resolveTargetCommit(ctx context.Context, branch, sourcePath, refPath, sourceCommit string) (string, bool, error) {
	cur := sourceCommit
	// Self-ref ("this fact derives from the previous version of this path"):
	// the source commit is where the current version is being written, so it
	// trivially "touches" refPath. Skip past it so the walk lands on the
	// genuine prior version (or runs off-branch if there is none).
	if refPath == sourcePath {
		parent, err := si.rh.FirstParentCommit(ctx, cur)
		if err != nil {
			return "", false, fmt.Errorf("resolveTargetCommit: firstParentCommit (self-ref): %w", err)
		}
		cur = parent
	}
	return si.rh.ResolveActiveCommitForPath(ctx, branch, refPath, cur)
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
func (fq *FactQuery) FactExistsAt(ctx context.Context, branch, path, commit string) (bool, error) {
	fq.probe("FactExistsAt")
	// Fact paths are lowercase-canonical, but both branches below compare
	// case-sensitively: branch_facts.path is plain TEXT (no COLLATE NOCASE), and
	// the commit-anchored walk matches commit_log paths literally. Normalize
	// once here so every downstream branch sees the canonical form.
	path = strings.ToLower(path)
	if commit == "" {
		// HEAD anchor: a fact is "live on the branch" iff there's a
		// branch_facts row for (branch, path). branch_facts is the live
		// view of which paths are currently un-retracted on the branch.
		branchID, err := fq.rh.BranchID(ctx, branch)
		if err != nil {
			return false, fmt.Errorf("FactExistsAt: branchID: %w", err)
		}
		var n int
		err = conn(ctx, fq.rh.DB()).QueryRowContext(ctx,
			`SELECT 1 FROM branch_facts WHERE branch_id = ? AND path = ?`,
			branchID, path,
		).Scan(&n)
		if err == sql.ErrNoRows {
			// Live row absent — but the fact may still be historically
			// reachable via fallback-before. Walk back from the branch
			// HEAD to find any prior add/modify.
			head, herr := fq.rh.HeadCommit(ctx, branch)
			if herr != nil {
				return false, fmt.Errorf("FactExistsAt: HeadCommit: %w", herr)
			}
			_, ok, werr := fq.rh.ResolveActiveCommitForPath(ctx, branch, path, head)
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
	_, ok, err := fq.rh.ResolveActiveCommitForPath(ctx, branch, path, commit)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// FactLiveAtCommit reports whether `path` is live (present, not retracted) as
// of `commit` — the delete-RESPECTING sibling of ResolveActiveCommitForPath.
//
// It walks the same first-parent ancestry but, crucially, does NOT filter the
// action: it takes the MOST RECENT commit_log event for the path in that
// ancestry and reports live iff that event is added/modified. A deletion that
// is more recent than the last add (i.e. the fact was retracted before
// `commit`) therefore reads as NOT live — matching the git tree state, which
// is what "the fact as of commit X" means.
//
// ResolveActiveCommitForPath answers a different question ("what is the last
// navigable version") by stepping OVER deletions; that is correct for ref
// classification and fallback-before content, but wrong as an existence gate.
//
// Returns false (not an error) when the path was never written in the
// ancestry of `commit`. Errors propagate.
func (fq *FactQuery) FactLiveAtCommit(ctx context.Context, branch, path, commit string) (bool, error) {
	fq.probe("FactLiveAtCommit")
	if err := fq.failRead("FactLiveAtCommit"); err != nil {
		return false, err
	}
	if commit == "" {
		return false, nil
	}

	var action string
	err := conn(ctx, fq.rh.DB()).QueryRowContext(ctx, vcs.FirstParentChainCTE+`
		SELECT cl.action
		  FROM fpc
		  JOIN commit_log cl ON cl.commit_hash = fpc.commit_hash
		 WHERE cl.path = ?
		 ORDER BY fpc.depth ASC
		 LIMIT 1
	`, commit, path).Scan(&action)
	if err == sql.ErrNoRows {
		return false, nil // path never written in this ancestry
	}
	if err != nil {
		return false, fmt.Errorf("FactLiveAtCommit: %w", err)
	}
	_ = branch // See SCHEMA INVARIANT above — branch is implicit.
	return action == "added" || action == "modified", nil
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
// resolved via vcs.Repo.ReadBlobHashAtCommit.
//
// Called AFTER the source Fact node's merge has been committed. The original
// reason was that direct-SQL reads could not see nodes MERGE'd through Cypher
// inside the same *sql.Tx, so node IDs only became visible post-commit. Node
// writes are direct SQL too now, so that constraint is gone and this could in
// principle move into the transaction — left as-is to keep the GraphQLite
// removal behaviour-neutral (see the matching note at the call site in
// search_crud.go). The tx parameter remains unused for the same reason; it is
// retained in the signature for symmetry with graphSyncFactTx and for the
// move-into-tx follow-up.
func (si *Index) graphAddDerivedFromAtCommitTx(
	ctx context.Context,
	tx storegit.CtxExecer,
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
			// The ref gate stops a NEWLY ADDED local ref from being written
			// unresolvable, but it deliberately exempts refs a fact already
			// carried — internal/corpus/refs does not re-judge the past. So this is
			// reachable through ordinary MCP writes too: a dedup merge grafts
			// the loser's carried refs onto the winner, and on a corpus that
			// predates the gate, or whose targets fell past the walk-back
			// horizon, those refs land here on every index sync. That is
			// expected, not a bypass — hence Debug, not Warn. A fact that
			// arrived another way (a direct git push to the KB branch, a
			// remote reconcile, a history rewrite) also lands here and is
			// indistinguishable at this point. Skipping the edge is still
			// correct; failing the index is not.
			log.Debug().Str("branch", branch).Str("source", sourcePath).
				Str("ref", refPath).Str("source_commit", sourceCommit).
				Msg("derived_from: local ref did not resolve; edge skipped")
			continue
		}

		targetBlobHash, err := si.rh.ReadBlobHashAtCommit(ctx, refPath, targetCommit)
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
func (si *Index) graphDerivedFromEdgeExists(ctx context.Context, srcID, tgtID int64, sourceCommit, targetCommit string) (bool, error) {
	var n int
	err := conn(ctx, si.rh.DB()).QueryRowContext(ctx, `
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
func (si *Index) graphNodeIDByBlob(ctx context.Context, path, blobHash string) (int64, error) {
	var nodeID int64
	err := conn(ctx, si.rh.DB()).QueryRowContext(ctx, `
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
