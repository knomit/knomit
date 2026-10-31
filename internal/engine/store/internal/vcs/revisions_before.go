package vcs

import (
	"context"
	"database/sql"
	"fmt"

	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/internal/sqlite"
)

// ResolveActiveCommitForPath returns the most recent commit on fromCommit's
// first-parent line (fromCommit included) where `path` was added or
// modified, by commit_log. Deletions are stepped over: they're write events
// in the sparse history, not stop conditions. Returns ("", false, nil) when
// no add/modify ancestor exists. Errors propagate.
//
// "Most recent" is first-parent DEPTH, never committed_at — see
// resolveTargetCommit's doc-comment for the merge-branch rationale.
//
// It is LineEvents with Writes and Limit 1: commit_fp jump pointers, with
// the commit_parents walk as the fallback for an underived or structurally
// degraded fromCommit. See LineEvents for the cost and for why the two agree
// with the ORDER BY fpc.depth chain walk this used to run.
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
	evs, err := LineEvents(ctx, sqlite.Conn(ctx, rh.db), fromCommit, LineQuery{Path: path, Writes: true, Limit: 1})
	if err != nil {
		return "", false, fmt.Errorf("resolveActiveCommitForPath: %w", err)
	}
	if len(evs) == 0 {
		return "", false, nil
	}
	return evs[0].Commit, true, nil
}

// LineQuery selects the commit_log events LineEvents returns: rows for Path,
// only 'added'/'modified' when Writes is set (every action otherwise), only
// at commits on the branch with BranchID when it is non-zero, at most Limit
// of them (Limit must be positive).
type LineQuery struct {
	Path     string
	Writes   bool
	BranchID int64
	Limit    int
}

// LineEvent is one commit_log row LineEvents returns.
type LineEvent struct {
	Commit      string
	CommittedAt int64
	Message     string
	Action      string
}

// LineEvents returns the commit_log rows that q selects at commits on
// from's FIRST-PARENT LINE (from included), nearest first, at most q.Limit.
// It answers exactly what this did before:
//
//	WITH RECURSIVE fpc AS (from, then parent_order = 0 over commit_parents)
//	SELECT ... FROM fpc JOIN commit_log [JOIN branch_commits] ...
//	 ORDER BY fpc.depth LIMIT q.Limit
//
// whose ORDER BY made SQLite produce and sort the WHOLE first-parent chain
// before its LIMIT (a recursive CTE stops early under a bare LIMIT, never
// under ORDER BY ... LIMIT): every call was O(history). It is the one
// implementation behind ResolveActiveCommitForPath,
// HistoryQuery.RevisionsBefore and FactQuery.FactLiveAtCommit.
//
// HOW (lineEventsByJumps): the path's own commit_log rows, nearest first by
// commit_fp depth, each tested for "on from's first-parent line" with the
// commit_fp jump pointers in O(log depth) statements — the LiveChange
// technique over commit_log. A call costs O(log depth) per row examined, and
// the rows examined are only the path's own events (on any branch) at or
// below from's depth; it does not grow with the length of the history.
//
// FALLBACK (lineEventsByWalk, over commit_parents, stopping once it has
// q.Limit rows) whenever commit_fp cannot answer in full:
//   - from has NO commit_fp row (not derived): there is no depth to jump
//     from;
//   - from is STRUCTURALLY DEGRADED (commit_fp.degraded = 2: a parent commit
//     was missing when it or an ancestor was derived, so depth and jump
//     pointers are cut at that boundary) AND fewer than q.Limit rows were
//     found above the boundary. commit_parents still holds the edges past
//     the boundary, so the walk can see rows the jumps cannot. Rows found
//     above the boundary are the walk's first rows too, so a full answer
//     needs no fallback. (degraded = 1, content_unavailable, leaves depth
//     and pointers intact and changes nothing here.)
//
// WHY commit_fp AGREES WITH commit_parents everywhere else: both record
// git's first-parent edges (fsck checks commit_parents against the commit
// objects); commit_fp rows are written parents first, before a commit is
// recorded in branch_commits (kb invariant 11606a61), so every first-parent
// ancestor of a derived, undegraded commit is derived, with the depth the
// commit_parents walk would count. commit_log rows at commits with no
// commit_fp row cannot be on such a commit's line, so the JOIN dropping them
// changes no answer. Where commit_parents is itself incomplete (a backfill
// that skipped an unreadable commit) the jumps follow git and the old walk
// stopped short; the jumps are the correct answer there.
func LineEvents(ctx context.Context, q storegit.CtxExecer, from string, lq LineQuery) ([]LineEvent, error) {
	evs, complete, err := lineEventsByJumps(ctx, q, from, lq)
	if err != nil || complete {
		return evs, err
	}
	return lineEventsByWalk(ctx, q, from, lq)
}

// lineActionSQL is the action filter of a LineQuery over commit_log alias cl.
func lineActionSQL(lq LineQuery) string {
	if lq.Writes {
		return ` AND cl.action IN ('added','modified')`
	}
	return ``
}

// lineBranchSQL restricts commit_log alias cl to the LineQuery's branch.
func lineBranchSQL(lq LineQuery) string {
	if lq.BranchID == 0 {
		return ``
	}
	return ` AND EXISTS (SELECT 1 FROM branch_commits bc WHERE bc.branch_id = ` + fmt.Sprint(lq.BranchID) + ` AND bc.commit_hash = cl.commit_hash)`
}

// linePage is how many candidate rows lineEventsByJumps reads per query.
const linePage = 16

