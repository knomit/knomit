package store

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	storegit "knomit/internal/store/git"
	"sort"
)

// LiveRevision returns the commit whose version of path is live at anchor on
// branch: the nearest commit on the anchor's first-parent line (anchor
// included) that added or modified path, counting only commits visible on the
// branch. It is RevisionsBefore(anchor, 1) — path_changes has a row for every
// commit_log add/modify row of a .md path — answered with jump pointers
// instead of a first-parent walk; below a history boundary (a missing commit
// object) it finds nothing. "" when there is none.
func (hq *historyQuery) LiveRevision(ctx context.Context, branch, path, anchor string) (string, error) {
	branchID, err := hq.rh.BranchID(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("LiveRevision: branchID: %w", err)
	}
	_, row, err := LiveChange(ctx, conn(ctx, hq.rh.DB()), ActiveTables, path, anchor, branchID)
	return row, err
}

// FactRevision is one change of a path's content: the commit that introduced a
// blob none of its parents had.
type FactRevision struct {
	Commit     string
	Blob       string // "" when content_unavailable left it unknown
	Action     string // "added" | "modified"
	Message    string // first line of the commit message
	AuthoredAt int64  // Unix seconds, the commit's author date
	// FromBlob is the content this change was edited from: the path's blob at
	// the first parent that has it. "" for an addition, or when unknown.
	FromBlob string
	// Diff is the change against FromBlob; nil when there is none to show (an
	// addition, no tracked change, content that is not a fact, or content
	// that is unavailable).
	Diff *RevisionDiff
	// ContentUnavailable: this revision's content, or the content it was
	// edited from, is missing from the repository.
	ContentUnavailable bool
}

// PathHistoryCursor is a keyset position in a path history: the changes
// still to be listed next (the frontier of the link walk), plus whether the
// anchor was on the branch when the history started.
type PathHistoryCursor struct {
	Frontier       []string
	AnchorOnBranch bool
}

// PathHistory returns up to limit changes of path, newest first, in the
// history of anchorCommit on branch, and the position to continue from (nil
// when the history is exhausted). cur == nil starts at the change live at the
// anchor, which is always the first one returned. Reads only: every indexed
// commit was derived before it was indexed.
//
// Order: newest first by author date, but a change is never listed below one
// it descends from (the precomputed order_at/gen key). ErrHistoryChanged
// reports that a continuation's anchor left the branch, or that a change the
// walk needs (the frontier, or a link from it) is not visible on the branch.
func (hq *historyQuery) PathHistory(ctx context.Context, branch, path, anchorCommit string, cur *PathHistoryCursor, limit int) ([]FactRevision, *PathHistoryCursor, error) {
	if anchorCommit == "" || limit <= 0 {
		return nil, nil, nil
	}
	branchID, err := hq.rh.BranchID(ctx, branch)
	if err != nil {
		return nil, nil, fmt.Errorf("PathHistory: branchID: %w", err)
	}
	db := conn(ctx, hq.rh.DB())
	onBranch := func(h string) (bool, error) {
		var one int
		err := db.QueryRowContext(ctx, `SELECT 1 FROM branch_commits WHERE branch_id = ? AND commit_hash = ?`, branchID, h).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}

	h := &changeHeap{}
	seen := map[string]bool{}
	next := &PathHistoryCursor{}
	if cur == nil {
		anchorOn, err := onBranch(anchorCommit)
		if err != nil {
			return nil, nil, err
		}
		next.AnchorOnBranch = anchorOn
		if anchorOn {
			// Indexing derives every commit it records, so an indexed but
			// underived anchor is a broken invariant (only damage makes it):
			// say so rather than answer with an empty history.
			var marked int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+ActiveTables.FP+` WHERE commit_hash = ?`, anchorCommit).Scan(&marked); err != nil {
				return nil, nil, fmt.Errorf("PathHistory: mark: %w", err)
			}
			if marked == 0 {
				return nil, nil, fmt.Errorf("PathHistory: commit %s is indexed on %s but its history was never derived", anchorCommit, branch)
			}
		}
		// Candidate rows are always scoped to the branch: rows of commits on
		// no branch (never garbage-collected) are not even scanned.
		head, _, err := LiveChange(ctx, db, ActiveTables, path, anchorCommit, branchID)
		if err != nil || head == "" {
			return nil, nil, err
		}
		cur = &PathHistoryCursor{Frontier: []string{head}}
	} else {
		next.AnchorOnBranch = cur.AnchorOnBranch
		if cur.AnchorOnBranch {
			on, err := onBranch(anchorCommit)
			if err != nil {
				return nil, nil, err
			}
			if !on {
				return nil, nil, ErrHistoryChanged
			}
		}
	}
	for _, f := range cur.Frontier {
		if seen[f] {
			continue
		}
		n, ok, err := loadChange(ctx, db, branchID, path, f)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, ErrHistoryChanged
		}
		seen[f] = true
		heap.Push(h, n)
	}

	var out []FactRevision
	for h.Len() > 0 && len(out) < limit {
		n := heap.Pop(h).(changeNode)
		out = append(out, n.FactRevision)
		for _, from := range n.links {
			if from == "" || seen[from] {
				continue
			}
			m, ok, err := loadChange(ctx, db, branchID, path, from)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				// A change's ancestors are on every branch it is on; a link
				// that is not visible means the branch moved under us.
				return nil, nil, ErrHistoryChanged
			}
			seen[from] = true
			heap.Push(h, m)
		}
	}
	if h.Len() == 0 {
		return out, nil, nil
	}
	for _, n := range *h {
		next.Frontier = append(next.Frontier, n.Commit)
	}
	sort.Strings(next.Frontier)
	return out, next, nil
}

