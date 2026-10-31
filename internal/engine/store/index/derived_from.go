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

// graphAddDerivedFromAtCommitTx writes the DERIVED_FROM edges of the source
// BLOB, once: from Fact(sourcePath, sourceBlobHash) to Fact(refPath,
// target_blob_hash), with edge text properties source_commit and
// target_commit. Refs that resolve to "skip" via resolveTargetCommit
// (forward-broken or first ancestor is a deletion) are silently skipped. The
// branch parameter is the ingestion branch (the branch on which sourceCommit
// was committed); the resolution itself does not depend on it.
//
// The target Fact node already exists if resolveTargetCommit succeeds — it
// was upserted when refPath was originally indexed at target_commit. The
// target's blob_hash is the blob in the target_commit's tree for refPath,
// resolved via vcs.Repo.ReadBlobHashAtCommit.
//
// Called AFTER the source Fact node's merge has been committed. Every git
// read (the introducing commit, the claim's order, the targets) happens
// BEFORE the write transaction: an in-memory store has a one-connection pool,
// and the git object reads take a connection of their own. The claim and the
// edges are then written in ONE transaction (applyBlobRefs), so a blob is
// never claimed without its edges, and two indexers racing on one blob
// serialize on the write lock. The tx parameter is unused; it is retained in
// the signature for symmetry with graphSyncFactTx.
func (si *Index) graphAddDerivedFromAtCommitTx(
	ctx context.Context,
	tx storegit.CtxExecer,
	branch, sourcePath, sourceBlobHash, sourceCommit string,
	specs []refSpec,
) error {
	srcID, err := si.graphNodeIDByBlob(ctx, sourcePath, sourceBlobHash)
	if err != nil {
		return fmt.Errorf("graphAddDerivedFromAtCommitTx: source node id: %w", err)
	}
	if srcID == 0 {
		return fmt.Errorf("graphAddDerivedFromAtCommitTx: source Fact(%s,%s) not found", sourcePath, sourceBlobHash)
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
	// Reference edges are per BLOB and written ONCE (user ruling 2026-10-30):
	// a blob's refs are resolved at the commit that introduced it (the
	// earliest, in commitEarlier order), and the blob arriving anywhere else (a
	// COW hit: merge carrying it, another branch, an experiment, restore,
	// rename) adds nothing. Only a strictly EARLIER introduction, seen later
	// (an upstream synced after the agent branch), moves them, so a rebuild
	// and an incremental index agree.
	sourceCommit, err = si.introducingCommit(ctx, branch, sourcePath, sourceBlobHash, sourceCommit)
	if err != nil {
		return fmt.Errorf("graphAddDerivedFromAtCommitTx: %w", err)
	}
	if sourceCommit == "" {
		return nil // an arrival at a commit that does not hold the blob
	}

	// The ref_edges rows need the repo id to tell kb://<self>/ refs from
	// cross-KB ones (refSpecsOf). Unresolved (localRepoID retries on the next
	// call), the specs the caller built are wrong for such refs: write the
	// edges as before but leave the rows alone, so they are never rewritten
	// from wrong specs; the drift that leaves is repaired by the next rebuild
	// or revisit with the id (PR #694 review F1).
	rowsOK := si.localRepoID(ctx, branch) != ""

	var targets []refTarget
	resolved := false
	// The claim is decided outside the transaction (it may read git for the
	// order) and re-checked inside it: if another indexer changed the claim in
	// between, decide again.
	for attempt := 0; attempt < 5; attempt++ {
		seen, err := si.readBlobClaim(ctx, sourceBlobHash)
		if err != nil {
			return fmt.Errorf("graphAddDerivedFromAtCommitTx: %w", err)
		}
		write, err := si.claimBlobRefs(ctx, seen, srcID, sourceCommit)
		if err != nil {
			return fmt.Errorf("graphAddDerivedFromAtCommitTx: %w", err)
		}
		if !write {
			return nil
		}
		if !resolved {
			if targets, err = si.resolveRefTargets(ctx, branch, sourcePath, sourceCommit, localPaths(specs)); err != nil {
				return fmt.Errorf("graphAddDerivedFromAtCommitTx: %w", err)
			}
			resolved = true
		}
		applied, err := si.applyBlobRefs(ctx, seen, srcID, sourceBlobHash, sourceCommit, targets, specs, rowsOK)
		if err != nil {
			return fmt.Errorf("graphAddDerivedFromAtCommitTx: %w", err)
		}
		if applied {
			return nil
		}
	}
	return fmt.Errorf("graphAddDerivedFromAtCommitTx: the claim of blob %s kept changing", sourceBlobHash)
}

// refTarget is one resolved reference: the target version's node and the
// commit it was resolved at.
type refTarget struct {
	tgtID        int64
	targetCommit string
}

// resolveRefTargets resolves each ref at sourceCommit to the target's node.
// A ref that does not resolve, or whose target blob is not readable, or whose
// node is not indexed yet, is skipped (and retried by Sync's pass 2 or a
// rebuild: the claim allows the same node and commit again).
func (si *Index) resolveRefTargets(ctx context.Context, branch, sourcePath, sourceCommit string, refs []string) ([]refTarget, error) {
	var out []refTarget
	for _, refPath := range refs {
		targetCommit, ok, err := si.resolveTargetCommit(ctx, branch, sourcePath, refPath, sourceCommit)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", refPath, err)
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
			// The read loads the blob object, so THIS is the target half of
			// the real-blob invariant: a target whose blob is not in the
			// object store fails here (a separate existence check after it
			// could never fire). If the read ever stops loading the blob (a
			// tree-entry-only read), add a blobExists check on the target.
			// No edge for it, and the other refs still get theirs.
			si.danglingRefsSkipped.Add(1)
			log.Debug().Err(err).Str("source", sourcePath).Str("ref", refPath).Str("target_commit", targetCommit).
				Msg("derived_from: target blob unreadable; edge skipped")
			continue
		}
		tgtID, err := si.graphNodeIDByBlob(ctx, refPath, targetBlobHash)
		if err != nil {
			return nil, fmt.Errorf("target node id: %w", err)
		}
		if tgtID == 0 {
			// Target Fact node missing despite resolveTargetCommit succeeding.
			// Indicates an indexing race or stale state; skip rather than fail.
			log.Debug().Str("branch", branch).Str("ref", refPath).Str("target_commit", targetCommit).
				Msg("graphAddDerivedFromAtCommitTx: target Fact node not found, skipping (likely intra-commit ordering — Sync pass 2 will retry)")
			continue
		}
		out = append(out, refTarget{tgtID: tgtID, targetCommit: targetCommit})
	}
	return out, nil
}