// lineEventsByJumps is LineEvents over commit_fp. complete is false when the
// caller must walk commit_parents instead (see LineEvents' FALLBACK); evs is
// then partial and must be discarded. Every query is read to the end and
// closed before the next runs, so nothing holds a pool connection while
// asking for another (kb decision 189c424e).
func lineEventsByJumps(ctx context.Context, q storegit.CtxExecer, from string, lq LineQuery) (evs []LineEvent, complete bool, err error) {
	if lq.Limit <= 0 {
		return nil, true, nil
	}
	var xDepth, degraded int
	err = q.QueryRowContext(ctx, `SELECT depth, degraded FROM commit_fp WHERE commit_hash = ?`, from).Scan(&xDepth, &degraded)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("first-parent line: depth: %w", err)
	}
	type cand struct {
		commit string
		depth  int
	}
	var hits []string
	// Keyset pages over the path's rows at or below from's depth, deepest
	// first; the first page starts AT from's depth ("\xff" sorts above every
	// hash), so from itself is a candidate.
	afterDepth, afterCommit := xDepth, "\xff"
pages:
	for {
		rows, err := q.QueryContext(ctx, `
			SELECT cl.commit_hash, fp.depth
			  FROM commit_log cl
			  JOIN commit_fp fp ON fp.commit_hash = cl.commit_hash
			 WHERE cl.path = ?`+lineActionSQL(lq)+lineBranchSQL(lq)+`
			   AND (fp.depth < ? OR (fp.depth = ? AND cl.commit_hash < ?))
			 ORDER BY fp.depth DESC, cl.commit_hash DESC
			 LIMIT ?`, lq.Path, afterDepth, afterDepth, afterCommit, linePage)
		if err != nil {
			return nil, false, fmt.Errorf("first-parent line: candidates: %w", err)
		}
		var cands []cand
		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.commit, &c.depth); err != nil {
				rows.Close()
				return nil, false, fmt.Errorf("first-parent line: candidates: %w", err)
			}
			cands = append(cands, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, false, fmt.Errorf("first-parent line: candidates: %w", err)
		}
		// At most one commit per depth is on the line, so the candidates on
		// it, in this order, are the line's rows nearest first.
		for _, c := range cands {
			on, err := onFirstParentLine(ctx, q, ActiveTables, c.commit, c.depth, from, xDepth)
			if err != nil {
				return nil, false, fmt.Errorf("first-parent line: %w", err)
			}
			if on {
				hits = append(hits, c.commit)
				if len(hits) == lq.Limit {
					break pages
				}
			}
		}
		if len(cands) < linePage {
			break
		}
		last := cands[len(cands)-1]
		afterDepth, afterCommit = last.depth, last.commit
	}
	if len(hits) < lq.Limit && degraded == degradedStructural {
		return nil, false, nil
	}
	evs = make([]LineEvent, 0, len(hits))
	for _, h := range hits {
		ev := LineEvent{Commit: h}
		if err := q.QueryRowContext(ctx, `
			SELECT COALESCE(committed_at, 0), message, action FROM commit_log
			 WHERE commit_hash = ? AND path = ?`, h, lq.Path).Scan(&ev.CommittedAt, &ev.Message, &ev.Action); err != nil {
			return nil, false, fmt.Errorf("first-parent line: row: %w", err)
		}
		evs = append(evs, ev)
	}
	return evs, true, nil
}

// LineWalkCTE is the fallback walk of LineEvents over commit_parents for
// query lq. It binds ?1 = the start commit, ?2 = the path and ?3 = the limit,
// and yields walk(commit_hash, depth, hits) newest → oldest along
// parent_order = 0, where hits counts the commits so far (this one included)
// with a commit_log row lq selects. The recursive step expands only while
// hits < limit, so the walk ends at the limit-th such commit (or the end of
// the chain): it yields at most (steps to that commit) + 1 rows, whatever
// the outer query does with them. Exported so a test can count them.
func LineWalkCTE(lq LineQuery) string {
	hit := func(c string) string {
		return `EXISTS (SELECT 1 FROM commit_log cl WHERE cl.commit_hash = ` + c + ` AND cl.path = ?2` +
			lineActionSQL(lq) + lineBranchSQL(lq) + `)`
	}
	return `
	WITH RECURSIVE walk(commit_hash, depth, hits) AS (
	    SELECT ?1, 0, ` + hit("?1") + `
	    UNION ALL
	    SELECT cp.parent_hash, walk.depth + 1, walk.hits + ` + hit("cp.parent_hash") + `
	      FROM walk
	      JOIN commit_parents cp ON cp.commit_hash = walk.commit_hash AND cp.parent_order = 0
	     WHERE walk.hits < ?3
	)`
}

// lineEventsByWalk is LineEvents over commit_parents (LineWalkCTE). Its
// ORDER BY sorts only the walk's own rows, which the walk bounds.
func lineEventsByWalk(ctx context.Context, q storegit.CtxExecer, from string, lq LineQuery) ([]LineEvent, error) {
	if lq.Limit <= 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, LineWalkCTE(lq)+`
		SELECT cl.commit_hash, COALESCE(cl.committed_at, 0), cl.message, cl.action
		  FROM walk
		  JOIN commit_log cl ON cl.commit_hash = walk.commit_hash AND cl.path = ?2
		 WHERE 1 = 1`+lineActionSQL(lq)+lineBranchSQL(lq)+`
		 ORDER BY walk.depth ASC
		 LIMIT ?3`, from, lq.Path, lq.Limit)
	if err != nil {
		return nil, fmt.Errorf("first-parent line: walk: %w", err)
	}
	defer rows.Close()
	var evs []LineEvent
	for rows.Next() {
		var ev LineEvent
		if err := rows.Scan(&ev.Commit, &ev.CommittedAt, &ev.Message, &ev.Action); err != nil {
			return nil, fmt.Errorf("first-parent line: walk: %w", err)
		}
		evs = append(evs, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("first-parent line: walk: %w", err)
	}
	return evs, nil
}
