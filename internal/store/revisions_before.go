package store

import (
	"context"
	"database/sql"
	"fmt"
)

// FirstParentChainCTE is the recursive first-parent ancestry walk shared by the
// sparse-history resolvers (RevisionsBefore, ResolveActiveCommitForPath). It
// binds one parameter — the start commit — and yields (commit_hash, depth) rows
// newest → oldest along parent_order = 0. Callers append their own
// `SELECT … FROM fpc JOIN …`. First-parent (not wall-clock) ancestry is the
// load-bearing semantic — see resolveTargetCommit's doc-comment for the
// merge-branch rationale.
const FirstParentChainCTE = `
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

// ResolveActiveCommitForPath walks first-parent ancestry of fromCommit on
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
// from the branch tip (PopulateCommitLog uses gogit.LogOrderDefault), so
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
// ResolveActiveCommitForPath lives on repoHandler (it only needs rh.db) so its
// callers — factIndex, searchIndex, factQuery and graphStore — share it directly —
// factIndex's fallback-before read resolves the last-valid version through the
// SAME index walk the graph resolver uses, instead of a divergent go-git walk.
func (rh *repoHandler) ResolveActiveCommitForPath(ctx context.Context, branch, path, fromCommit string) (string, bool, error) {
	if fromCommit == "" {
		return "", false, nil
	}

	var hash string
	err := conn(ctx, rh.db).QueryRowContext(ctx, FirstParentChainCTE+`
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