// applyBlobRefs writes the claim, the edges and the blob's ref_edges rows in
// ONE transaction. It first re-reads the claim inside the transaction: when it
// is no longer the one the decision was made on (another indexer moved it),
// nothing is written and applied=false, and the caller decides again.
//
// The ref_edges rows (writeRefRowsTx) are derived, inside the same
// transaction, from the edges now on srcID and the blob's refs in the
// author's order (specs), and replace whatever rows the blob had: a claim
// move or a void claim re-claimed replaces the rows with the edges, and a
// fill-in (Sync's pass 2, a rebuild) adds the rows of the edges it adds.
//
// rowsOK=false (the repo id is unresolved, so specs may misclassify
// kb://<self>/ refs): the rows are neither checked nor written.
func (si *Index) applyBlobRefs(ctx context.Context, seen blobClaim, srcID int64, blobHash, sourceCommit string, targets []refTarget, specs []refSpec, rowsOK bool) (applied bool, err error) {
	// Nothing to write (the claim is already this node and commit, every
	// edge exists and the rows match them): no transaction, so a caller
	// re-visiting a resolved blob never takes the write lock.
	if seen.live() && seen.node == srcID && seen.at == sourceCommit {
		missing := false
		for _, t := range targets {
			exists, err := si.graphDerivedFromEdgeExists(ctx, srcID, t.tgtID, sourceCommit, t.targetCommit)
			if err != nil {
				return false, fmt.Errorf("dedup check: %w", err)
			}
			if !exists {
				missing = true
				break
			}
		}
		if !missing {
			if !rowsOK {
				return true, nil
			}
			current, err := refRowsCurrent(ctx, conn(ctx, si.rh.DB()), srcID, blobHash, specs)
			if err != nil {
				return false, err
			}
			if current {
				return true, nil
			}
		}
	}
	tctx, tx, own, err := beginTxIfNeeded(ctx, si.rh.DB())
	if err != nil {
		return false, fmt.Errorf("claim refs of blob %s: begin: %w", blobHash, err)
	}
	if own {
		defer func() {
			if err != nil || !applied {
				tx.Rollback() //nolint:errcheck
			}
		}()
	}
	now, err := si.readBlobClaim(tctx, blobHash)
	if err != nil {
		return false, err
	}
	if now != seen {
		return false, nil
	}
	db := conn(tctx, si.rh.DB())
	if !seen.live() || seen.node != srcID || seen.at != sourceCommit {
		// A claim being replaced takes its edges with it, in this
		// transaction: an earlier introduction moves them, and a VOID claim
		// (its commit on no branch) pinned history nobody reaches. Keeping
		// either would give the blob two edge sets, and outgoing reads key
		// by blob with no branch filter on source_commit.
		if seen.found {
			log.Debug().Str("blob", blobHash).Str("from", seen.at).Str("to", sourceCommit).Bool("void", seen.void).
				Msg("derived_from: the blob's claim is replaced; its refs are re-resolved")
			if err := graphDeleteOutgoingEdges(tctx, db, seen.node, EdgeDerivedFrom); err != nil {
				return false, err
			}
			if _, err := db.ExecContext(tctx, `DELETE FROM node_props_text WHERE node_id = ? AND key_id = (SELECT id FROM property_keys WHERE key = ?)`,
				seen.node, refsCommitKey); err != nil {
				return false, fmt.Errorf("claim refs of blob %s: clear: %w", blobHash, err)
			}
		}
		if err := graphSetNodeProps(tctx, db, srcID, map[string]string{refsCommitKey: sourceCommit}); err != nil {
			return false, fmt.Errorf("claim refs of blob %s: %w", blobHash, err)
		}
	}
	if si.beforeRefEdgesWrite != nil {
		if err := si.beforeRefEdgesWrite(); err != nil {
			return false, err
		}
	}
	for _, t := range targets {
		// Dedup guard: skip if this exact (src→tgt, source_commit,
		// target_commit) edge already exists (Sync's pass 2, or a rebuild,
		// filling in a claim that pass 1 wrote partly).
		exists, err := si.graphDerivedFromEdgeExists(tctx, srcID, t.tgtID, sourceCommit, t.targetCommit)
		if err != nil {
			return false, fmt.Errorf("dedup check: %w", err)
		}
		if exists {
			continue
		}
		edgeID, err := si.graphInsertEdgeReturningID(tctx, srcID, t.tgtID, EdgeDerivedFrom)
		if err != nil {
			return false, fmt.Errorf("insert edge: %w", err)
		}
		if err := si.graphSetEdgeProps(tctx, edgeID, map[string]string{
			"source_commit": sourceCommit,
			"target_commit": t.targetCommit,
		}); err != nil {
			return false, fmt.Errorf("set props: %w", err)
		}
	}
	if si.beforeRefRowsWrite != nil {
		if err := si.beforeRefRowsWrite(); err != nil {
			return false, err
		}
	}
	if rowsOK {
		if err := writeRefRowsTx(tctx, db, srcID, blobHash, specs); err != nil {
			return false, err
		}
	}
	if own {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("claim refs of blob %s: commit: %w", blobHash, err)
		}
	}
	return true, nil
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

