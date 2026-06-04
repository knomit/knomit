package store

import (
	"context"
	"fmt"
)

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
// Walks first-parent ancestry via the same recursive CTE as
// resolveActiveCommitForPath — NEVER wall-clock ordering. See that method's
// doc-comment for the merge-branch rationale, and the branch-commits
// reachability invariant for why the commit_log JOIN keeps the walk on-branch.
func (si *searchIndex) RevisionsBefore(ctx context.Context, branch, path, anchorCommit string, limit int) ([]RevisionMeta, error) {
	if anchorCommit == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, `
		WITH RECURSIVE fpc(commit_hash, depth) AS (
		    SELECT ?, 0
		    UNION ALL
		    SELECT cp.parent_hash, fpc.depth + 1
		      FROM commit_parents cp
		      JOIN fpc ON cp.commit_hash = fpc.commit_hash AND cp.parent_order = 0
		)
		SELECT cl.commit_hash, COALESCE(cl.committed_at, 0), cl.message, cl.action
		  FROM fpc
		  JOIN commit_log cl ON cl.commit_hash = fpc.commit_hash
		 WHERE cl.path = ? AND cl.action IN ('added','modified')
		 ORDER BY fpc.depth ASC
		 LIMIT ?
	`, anchorCommit, path, limit)
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
	_ = branch // first-parent ancestry is branch-implicit; see invariant note above.
	return out, nil
}
