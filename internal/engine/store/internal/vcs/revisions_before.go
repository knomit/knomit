package vcs

import (
	"context"
	"database/sql"
	"fmt"

	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/internal/sqlite"
)

// FirstParentChainCTE is the recursive first-parent ancestry walk shared by the
// sparse-history resolvers (RevisionsBefore, FactLiveAtCommit). It
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

// ResolveActiveCommitForPath returns the most recent commit on fromCommit's
// first-parent line (fromCommit included) where `path` was added or
// modified, by commit_log. Deletions are stepped over: they're write events
// in the sparse history, not stop conditions. Returns ("", false, nil) when
// no add/modify ancestor exists. Errors propagate.
//
// "Most recent" is first-parent DEPTH, never committed_at — see
// resolveTargetCommit's doc-comment for the merge-branch rationale.
//
// Implementation (resolveActiveCommitByJumps): the path's add/modify rows
// in commit_log, nearest first by commit_fp depth, each tested for "on
// fromCommit's first-parent line" with the commit_fp jump pointers in
// O(log depth) lookups — the LiveChange technique, over commit_log instead
// of path_changes. A call costs O(log depth) per candidate examined, and the
// candidates are only the path's own writes (on any branch) at or below
// fromCommit's depth; it no longer grows with the length of the history.
// When fromCommit has no commit_fp row (an underived commit; every indexed
// commit has one) it falls back to ActiveCommitWalkCTE over commit_parents.
//
// Both forms answer exactly what the old resolver answered:
// FirstParentChainCTE JOIN commit_log ORDER BY fpc.depth LIMIT 1. That ORDER
// BY made SQLite produce and sort the WHOLE first-parent chain before the
// LIMIT applied (a recursive CTE stops early under a bare LIMIT, never under
// ORDER BY ... LIMIT), so every call was O(history) however recently path
// was written. TestResolveActiveCommitForPath_EqualsChainWalk (store
// package) holds both forms to the old query for every (path, commit).
//
// WHY commit_fp AGREES WITH commit_parents: both are derived from the same
// git first-parent edges; commit_fp rows are written, parents first, before
// a commit is recorded in branch_commits (kb invariant 11606a61), so every
// first-parent ancestor of a derived commit is derived with the depth the
// commit_parents walk would count. A missing parent commit is a boundary in
// both: commit_fp cuts depth and jump pointers there, and the walk finds no
// commit_parents row for the missing commit. commit_log rows of commits with
// no commit_fp row cannot be on a derived commit's first-parent line, so the
// JOIN dropping them changes no answer.
//
// SCHEMA INVARIANT: branch_commits is populated by full-reachability walk
// from the branch tip (PopulateCommitLog uses gogit.LogOrderDefault), so
// any first-parent ancestor of an on-branch commit is itself on-branch.
// The first-parent test restricts the answer to fromCommit's own line, so
// `branch` is implicit.
//
// ResolveActiveCommitForPath lives on Repo (it only needs rh.db) so its
// callers — facts.Index, index.Index, index.FactQuery and index.GraphStore — share it directly —
// facts.Index's fallback-before read resolves the last-valid version through the
// SAME index walk the graph resolver uses, instead of a divergent go-git walk.
func (rh *Repo) ResolveActiveCommitForPath(ctx context.Context, branch, path, fromCommit string) (string, bool, error) {
	noteGitPathRead("ResolveActiveCommitForPath")
	_ = branch // See SCHEMA INVARIANT above — branch is implicit.
	if fromCommit == "" {
		return "", false, nil
	}
	q := sqlite.Conn(ctx, rh.db)
	hash, ok, handled, err := resolveActiveCommitByJumps(ctx, q, path, fromCommit)
	if err != nil || handled {
		return hash, ok, err
	}
	return resolveActiveCommitWalk(ctx, q, path, fromCommit)
}

// activeCommitPage is how many candidate writes resolveActiveCommitByJumps
// reads per query.
const activeCommitPage = 16