// blobClaim is the refs_commit mark of a blob as read: the node carrying the
// blob's edges and the commit they were resolved at (found=false: none).
// void=true: the claim's commit is in no branch_commits row (an experiment
// rolled back, a rewound branch), so no branch reaches the history its edges
// describe; it counts as no claim, and the next arrival replaces it and its
// edges.
type blobClaim struct {
	node  int64
	at    string
	found bool
	void  bool
}

// live reports whether the claim stands: present and reachable from a branch.
func (c blobClaim) live() bool { return c.found && !c.void }

// readBlobClaim reads the blob's refs_commit mark and whether its commit is
// still reachable (SQL only, so it can run inside the write transaction).
func (si *Index) readBlobClaim(ctx context.Context, blobHash string) (blobClaim, error) {
	var c blobClaim
	err := conn(ctx, si.rh.DB()).QueryRowContext(ctx, `
		SELECT nb.node_id, rc.value
		  FROM node_props_text nb
		  JOIN node_props_text rc ON rc.node_id = nb.node_id
		       AND rc.key_id = (SELECT id FROM property_keys WHERE key = ?)
		 WHERE nb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash') AND nb.value = ?
		 ORDER BY nb.node_id
		 LIMIT 1`, refsCommitKey, blobHash).Scan(&c.node, &c.at)
	switch {
	case err == sql.ErrNoRows:
		return blobClaim{}, nil
	case err != nil:
		return blobClaim{}, fmt.Errorf("claim refs of blob %s: %w", blobHash, err)
	}
	c.found = true
	var one int
	err = conn(ctx, si.rh.DB()).QueryRowContext(ctx,
		`SELECT 1 FROM branch_commits WHERE commit_hash = ? LIMIT 1`, c.at).Scan(&one)
	switch {
	case err == sql.ErrNoRows:
		c.void = true
	case err != nil:
		return blobClaim{}, fmt.Errorf("claim refs of blob %s: reachability: %w", blobHash, err)
	}
	return c, nil
}