// changeNode is a change plus its sort key and edited-from links.
type changeNode struct {
	FactRevision
	orderAt int64
	gen     int
	links   []string
}

// loadChange reads one entry and its links. ok is false when the entry is not
// indexed or not visible on the branch.
func loadChange(ctx context.Context, db storegit.CtxExecer, branchID int64, path, commit string) (changeNode, bool, error) {
	n := changeNode{FactRevision: FactRevision{Commit: commit}}
	var diff string
	var unavailable int
	err := db.QueryRowContext(ctx, `
		SELECT pc.blob, pc.action, pc.author_at, pc.order_at, pc.gen, pc.message, pc.diff, pc.content_unavailable
		  FROM `+ActiveTables.Changes+` pc
		  JOIN branch_commits bc ON bc.commit_hash = pc.commit_hash AND bc.branch_id = ?
		 WHERE pc.path = ? AND pc.commit_hash = ? AND pc.entry = 1`, branchID, path, commit).
		Scan(&n.Blob, &n.Action, &n.AuthoredAt, &n.orderAt, &n.gen, &n.Message, &diff, &unavailable)
	if errors.Is(err, sql.ErrNoRows) {
		return n, false, nil
	}
	if err != nil {
		return n, false, fmt.Errorf("PathHistory: change %s: %w", commit, err)
	}
	n.ContentUnavailable = unavailable != 0
	if diff != "" {
		n.Diff = &RevisionDiff{}
		if err := json.Unmarshal([]byte(diff), n.Diff); err != nil {
			return n, false, fmt.Errorf("PathHistory: diff of %s: %w", commit, err)
		}
	}
	rows, err := db.QueryContext(ctx, `
		SELECT from_commit, from_blob FROM `+ActiveTables.Links+`
		 WHERE path = ? AND commit_hash = ? ORDER BY parent_order`, path, commit)
	if err != nil {
		return n, false, fmt.Errorf("PathHistory: links %s: %w", commit, err)
	}
	defer rows.Close()
	for rows.Next() {
		var from, fromBlob string
		if err := rows.Scan(&from, &fromBlob); err != nil {
			return n, false, err
		}
		if n.FromBlob == "" {
			n.FromBlob = fromBlob
		}
		n.links = append(n.links, from)
	}
	return n, true, rows.Err()
}

// changeHeap pops the newest change first: order_at desc, gen desc, commit asc.
// Because a change's key is strictly after every change it was edited from,
// popping from the live change yields the whole history in key order.
type changeHeap []changeNode

func (h changeHeap) Len() int { return len(h) }

func (h changeHeap) Less(i, j int) bool {
	if h[i].orderAt != h[j].orderAt {
		return h[i].orderAt > h[j].orderAt
	}
	if h[i].gen != h[j].gen {
		return h[i].gen > h[j].gen
	}
	return h[i].Commit < h[j].Commit
}

func (h changeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *changeHeap) Push(x any) { *h = append(*h, x.(changeNode)) }

func (h *changeHeap) Pop() any {
	old := *h
	n := old[len(old)-1]
	*h = old[:len(old)-1]
	return n
}

// RevisionsBefore returns up to `limit` revisions of `path` in the first-parent
// ancestry of `anchorCommit`, newest → oldest, restricted to commits where
// `path` was added or modified. Deletions are stepped over — they are write
// events in the sparse history, not stop conditions.
//
// History is bounded to the ancestry of `anchorCommit`: revisions committed
// after the anchor are never surfaced ("explain as of C" rewinds the timeline).
//
// Walks first-parent ancestry via the shared FirstParentChainCTE — NEVER
// wall-clock ordering. See resolveTargetCommit's doc-comment for the
// merge-branch rationale. The walk is scoped to `branch` by joining
// branch_commits on the resolved branch_id, so revisions reachable only via an
// off-branch lineage never surface even when anchorCommit was committed
// elsewhere.
func (hq *historyQuery) RevisionsBefore(ctx context.Context, branch, path, anchorCommit string, limit int) ([]RevisionMeta, error) {
	if anchorCommit == "" || limit <= 0 {
		return nil, nil
	}
	branchID, err := hq.rh.BranchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("RevisionsBefore: branchID: %w", err)
	}
	rows, err := conn(ctx, hq.rh.DB()).QueryContext(ctx, FirstParentChainCTE+`
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
		rm.Message = FirstLine(msg)
		out = append(out, rm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("RevisionsBefore: rows: %w", err)
	}
	return out, nil
}
