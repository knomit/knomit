package store

import (
	"context"
	"database/sql"
	"fmt"
)


// resolveTargetCommit walks ancestors of sourceCommit on the given branch
// looking for the FIRST commit_log row that touches refPath, and returns
// (commit_hash, ok=true) if that row's action is "added" or "modified".
// Returns ("", false, nil) if no ancestor touches refPath, or if the first
// ancestor that touches it has action="deleted" — both are skip-the-edge
// cases per design spec write-path step 2.
//
// The walk uses commit_log + branch_commits to constrain to the branch's
// ancestry; the result is the most recent commit on the branch with
// committed_at <= committed_at(sourceCommit) that touches refPath. This
// honours git topology by filtering through branch_commits, which is
// populated by walking parents from the branch ref.
func (si *searchIndex) resolveTargetCommit(ctx context.Context, branch, refPath, sourceCommit string) (string, bool, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return "", false, fmt.Errorf("resolveTargetCommit: branchID: %w", err)
	}

	var sourceCommittedAt int64
	err = conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT committed_at FROM commit_log WHERE commit_hash = ? LIMIT 1`,
		sourceCommit,
	).Scan(&sourceCommittedAt)
	if err == sql.ErrNoRows {
		// Source commit isn't in commit_log yet (caller passed an arg before
		// commit_log is populated, or the arg is invalid). No edges resolvable.
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolveTargetCommit: source committed_at: %w", err)
	}

	var targetCommit, action string
	err = conn(ctx, si.rh.db).QueryRowContext(ctx, `
		SELECT cl.commit_hash, cl.action
		FROM commit_log cl
		JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
		WHERE bc.branch_id = ?
		  AND cl.path = ?
		  AND cl.committed_at <= ?
		ORDER BY cl.committed_at DESC, cl.rowid DESC -- rowid is monotonic (no WITHOUT ROWID)
		LIMIT 1
	`, branchID, refPath, sourceCommittedAt).Scan(&targetCommit, &action)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolveTargetCommit: lookup: %w", err)
	}
	if action == "deleted" {
		return "", false, nil
	}
	return targetCommit, true, nil
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
		targetCommit, ok, err := si.resolveTargetCommit(ctx, branch, refPath, sourceCommit)
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
