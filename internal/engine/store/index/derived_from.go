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
	if err := fq.failRead(ReadFactLiveAtCommit); err != nil {
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
	// Reference edges are per BLOB and written ONCE (user ruling 2026-10-30):
	// a blob's refs are resolved at the commit that introduced it, and the
	// blob arriving anywhere else (a COW hit: merge carrying it, another
	// branch, an experiment, restore, rename) adds nothing. Only a strictly
	// EARLIER introduction, seen later (an upstream synced after the agent
	// branch), moves them, so a rebuild (oldest first) and an incremental
	// index agree.
	sourceCommit, err = si.introducingCommit(ctx, branch, sourcePath, sourceBlobHash, sourceCommit)
	if err != nil {
		return fmt.Errorf("graphAddDerivedFromAtCommitTx: %w", err)
	}
	if sourceCommit == "" {
		return nil // an arrival at a commit that does not hold the blob
	}
	// A reference edge joins two REAL blobs: the source's and the target's
	// must both be in this store's object table, or there is no edge
	// (user ruling 2026-10-30; Verify reports a violation, the rebuild drops
	// it).
	if !si.blobExists(ctx, sourceBlobHash) {
		si.danglingRefsSkipped.Add(1)
		log.Debug().Str("source", sourcePath).Str("blob", sourceBlobHash).
			Msg("derived_from: source blob not in the object store; no reference edges")
		return nil
	}
	write, err := si.claimBlobRefs(ctx, srcID, sourcePath, sourceBlobHash, sourceCommit)
	if err != nil {
		return fmt.Errorf("graphAddDerivedFromAtCommitTx: %w", err)
	}
	if !write {
		return nil
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
			// The read loads the blob object: a target whose blob is not in
			// the object store fails here. No edge for it, and the other refs
			// still get theirs (it used to abort the rest of the list). The
			// claim allows the same node and commit again, so Sync's pass 2
			// or a rebuild retries a transient failure.
			si.danglingRefsSkipped.Add(1)
			log.Debug().Err(err).Str("source", sourcePath).Str("ref", refPath).Str("target_commit", targetCommit).
				Msg("derived_from: target blob unreadable; edge skipped")
			continue
		}
		if !si.blobExists(ctx, targetBlobHash) {
			si.danglingRefsSkipped.Add(1)
			log.Debug().Str("source", sourcePath).Str("ref", refPath).Str("blob", targetBlobHash).
				Msg("derived_from: target blob not in the object store; edge skipped")
			continue
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

// blobExists reports whether the blob is in this store's object table.
func (si *Index) blobExists(ctx context.Context, blobHash string) bool {
	if blobHash == "" {
		return false
	}
	var one int
	err := conn(ctx, si.rh.DB()).QueryRowContext(ctx,
		`SELECT 1 FROM objects WHERE hash = ? AND type = ?`, blobHash, vcs.BlobObjectType).Scan(&one)
	return err == nil
}

// DanglingRefsSkipped is how many reference edges this index declined to
// write because a source or target blob was not in the object store.
func (si *Index) DanglingRefsSkipped() int64 { return si.danglingRefsSkipped.Load() }

// danglingRefEdgesSQL selects the DERIVED_FROM edges whose source or target
// Fact node names a blob that is not in the object table.
const danglingRefEdgesSQL = `
	SELECT e.id
	  FROM edges e
	  JOIN node_props_text sb ON sb.node_id = e.source_id
	       AND sb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash')
	  JOIN node_props_text tb ON tb.node_id = e.target_id
	       AND tb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash')
	 WHERE e.type = 'DERIVED_FROM'
	   AND (NOT EXISTS (SELECT 1 FROM objects o WHERE o.hash = sb.value AND o.type = 3)
	     OR NOT EXISTS (SELECT 1 FROM objects o WHERE o.hash = tb.value AND o.type = 3))`

// dropDanglingRefEdges deletes every reference edge whose source or target
// blob is not in the object store (rebuild runs it in its transaction).
func dropDanglingRefEdges(ctx context.Context, tx storegit.CtxExecer) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE id IN (`+danglingRefEdgesSQL+`)`)
	if err != nil {
		return 0, fmt.Errorf("drop dangling reference edges: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DanglingRefEdges counts the reference edges whose source or target blob is
// not in the object store: Verify's check.
func (si *Index) DanglingRefEdges(ctx context.Context) (int64, error) {
	var n int64
	err := conn(ctx, si.rh.DB()).QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+danglingRefEdgesSQL+`)`).Scan(&n)
	return n, err
}

// refsCommitKey is the Fact node property that records the commit a blob's
// refs were resolved at. It is set on the ONE node of the blob that carries
// its DERIVED_FROM edges, even when no ref resolved, so a later arrival of the
// same blob finds the blob already resolved and writes nothing.
const refsCommitKey = "refs_commit"

// claimBlobRefs decides whether this call writes the blob's reference edges:
//   - the blob has no resolution yet: it is claimed for (srcID, sourceCommit);
//   - it was resolved at this same node and commit (Sync's pass 2 retrying an
//     intra-batch ref): the call fills in what pass 1 could not;
//   - it was resolved at a commit that is strictly LATER by (committed_at,
//     hash) than sourceCommit: the earlier introduction wins; the old edges
//     are deleted and the blob is re-claimed here;
//   - otherwise (any other arrival of a resolved blob): nothing is written.
func (si *Index) claimBlobRefs(ctx context.Context, srcID int64, sourcePath, blobHash, sourceCommit string) (bool, error) {
	db := conn(ctx, si.rh.DB())
	var node int64
	var at string
	err := db.QueryRowContext(ctx, `
		SELECT nb.node_id, rc.value
		  FROM node_props_text nb
		  JOIN node_props_text rc ON rc.node_id = nb.node_id
		       AND rc.key_id = (SELECT id FROM property_keys WHERE key = ?)
		 WHERE nb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash') AND nb.value = ?
		 LIMIT 1`, refsCommitKey, blobHash).Scan(&node, &at)
	switch {
	case err == sql.ErrNoRows:
		return true, graphSetNodeProps(ctx, db, srcID, map[string]string{refsCommitKey: sourceCommit})
	case err != nil:
		return false, fmt.Errorf("claim refs of blob %s: %w", blobHash, err)
	case node == srcID && at == sourceCommit:
		return true, nil
	}
	earlier, err := si.commitEarlier(ctx, sourceCommit, at)
	if err != nil {
		return false, err
	}
	if !earlier {
		return false, nil
	}
	log.Debug().Str("blob", blobHash).Str("from", at).Str("to", sourceCommit).
		Msg("derived_from: an earlier introduction of the blob re-resolves its refs")
	if err := graphDeleteOutgoingEdges(ctx, db, node, EdgeDerivedFrom); err != nil {
		return false, err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM node_props_text WHERE node_id = ? AND key_id = (SELECT id FROM property_keys WHERE key = ?)`,
		node, refsCommitKey); err != nil {
		return false, fmt.Errorf("claim refs of blob %s: clear: %w", blobHash, err)
	}
	return true, graphSetNodeProps(ctx, db, srcID, map[string]string{refsCommitKey: sourceCommit})
}