// claimBlobRefs decides, from the claim as read, whether this call writes the
// blob's reference edges:
//   - the blob has no resolution yet, or its claim is VOID (its commit is no
//     longer on any branch): it is claimed for (srcID, sourceCommit), and
//     applyBlobRefs drops the void claim's edges in the same transaction;
//   - it was resolved at this same node and commit (Sync's pass 2 retrying an
//     intra-batch ref, a rebuild): the call fills in what is missing;
//   - it was resolved at a commit that sourceCommit strictly precedes in
//     introduction order (commitEarlier): the earlier introduction wins, and
//     applyBlobRefs moves the edges here;
//   - otherwise (any other arrival of a resolved blob): nothing is written.
//
// It may read git (the order), so it runs OUTSIDE the write transaction;
// applyBlobRefs re-checks that the claim did not change before writing.
func (si *Index) claimBlobRefs(ctx context.Context, seen blobClaim, srcID int64, sourceCommit string) (bool, error) {
	switch {
	case !seen.live():
		return true, nil
	case seen.node == srcID && seen.at == sourceCommit:
		return true, nil
	}
	return si.commitEarlier(ctx, sourceCommit, seen.at)
}

// blobRefsResetKey is the meta sentinel of the one-time move to per-blob
// reference edges (GraphSchemaVersion 8).
const blobRefsResetKey = "derived_from_per_blob"

// blobRefsPendingPrefix prefixes the meta key of a branch whose reference
// edges the one-time reset dropped and that has not been re-resolved yet
// (meta "derived_from_per_blob_pending:<branch>").
const blobRefsPendingPrefix = "derived_from_per_blob_pending:"

