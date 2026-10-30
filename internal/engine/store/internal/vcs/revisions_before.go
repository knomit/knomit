package vcs

import (
	"context"
	"database/sql"
	"fmt"

	"knomit/internal/engine/store/internal/sqlite"
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
// ResolveActiveCommitForPath lives on Repo (it only needs rh.db) so its
// callers — facts.Index, index.Index, index.FactQuery and index.GraphStore — share it directly —
// facts.Index's fallback-before read resolves the last-valid version through the
// SAME index walk the graph resolver uses, instead of a divergent go-git walk.
//
// EARLY STOP (round 4 of PR #692): the recursion carries whether its commit
// wrote path (one commit_log primary-key lookup per step) and expands no
// further from a commit that did, so it ends at the FIRST such commit, the
// one of smallest depth, which is exactly what the chain walk with ORDER BY
// depth LIMIT 1 answers. That walk joined the WHOLE first-parent chain
// before ordering (the ORDER BY defeats the pull-based LIMIT), so every call
// was O(history): measured 0.4 ms each on knomit-kb, 40% of a FactSet's
// first page, and quadratic in history for a store writing facts with refs
// (every edge's target is resolved here). Now a call costs O(depth to the
// commit that wrote path).
func (rh *Repo) ResolveActiveCommitForPath(ctx context.Context, branch, path, fromCommit string) (string, bool, error) {
	if fromCommit == "" {
		return "", false, nil
	}
	var hash string
	err := sqlite.Conn(ctx, rh.db).QueryRowContext(ctx, `
		WITH RECURSIVE fpc(commit_hash, wrote) AS (
		    SELECT ?1, EXISTS (SELECT 1 FROM commit_log
		                        WHERE commit_hash = ?1 AND path = ?2 AND action IN ('added','modified'))
		    UNION ALL
		    SELECT cp.parent_hash, EXISTS (SELECT 1 FROM commit_log
		                                    WHERE commit_hash = cp.parent_hash AND path = ?2 AND action IN ('added','modified'))
		      FROM fpc
		      JOIN commit_parents cp ON cp.commit_hash = fpc.commit_hash AND cp.parent_order = 0
		     WHERE fpc.wrote = 0
		)
		SELECT commit_hash FROM fpc WHERE wrote = 1 LIMIT 1
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

// ResolveActiveCommitForPathChainForTest is ResolveActiveCommitForPath
// without the depth-0 fast path (the chain walk alone), so a test can prove
// the two answer alike. Never called in production.
func (rh *Repo) ResolveActiveCommitForPathChainForTest(ctx context.Context, branch, path, fromCommit string) (string, bool, error) {
	if fromCommit == "" {
		return "", false, nil
	}
	return rh.resolveActiveCommitChain(ctx, branch, path, fromCommit)
}

// resolveActiveCommitChain is the first-parent chain walk of
// ResolveActiveCommitForPath.
func (rh *Repo) resolveActiveCommitChain(ctx context.Context, branch, path, fromCommit string) (string, bool, error) {
	var hash string
	err := sqlite.Conn(ctx, rh.db).QueryRowContext(ctx, FirstParentChainCTE+`
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