// resolveActiveCommitByJumps is ResolveActiveCommitForPath over commit_fp.
// handled is false (and nothing else is set) when fromCommit has no
// commit_fp row; the caller then walks commit_parents instead. Every query
// is read to the end and closed before the next one runs, so nothing holds a
// pool connection while asking for another.
func resolveActiveCommitByJumps(ctx context.Context, q storegit.CtxExecer, path, fromCommit string) (hash string, ok, handled bool, err error) {
	var xDepth int
	err = q.QueryRowContext(ctx, `SELECT depth FROM commit_fp WHERE commit_hash = ?`, fromCommit).Scan(&xDepth)
	if err == sql.ErrNoRows {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, fmt.Errorf("resolveActiveCommitForPath: depth: %w", err)
	}
	type cand struct {
		commit string
		depth  int
	}
	// Keyset pages over the path's writes at or below fromCommit's depth,
	// deepest first; the first page starts AT fromCommit's depth ("\xff"
	// sorts above every hash), so fromCommit itself is a candidate.
	afterDepth, afterCommit := xDepth, "\xff"
	for {
		rows, err := q.QueryContext(ctx, `
			SELECT cl.commit_hash, fp.depth
			  FROM commit_log cl
			  JOIN commit_fp fp ON fp.commit_hash = cl.commit_hash
			 WHERE cl.path = ? AND cl.action IN ('added','modified')
			   AND (fp.depth < ? OR (fp.depth = ? AND cl.commit_hash < ?))
			 ORDER BY fp.depth DESC, cl.commit_hash DESC
			 LIMIT ?`, path, afterDepth, afterDepth, afterCommit, activeCommitPage)
		if err != nil {
			return "", false, true, fmt.Errorf("resolveActiveCommitForPath: candidates: %w", err)
		}
		var cands []cand
		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.commit, &c.depth); err != nil {
				rows.Close()
				return "", false, true, fmt.Errorf("resolveActiveCommitForPath: candidates: %w", err)
			}
			cands = append(cands, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", false, true, fmt.Errorf("resolveActiveCommitForPath: candidates: %w", err)
		}
		// The deepest candidate on the line is the nearest write: at most
		// one commit per depth is on it.
		for _, c := range cands {
			on, err := onFirstParentLine(ctx, q, ActiveTables, c.commit, c.depth, fromCommit, xDepth)
			if err != nil {
				return "", false, true, fmt.Errorf("resolveActiveCommitForPath: %w", err)
			}
			if on {
				return c.commit, true, true, nil
			}
		}
		if len(cands) < activeCommitPage {
			return "", false, true, nil
		}
		last := cands[len(cands)-1]
		afterDepth, afterCommit = last.depth, last.commit
	}
}

// ActiveCommitWalkCTE is the fallback walk of ResolveActiveCommitForPath,
// for a fromCommit with no commit_fp row. It binds ?1 = the start commit and
// ?2 = the path, and yields walk(commit_hash, wrote) newest → oldest along
// parent_order = 0, where wrote = 1 iff commit_log records an 'added' or
// 'modified' row for the path at that commit. The recursive step expands
// only from a row with wrote = 0, so the walk's LAST row is the first commit
// that wrote the path (or the end of the chain when none did): it yields at
// most (steps to that commit) + 1 rows, whatever the outer query does with
// them. Exported so a test can count the rows it yields.
const ActiveCommitWalkCTE = `
	WITH RECURSIVE walk(commit_hash, wrote) AS (
	    SELECT ?1, EXISTS (SELECT 1 FROM commit_log
	                        WHERE commit_hash = ?1 AND path = ?2
	                          AND action IN ('added','modified'))
	    UNION ALL
	    SELECT cp.parent_hash, EXISTS (SELECT 1 FROM commit_log
	                                    WHERE commit_hash = cp.parent_hash AND path = ?2
	                                      AND action IN ('added','modified'))
	      FROM walk
	      JOIN commit_parents cp ON cp.commit_hash = walk.commit_hash AND cp.parent_order = 0
	     WHERE walk.wrote = 0
	)`

func resolveActiveCommitWalk(ctx context.Context, q storegit.CtxExecer, path, fromCommit string) (string, bool, error) {
	var hash string
	err := q.QueryRowContext(ctx, ActiveCommitWalkCTE+`
		SELECT commit_hash FROM walk WHERE wrote = 1 LIMIT 1
	`, fromCommit, path).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolveActiveCommitForPath: %w", err)
	}
	return hash, true, nil
}