// blobRefsResetKey is the meta sentinel of the one-time move to per-blob
// reference edges (GraphSchemaVersion 8).
const blobRefsResetKey = "derived_from_per_blob"

// resetBlobRefsOnce drops every DERIVED_FROM edge and every refs_commit mark
// ONCE per store, inside the rebuild's transaction, so the first branch
// rebuilt after the upgrade re-resolves every blob it sees from scratch and
// later branches only add blobs (or earlier introductions) of their own. The
// edges of the old scheme cannot be kept: a blob's edges were the union of
// every commit it arrived at, and which of them is its first is not recorded.
func resetBlobRefsOnce(ctx context.Context, tx storegit.CtxExecer) error {
	var done string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, blobRefsResetKey).Scan(&done)
	if err == nil && done == "1" {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("reset reference edges: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE type = ?`, EdgeDerivedFrom); err != nil {
		return fmt.Errorf("reset reference edges: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_props_text WHERE key_id = (SELECT id FROM property_keys WHERE key = ?)`, refsCommitKey); err != nil {
		return fmt.Errorf("reset reference edges: marks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, '1')`, blobRefsResetKey); err != nil {
		return fmt.Errorf("reset reference edges: sentinel: %w", err)
	}
	return nil
}

// introducingCommit is the commit a NEW blob's refs are resolved at. Callers
// pass the version's source commit, which the indexer takes from
// LastCommitForPath: the most recent NON-merge commit that touched the file.
// For a blob a merge commit created (a conflict resolved in the merge) that
// commit does not hold the blob, and the merge does: when the passed commit
// does not hold the blob, the version live at the branch tip (first-parent,
// merges included) is used if it does. Only a blob not yet resolved pays for
// the check; any other arrival writes nothing anyway.
//
// The passed commit is used only when it holds the blob. For a blob already
// resolved, an arrival at a commit that does not hold it cannot be an earlier
// introduction: "" (write nothing).
func (si *Index) introducingCommit(ctx context.Context, branch, path, blobHash, commit string) (string, error) {
	if b, err := si.rh.ReadBlobHashAtCommit(ctx, path, commit); err == nil && b == blobHash {
		return commit, nil
	}
	if si.blobRefsClaimed(ctx, blobHash) {
		return "", nil
	}
	head, err := si.rh.HeadCommit(ctx, branch)
	if err != nil {
		return commit, nil
	}
	if c, ok, err := si.rh.ResolveActiveCommitForPath(ctx, branch, path, head); err == nil && ok {
		if b, err := si.rh.ReadBlobHashAtCommit(ctx, path, c); err == nil && b == blobHash {
			return c, nil
		}
	}
	// commit_log may not record the merge yet when the index syncs it: the
	// tip itself, when it holds the blob, is the commit that introduced it.
	if b, err := si.rh.ReadBlobHashAtCommit(ctx, path, head); err == nil && b == blobHash {
		return head, nil
	}
	return commit, nil
}

// blobRefsClaimed reports whether some Fact node of blobHash carries the
// refs_commit mark.
func (si *Index) blobRefsClaimed(ctx context.Context, blobHash string) bool {
	var one int
	err := conn(ctx, si.rh.DB()).QueryRowContext(ctx, `
		SELECT 1
		  FROM node_props_text nb
		  JOIN node_props_text rc ON rc.node_id = nb.node_id
		       AND rc.key_id = (SELECT id FROM property_keys WHERE key = ?)
		 WHERE nb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash') AND nb.value = ?
		 LIMIT 1`, refsCommitKey, blobHash).Scan(&one)
	return err == nil
}

// commitEarlier reports whether commit a precedes commit b, the order of a
// blob's introductions: an ancestor precedes its descendant; otherwise by
// committed_at, and two unrelated commits of the same second by hash. A
// commit commit_log does not know sorts last (it is checked for ancestry
// first: a merge being indexed is not in commit_log yet).
// Rebuild walks events by (committed_at, rowid) and an incremental index
// sees them as they come; this order makes both converge to the same claim.
func (si *Index) commitEarlier(ctx context.Context, a, b string) (bool, error) {
	when := func(h string) (int64, bool, error) {
		var t int64
		err := conn(ctx, si.rh.DB()).QueryRowContext(ctx,
			`SELECT committed_at FROM commit_log WHERE commit_hash = ? LIMIT 1`, h).Scan(&t)
		if err == sql.ErrNoRows {
			return 0, false, nil
		}
		return t, err == nil, err
	}
	ta, okA, err := when(a)
	if err != nil {
		return false, fmt.Errorf("commit time of %s: %w", a, err)
	}
	tb, okB, err := when(b)
	if err != nil {
		return false, fmt.Errorf("commit time of %s: %w", b, err)
	}
	switch {
	case !okA:
		return false, nil
	case !okB:
		return true, nil
	case ta != tb:
		return ta < tb, nil
	case a == b:
		return false, nil
	}
	if anc, err := si.isAncestor(ctx, a, b); err != nil || anc {
		return anc, err
	}
	if anc, err := si.isAncestor(ctx, b, a); err != nil || anc {
		return false, err
	}
	return a < b, nil
}

// isAncestor reports whether commit a is an ancestor of commit b, from the
// git objects (commit_parents is not written yet for a commit being indexed).
func (si *Index) isAncestor(_ context.Context, a, b string) (bool, error) {
	return si.rh.CommitIsAncestor(a, b)
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
