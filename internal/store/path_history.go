package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// PathHistory returns every change to `path` in the history of `anchorCommit`
// on `branch`: one entry per commit that introduced content for the path, as a
// single linear list, newest change first.
//
// This is ENUMERATION ("every version the fact has had"), not resolution
// ("which version is live at C"). Resolution is RevisionsBefore's first-parent
// walk and stays there; reusing it here listed one PR merge per PR and hid every
// write behind it. See kb/decisions/mcp/explain/history-enumeration.
//
// Which commits are walked: git's history simplification for the path. At a
// merge whose content for the path equals one of its parents' (TREESAME), only
// the first such parent is followed — so a PR merge leads into the writes it
// delivered, and a side whose write a merge discarded is never visited. At a
// merge equal to none of its parents, every parent is followed. The walk never
// leaves `branch` (parents outside branch_commits are not visited); the anchor
// itself may be off-branch, in which case it is walked through but not listed.
//
// What is an entry: a walked, on-branch commit whose blob for the path differs
// from the path's blob in EVERY parent (a parent without the path counts as
// different) and that has the path. A merge that carries a parent's blob over is
// never an entry; a merge that produces new content is. Returning to an earlier
// blob is a change like any other.
//
// Order: author date, newest first — when the change was made, which survives
// merge delivery. Commits with the same author second are ordered by their
// position in the walked graph, a descendant always before its ancestors.
//
// Action is "added" when no parent has the path, else "modified". CommittedAt
// carries the author date used for the order.
func (hq *historyQuery) PathHistory(ctx context.Context, branch, path, anchorCommit string) ([]RevisionMeta, error) {
	if anchorCommit == "" {
		return nil, nil
	}
	branchID, err := hq.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("PathHistory: branchID: %w", err)
	}
	db := conn(ctx, hq.rh.db)

	// Every commit reachable from the anchor through on-branch parents, with
	// its ordered parent edges.
	rows, err := db.QueryContext(ctx, `
		WITH RECURSIVE r(h) AS (
		    SELECT ?
		    UNION
		    SELECT cp.parent_hash
		      FROM r
		      JOIN commit_parents cp ON cp.commit_hash = r.h
		      JOIN branch_commits bc ON bc.commit_hash = cp.parent_hash AND bc.branch_id = ?
		)
		SELECT r.h,
		       EXISTS (SELECT 1 FROM branch_commits b WHERE b.branch_id = ? AND b.commit_hash = r.h),
		       cp.parent_order, cp.parent_hash
		  FROM r
		  LEFT JOIN commit_parents cp ON cp.commit_hash = r.h
		 ORDER BY r.h, cp.parent_order
	`, anchorCommit, branchID, branchID)
	if err != nil {
		return nil, fmt.Errorf("PathHistory: reachable: %w", err)
	}
	parents := map[string][]string{}
	onBranch := map[string]bool{}
	for rows.Next() {
		var h string
		var on bool
		var order sql.NullInt64
		var parent sql.NullString
		if err := rows.Scan(&h, &on, &order, &parent); err != nil {
			rows.Close()
			return nil, fmt.Errorf("PathHistory: scan reachable: %w", err)
		}
		onBranch[h] = on
		if parent.Valid {
			parents[h] = append(parents[h], parent.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PathHistory: reachable rows: %w", err)
	}

	// commit_log rows for the path: the change of each commit against its
	// FIRST parent (see changedFilesInCommit).
	type logRow struct{ action, message string }
	logRows := map[string]logRow{}
	lrows, err := db.QueryContext(ctx,
		`SELECT commit_hash, action, message FROM commit_log WHERE path = ?`, path)
	if err != nil {
		return nil, fmt.Errorf("PathHistory: commit_log: %w", err)
	}
	for lrows.Next() {
		var h string
		var lr logRow
		if err := lrows.Scan(&h, &lr.action, &lr.message); err != nil {
			lrows.Close()
			return nil, fmt.Errorf("PathHistory: scan commit_log: %w", err)
		}
		logRows[h] = lr
	}
	lrows.Close()
	if err := lrows.Err(); err != nil {
		return nil, fmt.Errorf("PathHistory: commit_log rows: %w", err)
	}

	walkable := func(h string) bool {
		_, ok := onBranch[h]
		return ok && onBranch[h]
	}

	type entry struct {
		commit, action, message string
	}
	var entries []entry
	followed := map[string][]string{}
	visited := map[string]bool{}
	stack := []string{anchorCommit}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[c] {
			continue
		}
		visited[c] = true

		ps := parents[c]
		lr, touched := logRows[c]
		changed := touched && (lr.action == "added" || lr.action == "modified")
		var follow []string
		isEntry, action := false, ""

		switch {
		case len(ps) <= 1:
			follow = ps
			if changed {
				isEntry, action = true, lr.action
			}
		case !touched:
			// Same content as the first parent: history continues there only.
			follow = ps[:1]
		default:
			blob, has, err := hq.blobAt(c, path)
			if err != nil {
				return nil, err
			}
			sameAs := -1
			anyParentHas := false
			for i, p := range ps {
				pb, phas, err := hq.blobAt(p, path)
				if err != nil {
					return nil, err
				}
				anyParentHas = anyParentHas || phas
				if i > 0 && sameAs < 0 && phas == has && pb == blob {
					sameAs = i
				}
			}
			if sameAs >= 0 {
				follow = ps[sameAs : sameAs+1]
			} else {
				follow = ps
				if has {
					isEntry, action = true, "modified"
					if !anyParentHas {
						action = "added"
					}
				}
			}
		}

		if isEntry && onBranch[c] {
			entries = append(entries, entry{commit: c, action: action, message: lr.message})
		}
		for _, p := range follow {
			if walkable(p) {
				followed[c] = append(followed[c], p)
			}
		}
		// Push in reverse so the first parent is walked first.
		for i := len(followed[c]) - 1; i >= 0; i-- {
			stack = append(stack, followed[c][i])
		}
	}

	// Topological rank over the walked graph: a descendant is always ranked
	// before its ancestors. Breaks same-second author-date ties.
	indeg := map[string]int{}
	for _, ps := range followed {
		for _, p := range ps {
			indeg[p]++
		}
	}
	rank := map[string]int{}
	queue := []string{anchorCommit}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		rank[c] = len(rank)
		for _, p := range followed[c] {
			indeg[p]--
			if indeg[p] == 0 {
				queue = append(queue, p)
			}
		}
	}

	out := make([]RevisionMeta, 0, len(entries))
	for _, e := range entries {
		co, err := hq.rh.repo.CommitObject(plumbing.NewHash(e.commit))
		if err != nil {
			return nil, fmt.Errorf("PathHistory: commit %s: %w", e.commit, err)
		}
		msg := e.message
		if msg == "" {
			msg = co.Message
		}
		out = append(out, RevisionMeta{
			Commit:      e.commit,
			CommittedAt: co.Author.When.Unix(),
			Message:     firstLine(msg),
			Action:      e.action,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CommittedAt != out[j].CommittedAt {
			return out[i].CommittedAt > out[j].CommittedAt
		}
		return rank[out[i].Commit] < rank[out[j].Commit]
	})
	return out, nil
}

// blobAt returns the blob hash of path in commit's tree; has is false when the
// path does not exist there.
func (hq *historyQuery) blobAt(commit, path string) (blob plumbing.Hash, has bool, err error) {
	co, err := hq.rh.repo.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("PathHistory: commit %s: %w", commit, err)
	}
	tree, err := co.Tree()
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("PathHistory: tree of %s: %w", commit, err)
	}
	e, err := tree.FindEntry(path)
	if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return plumbing.ZeroHash, false, nil
	}
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("PathHistory: %s at %s: %w", path, commit, err)
	}
	return e.Hash, true, nil
}
