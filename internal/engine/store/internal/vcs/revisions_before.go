package vcs

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"

	"github.com/go-git/go-git/v5/plumbing"

	storegit "knomit/internal/engine/store/git"
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
	evs, err := LineEvents(ctx, rh.db, fromCommit, LineQuery{Path: path, Writes: true, Limit: 1})
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

// LineEvents returns the commit_log rows that lq selects at commits on
// from's FIRST-PARENT LINE (from included), nearest first, at most lq.Limit.
// It answers exactly what this did before:
//
//	WITH RECURSIVE fpc AS (from, then parent_order = 0 over commit_parents)
//	SELECT ... FROM fpc JOIN commit_log [JOIN branch_commits] ...
//	 ORDER BY fpc.depth LIMIT lq.Limit
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
// ONE SNAPSHOT: every statement of a call runs in one read transaction — the
// caller's, when ctx carries one, else a deferred BEGIN on one connection of
// db's pool — so a concurrent re-derivation or rebuild committing between two
// statements cannot mix row generations into one answer (the old single
// statement was one snapshot by construction).
//
// FALLBACK (lineEventsByWalk, over commit_parents, stopping once it has
// lq.Limit rows) whenever commit_fp cannot answer in full:
//   - from has NO commit_fp row (not derived): there is no depth to jump
//     from;
//   - from is STRUCTURALLY DEGRADED (commit_fp.degraded = 2: a parent commit
//     was missing when it or an ancestor was derived, so depth and jump
//     pointers are cut at that boundary) AND fewer than lq.Limit rows were
//     found above the boundary. commit_parents still holds the edges past
//     the boundary, so the walk can see rows the jumps cannot. Rows found
//     above the boundary are the walk's first rows too, so a full answer
//     needs no fallback;
//   - a jump CONTRADICTS the rows (onLine reports broken): a node on the way
//     down has a depth other than the one its distance from from implies, or
//     runs out of jump pointers before the jump is done. Neither happens in a
//     consistent store; both are what an inconsistent row would look like, so
//     the walk answers instead of a possibly wrong jump.
//
// degraded = 1 (content_unavailable) leaves depth and pointers as they were
// derived and changes nothing here — because re-derivation never turns a
// row with degraded < 2 into a boundary under descendants derived over it
// (deriver.wouldCutDerivedLine).
//
// WHY commit_fp AGREES WITH commit_parents everywhere else: both record
// git's first-parent edges (fsck checks commit_parents against the commit
// objects); commit_fp rows are written parents first, before a commit is
// recorded in branch_commits (kb invariant 11606a61), and every row's depth
// and pointers agree with its first-parent ancestors' rows (kept so by
// wouldCutDerivedLine), so every first-parent ancestor of a derived,
// undegraded commit is derived, with the depth the commit_parents walk would
// count. commit_log rows at commits with no commit_fp row cannot be on such a
// commit's line, so the JOIN dropping them changes no answer. Where
// commit_parents is itself incomplete (a backfill that skipped an unreadable
// commit) the jumps follow git and the old walk stopped short; the jumps are
// the correct answer there.
func LineEvents(ctx context.Context, db *sql.DB, from string, lq LineQuery) ([]LineEvent, error) {
	if lq.Limit <= 0 {
		return nil, nil
	}
	if tx := storegit.TxFromContext(ctx); tx != nil {
		return lineEvents(ctx, tx, from, lq)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("first-parent line: conn: %w", err)
	}
	defer conn.Close()
	// A deferred BEGIN: the first SELECT pins one WAL read snapshot; no write
	// lock is taken (the pool's _txlock=immediate applies to BeginTx only).
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		return nil, fmt.Errorf("first-parent line: begin: %w", err)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`) //nolint:errcheck
	return lineEvents(ctx, conn, from, lq)
}

// lineEvents is LineEvents on one executor (already a single snapshot).
func lineEvents(ctx context.Context, q storegit.CtxExecer, from string, lq LineQuery) ([]LineEvent, error) {
	evs, complete, err := lineEventsByJumps(ctx, q, from, lq)
	if err != nil || complete {
		return evs, err
	}
	return lineEventsByWalk(ctx, q, from, lq)
}

// lineWrites binds LineQuery.Writes as an integer parameter.
func lineWrites(lq LineQuery) int {
	if lq.Writes {
		return 1
	}
	return 0
}

// linePage is how many candidate rows lineEventsByJumps reads per query.
const linePage = 16

// lineCandidatesSQL pages a LineQuery's candidates, deepest first. Binds ?1
// path, ?2/?3 the keyset (depth, commit) to continue below, ?4 the page
// size, ?5 the branch id (0 = any branch), ?6 writes only (1) or every
// action (0). The text is constant, so the statement is prepared once.
const lineCandidatesSQL = `
	SELECT cl.commit_hash, fp.depth
	  FROM commit_log cl
	  JOIN commit_fp fp ON fp.commit_hash = cl.commit_hash
	 WHERE cl.path = ?1
	   AND (?6 = 0 OR cl.action IN ('added','modified'))
	   AND (?5 = 0 OR EXISTS (SELECT 1 FROM branch_commits bc
	                           WHERE bc.branch_id = ?5 AND bc.commit_hash = cl.commit_hash))
	   AND (fp.depth < ?2 OR (fp.depth = ?2 AND cl.commit_hash < ?3))
	 ORDER BY fp.depth DESC, cl.commit_hash DESC
	 LIMIT ?4`

// LineEventsAfterDepthHook, when set (tests only), runs after
// lineEventsByJumps has read from's depth and before its first candidate
// page: a test commits a change there to prove the call reads one snapshot.
var lineEventsAfterDepthHook atomic.Pointer[func()]

// lineEventsByJumps is LineEvents over commit_fp. complete is false when the
// caller must walk commit_parents instead (see LineEvents' FALLBACK); evs is
// then partial and must be discarded. Every query is read to the end and
// closed before the next runs (kb decision 189c424e).
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
	if h := lineEventsAfterDepthHook.Load(); h != nil {
		(*h)()
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
		rows, err := q.QueryContext(ctx, lineCandidatesSQL,
			lq.Path, afterDepth, afterCommit, linePage, lq.BranchID, lineWrites(lq))
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
			on, broken, err := onLine(ctx, q, c.commit, c.depth, from, xDepth)
			if err != nil {
				return nil, false, fmt.Errorf("first-parent line: %w", err)
			}
			if broken {
				return nil, false, nil
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

// onLine reports whether r (at depth rDepth) is x or one of x's first-parent
// ancestors, by jumping x down xDepth-rDepth steps with the commit_fp jump
// pointers — onFirstParentLine, CHECKED: broken is true when the rows
// contradict the jump, i.e. a node reached on the way has a depth other than
// x's depth minus its distance from x, or has no pointer for the next jump
// (a node at depth d has one for every 2^k <= d, and the remaining steps
// never exceed the current node's depth). A consistent store never reports
// broken; the caller walks commit_parents when it does.
func onLine(ctx context.Context, q storegit.CtxExecer, r string, rDepth int, x string, xDepth int) (on, broken bool, err error) {
	steps := xDepth - rDepth
	if steps < 0 {
		return false, false, nil
	}
	cur, want := x, xDepth
	for level := 0; steps > 0; level++ {
		if steps&1 == 1 {
			var depth int
			var ups []byte
			err := q.QueryRowContext(ctx, `SELECT depth, ups FROM `+ActiveTables.FP+` WHERE commit_hash = ?`, cur).Scan(&depth, &ups)
			if err == sql.ErrNoRows {
				return false, true, nil
			}
			if err != nil {
				return false, false, fmt.Errorf("first-parent jump: %w", err)
			}
			off := level * len(plumbing.ZeroHash)
			if depth != want || off+len(plumbing.ZeroHash) > len(ups) {
				return false, true, nil
			}
			var h plumbing.Hash
			copy(h[:], ups[off:])
			cur = h.String()
			want -= 1 << level
		}
		steps >>= 1
	}
	return cur == r, false, nil
}

// LineWalkCTE is the fallback walk of LineEvents over commit_parents. It
// binds ?1 the start commit, ?2 the path, ?3 the limit, ?4 the branch id
// (0 = any branch) and ?5 writes only (1) or every action (0), and yields
// walk(commit_hash, depth, hits) newest → oldest along parent_order = 0,
// where hits counts the commits so far (this one included) with a
// commit_log row the query selects. The recursive step expands only while
// hits < limit, so the walk ends at the limit-th such commit (or the end of
// the chain): it yields at most (steps to that commit) + 1 rows, whatever
// the outer query does with them. Exported so a test can count them.
const LineWalkCTE = `
	WITH RECURSIVE walk(commit_hash, depth, hits) AS (
	    SELECT ?1, 0, EXISTS (SELECT 1 FROM commit_log cl WHERE cl.commit_hash = ?1 AND cl.path = ?2
	                AND (?5 = 0 OR cl.action IN ('added','modified'))
	                AND (?4 = 0 OR EXISTS (SELECT 1 FROM branch_commits bc WHERE bc.branch_id = ?4 AND bc.commit_hash = cl.commit_hash)))
	    UNION ALL
	    SELECT cp.parent_hash, walk.depth + 1, walk.hits + EXISTS (SELECT 1 FROM commit_log cl WHERE cl.commit_hash = cp.parent_hash AND cl.path = ?2
	                AND (?5 = 0 OR cl.action IN ('added','modified'))
	                AND (?4 = 0 OR EXISTS (SELECT 1 FROM branch_commits bc WHERE bc.branch_id = ?4 AND bc.commit_hash = cl.commit_hash)))
	      FROM walk
	      JOIN commit_parents cp ON cp.commit_hash = walk.commit_hash AND cp.parent_order = 0
	     WHERE walk.hits < ?3
	)`

// lineRowFilterSQL is the walk's filter on commit_log alias cl (the same
// filter LineWalkCTE counts hits with).
const lineRowFilterSQL = ` AND (?5 = 0 OR cl.action IN ('added','modified'))
	AND (?4 = 0 OR EXISTS (SELECT 1 FROM branch_commits bc WHERE bc.branch_id = ?4 AND bc.commit_hash = cl.commit_hash))`

// lineEventsByWalk is LineEvents over commit_parents (LineWalkCTE). Its
// ORDER BY sorts only the walk's own rows, which the walk bounds.
func lineEventsByWalk(ctx context.Context, q storegit.CtxExecer, from string, lq LineQuery) ([]LineEvent, error) {
	if lq.Limit <= 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, LineWalkCTE+`
		SELECT cl.commit_hash, COALESCE(cl.committed_at, 0), cl.message, cl.action
		  FROM walk
		  JOIN commit_log cl ON cl.commit_hash = walk.commit_hash AND cl.path = ?2
		 WHERE 1 = 1`+lineRowFilterSQL+`
		 ORDER BY walk.depth ASC
		 LIMIT ?3`, from, lq.Path, lq.Limit, lq.BranchID, lineWrites(lq))
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