// resetBlobRefsOnce drops every DERIVED_FROM edge and every refs_commit mark
// ONCE per store, inside the rebuild's transaction, and marks EVERY row of
// `branches` (open experiments included) pending, in the same transaction.
// The rebuild re-resolves its own branch in phase B and then every pending
// branch (reResolvePendingBranches), clearing each mark after its branch is
// done. The edges of the old scheme cannot be kept: a blob's edges were the
// union of every commit it arrived at, and which of them is its first is not
// recorded.
//
// Crash safety: the reset, the sentinel and the marks commit together; a
// mark is cleared only after its branch was re-resolved, and the rebuilt
// branch's schema version is bumped only after every mark is drained. An
// interrupted upgrade leaves marks behind and a branch whose version was not
// bumped, so the next boot rebuilds it, and that rebuild drains the marks
// again (any rebuild drains whatever marks it finds).
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
	// ref_edges mirrors the edges: it goes with them, and the re-resolve of
	// every branch writes both again.
	if _, err := tx.ExecContext(ctx, `DELETE FROM ref_edges`); err != nil {
		return fmt.Errorf("reset reference edges: ref_edges: %w", err)
	}
	// The drain re-creates rows only for blobs on a branch; a version on no
	// branch would lose its cross-KB rows for good. Clearing the backfill
	// sentinel makes the next backfill (the heal's, right after its
	// rebuilds) scan for them again (PR #694 review N4), and outgoing reads
	// fall back to the edges until it has run.
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, refEdgesBackfillKey); err != nil {
		return fmt.Errorf("reset reference edges: backfill sentinel: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) SELECT ? || name, '1' FROM branches`, blobRefsPendingPrefix); err != nil {
		return fmt.Errorf("reset reference edges: pending branches: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, '1')`, blobRefsResetKey); err != nil {
		return fmt.Errorf("reset reference edges: sentinel: %w", err)
	}
	return nil
}

// pendingRefBranches lists the branches still marked pending by the reset.
func (si *Index) pendingRefBranches(ctx context.Context) ([]string, error) {
	rows, err := conn(ctx, si.rh.DB()).QueryContext(ctx,
		`SELECT substr(key, ?) FROM meta WHERE key LIKE ? ORDER BY key`,
		len(blobRefsPendingPrefix)+1, blobRefsPendingPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("pending reference branches: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("pending reference branches: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// clearPendingRefBranch removes a branch's pending mark.
func (si *Index) clearPendingRefBranch(ctx context.Context, branch string) error {
	if _, err := conn(ctx, si.rh.DB()).ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, blobRefsPendingPrefix+branch); err != nil {
		return fmt.Errorf("clear pending reference branch %s: %w", branch, err)
	}
	return nil
}

// introducingCommit is the commit a blob's refs are resolved at.
//
// Callers pass the version's source commit, which the indexer takes from
// LastCommitForPath (the most recent NON-merge commit that touched the file;
// it can misattribute on interleaved histories), or a rebuild's commit_log
// event, so it may not hold the blob, or may not be its earliest
// introduction. The answer:
//   - the EARLIEST, in commitEarlier order, of the blob's introductions at
//     path (path_changes entries reachable from an indexed branch) and the
//     passed commit when it holds the blob. This holds whether or not the
//     blob is claimed: for a claimed blob, claimBlobRefs then compares it
//     with the claim, so an earlier introduction the claim missed moves it.
//     A merge that carried the blob, or a tip that holds it, is never
//     preferred over the commit that wrote it;
//   - none of those, and the blob has a LIVE claim: "" (write nothing);
//   - none of those, and no live claim (none, or a void one: its commit is on
//     no branch any more): the version live at the branch tip, or the tip
//     itself, when it holds the blob (a merge commit being indexed before
//     branch_commits names it).
//
// Never a commit that does not hold the blob at path: when none is found the
// answer is "" and no edge is written.
func (si *Index) introducingCommit(ctx context.Context, branch, path, blobHash, commit string) (string, error) {
	holds := func(c string) bool {
		b, err := si.rh.ReadBlobHashAtCommit(ctx, path, c)
		return err == nil && b == blobHash
	}
	passedHolds := commit != "" && holds(commit)
	// A passed commit no branch reaches (an experiment rolled back while a
	// drain walked it) competes only as the last resort below: a claim there
	// would be void at once.
	passedReachable := passedHolds && si.commitReachable(ctx, commit)
	best, err := si.earliestIntroduction(ctx, path, blobHash, holds)
	if err != nil {
		return "", err
	}
	if passedReachable {
		if best == "" {
			return commit, nil
		}
		earlier, err := si.commitEarlier(ctx, commit, best)
		if err != nil {
			return "", err
		}
		if earlier {
			return commit, nil
		}
	}
	if best != "" {
		return best, nil
	}
	if si.blobRefsClaimed(ctx, blobHash) {
		return "", nil
	}
	head, err := si.rh.HeadCommit(ctx, branch)
	if err != nil {
		return "", nil // the branch is gone: nothing reaches what it held
	}
	if c, ok, err := si.rh.ResolveActiveCommitForPath(ctx, branch, path, head); err == nil && ok && holds(c) {
		return c, nil
	}
	// commit_log may not record the merge yet when the index syncs it: the
	// tip itself, when it holds the blob, is the commit that introduced it.
	if holds(head) {
		return head, nil
	}
	if passedHolds {
		return commit, nil
	}
	return "", nil
}

// commitReachable reports whether some branch reaches the commit (a
// branch_commits row names it).
func (si *Index) commitReachable(ctx context.Context, commit string) bool {
	var one int
	err := conn(ctx, si.rh.DB()).QueryRowContext(ctx,
		`SELECT 1 FROM branch_commits WHERE commit_hash = ? LIMIT 1`, commit).Scan(&one)
	return err == nil
}

// earliestIntroduction is the earliest commit, in commitEarlier order, that
// introduced blobHash at path (a path_changes entry: the blob differs from
// every parent) and that some indexed branch reaches, or "" when none is
// recorded. Each candidate is checked to really hold the blob.
func (si *Index) earliestIntroduction(ctx context.Context, path, blobHash string, holds func(string) bool) (string, error) {
	rows, err := conn(ctx, si.rh.DB()).QueryContext(ctx, `
		SELECT pc.commit_hash
		  FROM path_changes pc
		 WHERE pc.path = ? AND pc.blob = ? AND pc.entry = 1
		   AND EXISTS (SELECT 1 FROM branch_commits bc WHERE bc.commit_hash = pc.commit_hash)
		 ORDER BY pc.commit_hash`, path, blobHash)
	if err != nil {
		return "", fmt.Errorf("introductions of %s at %s: %w", blobHash, path, err)
	}
	var cands []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return "", fmt.Errorf("introductions of %s at %s: %w", blobHash, path, err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("introductions of %s at %s: %w", blobHash, path, err)
	}
	best := ""
	for _, c := range cands {
		if !holds(c) {
			continue
		}
		if best == "" {
			best = c
			continue
		}
		earlier, err := si.commitEarlier(ctx, c, best)
		if err != nil {
			return "", err
		}
		if earlier {
			best = c
		}
	}
	return best, nil
}

// blobRefsClaimed reports whether some Fact node of blobHash carries a LIVE
// refs_commit mark (a void one, at a commit no branch reaches, does not
// count).
func (si *Index) blobRefsClaimed(ctx context.Context, blobHash string) bool {
	c, err := si.readBlobClaim(ctx, blobHash)
	return err == nil && c.live()
}

// commitEarlier reports whether commit a precedes commit b in the order of a
// blob's introductions. The order is TOTAL: each commit gets the key
// (committed_at, same-second ancestors, hash), compared lexicographically:
//   - committed_at first (commit_log's, else the git committer time), so with
//     clock skew a descendant can come out earlier than its ancestor;
//   - within one second, the number of ancestors committed in that same
//     second (walking parents while committed_at >= that second). An
//     ancestor of the same second has strictly fewer, so it comes first;
//   - then the hash.
//
// A per-commit key makes the order transitive, so the claim converges to the
// same minimum whatever order the introductions are seen in: a rebuild (events
// by committed_at) and an incremental index agree. (A pairwise "ancestor,
// else hash" tie-break is not transitive: A anc B, B < C and C < A by hash,
// with C unrelated, is a cycle.) A commit whose time cannot be read sorts
// last.
func (si *Index) commitEarlier(ctx context.Context, a, b string) (bool, error) {
	if a == b {
		return false, nil
	}
	ta, okA, err := si.commitTime(ctx, a)
	if err != nil {
		return false, err
	}
	tb, okB, err := si.commitTime(ctx, b)
	if err != nil {
		return false, err
	}
	switch {
	case okA != okB:
		return okA, nil
	case !okA:
		return a < b, nil
	case ta != tb:
		return ta < tb, nil
	}
	na, err := si.sameSecondAncestors(ctx, a, ta)
	if err != nil {
		return false, err
	}
	nb, err := si.sameSecondAncestors(ctx, b, tb)
	if err != nil {
		return false, err
	}
	if na != nb {
		return na < nb, nil
	}
	return a < b, nil
}

// commitTime is a commit's committed_at: commit_log's (what a rebuild orders
// by), else the git committer time (a commit being indexed is not in
// commit_log yet). ok=false when neither is readable.
func (si *Index) commitTime(ctx context.Context, h string) (int64, bool, error) {
	var t int64
	err := conn(ctx, si.rh.DB()).QueryRowContext(ctx,
		`SELECT committed_at FROM commit_log WHERE commit_hash = ? LIMIT 1`, h).Scan(&t)
	if err == nil {
		return t, true, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, fmt.Errorf("commit time of %s: %w", h, err)
	}
	_, gt, gerr := si.rh.CommitParentsAndTime(h)
	if gerr != nil {
		return 0, false, nil
	}
	return gt, true, nil
}

// sameSecondAncestors counts the strict ancestors of h whose committed_at is
// t, walking parents through commits with committed_at >= t (the walk stops
// at older commits, so it is bounded by the commits of that second, plus
// whatever clock skew puts above it).
func (si *Index) sameSecondAncestors(ctx context.Context, h string, t int64) (int, error) {
	seen := map[string]struct{}{h: {}}
	stack := []string{h}
	n := 0
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		parents, _, err := si.rh.CommitParentsAndTime(c)
		if err != nil {
			continue // a missing commit object ends the walk on that side
		}
		for _, p := range parents {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			pt, ok, err := si.commitTime(ctx, p)
			if err != nil {
				return 0, err
			}
			if !ok || pt < t {
				continue
			}
			if pt == t {
				n++
			}
			stack = append(stack, p)
		}
	}
	return n, nil
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
