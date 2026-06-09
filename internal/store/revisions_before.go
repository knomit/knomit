package store

import (
	"context"
	"fmt"
)

// firstParentChainCTE is the recursive first-parent ancestry walk shared by the
// sparse-history resolvers (RevisionsBefore, resolveActiveCommitForPath). It
// binds one parameter — the start commit — and yields (commit_hash, depth) rows
// newest → oldest along parent_order = 0. Callers append their own
// `SELECT … FROM fpc JOIN …`. First-parent (not wall-clock) ancestry is the
// load-bearing semantic — see resolveTargetCommit's doc-comment for the
// merge-branch rationale.
const firstParentChainCTE = `
	WITH RECURSIVE fpc(commit_hash, depth) AS (
	    SELECT ?, 0
	    UNION ALL
	    SELECT cp.parent_hash, fpc.depth + 1
	      FROM commit_parents cp
	      JOIN fpc ON cp.commit_hash = fpc.commit_hash AND cp.parent_order = 0
	)`

// RevisionMeta is one revision of a fact path in its commit history.
type RevisionMeta struct {
	Commit      string
	CommittedAt int64  // Unix seconds
	Message     string // first line of the commit message
	Action      string // "added" | "modified"
}

// RevisionsBefore returns up to `limit` revisions of `path` in the first-parent
// ancestry of `anchorCommit`, newest → oldest, restricted to commits where
// `path` was added or modified. Deletions are stepped over — they are write
// events in the sparse history, not stop conditions.
//
// History is bounded to the ancestry of `anchorCommit`: revisions committed
// after the anchor are never surfaced ("explain as of C" rewinds the timeline).
//
// Walks first-parent ancestry via the shared firstParentChainCTE — NEVER
// wall-clock ordering. See resolveTargetCommit's doc-comment for the
// merge-branch rationale. The walk is scoped to `branch` by joining
// branch_commits on the resolved branch_id, so revisions reachable only via an
// off-branch lineage never surface even when anchorCommit was committed
// elsewhere.
func (si *searchIndex) RevisionsBefore(ctx context.Context, branch, path, anchorCommit string, limit int) ([]RevisionMeta, error) {
	if anchorCommit == "" || limit <= 0 {
		return nil, nil
	}
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("RevisionsBefore: branchID: %w", err)
	}
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, firstParentChainCTE+`
		SELECT cl.commit_hash, COALESCE(cl.committed_at, 0), cl.message, cl.action
		  FROM fpc
		  JOIN commit_log cl ON cl.commit_hash = fpc.commit_hash
		  JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
		 WHERE bc.branch_id = ? AND cl.path = ? AND cl.action IN ('added','modified')
		 ORDER BY fpc.depth ASC
		 LIMIT ?
	`, anchorCommit, branchID, path, limit)
	if err != nil {
		return nil, fmt.Errorf("RevisionsBefore: %w", err)
	}
	defer rows.Close()

	var out []RevisionMeta
	for rows.Next() {
		var rm RevisionMeta
		var msg string
		if err := rows.Scan(&rm.Commit, &rm.CommittedAt, &msg, &rm.Action); err != nil {
			return nil, fmt.Errorf("RevisionsBefore: scan: %w", err)
		}
		rm.Message = firstLine(msg)
		out = append(out, rm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("RevisionsBefore: rows: %w", err)
	}
	return out, nil
}
