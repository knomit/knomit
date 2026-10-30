package index

import (
	"context"
	"fmt"
	"strings"
)

// IncomingAtCommit returns the facts that cite (path, commitHash) on the
// given branch: the DERIVED_FROM edges INTO the target's blob at that commit,
// each described by the nodes of its citing blob that this branch holds (now
// or in its history), with a commit of this branch at which that path holds
// it. Visibility is "the citing blob is held on this branch", not "the
// edge's source_commit is on it" (user ruling 2026-10-30, PR #693 round 4).
//
// Each returned entry is a distinct ref-event: the same source_path can
// appear multiple times (different source_commits = different versions of
// the source).
//
// NO committed_at / wall-clock bound is applied. Incoming referrers are, by
// nature, written AFTER the target version they point at, so a "source
// committed_at ≤ anchor committed_at" bound would drop every legitimate
// referrer whenever git's 1-second commit clock puts the referrer in a later
// second than the anchor — nondeterministic, and a direct violation of the
// first-parent-never-wall-clock resolver invariant (see
// ResolveActiveCommitForPath / resolveTargetCommit). Referrers to a distinct
// version of `path` are already isolated by the (path, blob_hash) node key
// below. The ONE case the node key cannot separate — a byte-identical revert
// creating a second blob-version episode whose later referrers surface at the
// earlier episode's anchor — is an accepted gap: it is structurally
// indistinguishable (via first-parent resolution) from the legitimate
// merge-carry-forward that TestGraphAtCommit_ResolvesThroughMergeCommit pins,
// where the edge's target_commit likewise differs from the resolved anchor.
func (gs *GraphStore) IncomingAtCommit(ctx context.Context, branch, path, commitHash string) ([]RefSummary, error) {
	gs.probe("IncomingAtCommit")
	branchID, err := gs.rh.BranchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("IncomingAtCommit: branchID: %w", err)
	}

	// Sparse-history walk-back: edges are stored anchored to the target's
	// actual write-commit. Resolve `commitHash` (the query anchor) into the
	// target path's effective write-commit ≤ commitHash. Without this, queries
	// at any commit other than the exact write-commit return 0. See the
	// historical-graph-invariant note.
	effectiveCommit, ok, err := gs.rh.ResolveActiveCommitForPath(ctx, branch, path, commitHash)
	if err != nil {
		return nil, fmt.Errorf("IncomingAtCommit: resolve effective commit: %w", err)
	}
	if !ok {
		// Target path was never written in the ancestry of commitHash —
		// there can't be any edges into it.
		return nil, nil
	}
	// Match by the target fact's BLOB-version node at effectiveCommit, not by
	// the edge's `target_commit`. See OutgoingAtCommit for the merge-commit
	// rationale: a merge re-touches the path (so ResolveActiveCommitForPath
	// lands on the merge) but carries the same blob forward without re-anchoring
	// the edges, so a target_commit == effectiveCommit filter drops every
	// incoming edge at a merge-resolved anchor.
	blobHash, err := gs.rh.ReadBlobHashAtCommit(ctx, path, effectiveCommit)
	if err != nil {
		return nil, fmt.Errorf("IncomingAtCommit: blob at %s: %w", effectiveCommit, err)
	}
	if blobHash == "" {
		return nil, nil
	}

	// 1. Candidate (source_path, source_title, source_commit, source_deleted) rows.
	// Retracted sources are returned with deleted=true so the UI can render
	// them with the retracted treatment instead of hiding the edge entirely
	// (mirrors how OutgoingAtCommit exposes retracted targets).
	//
	// VISIBILITY (user ruling 2026-10-30, PR #693 round 4): an edge is
	// visible on this branch iff the CITING BLOB is held on it, not iff the
	// edge's source_commit is on it. A blob's edges are resolved once, at its
	// earliest introduction, which may be on another branch that never
	// merged here (the same bytes written independently). The source is
	// described by every node of the citing blob, one row per node whose
	// path holds the blob on this branch, now (branch_facts) or in its
	// history (path_changes on its commits: a retracted citer, a rename's old
	// path), so a path that only another branch has does not surface here.
	found, err := graphDerivedFromNeighbourRows(ctx, conn(ctx, gs.rh.DB()), blobHash, true)
	if err != nil {
		return nil, fmt.Errorf("IncomingAtCommit: read edges: %w", err)
	}
	if len(found) == 0 {
		return []RefSummary{}, nil
	}
	held, onBranch, dates, err := gs.blobHoldings(ctx, branchID, found)
	if err != nil {
		return nil, fmt.Errorf("IncomingAtCommit: %w", err)
	}
	out := make([]RefSummary, 0, len(found))
	for _, n := range found {
		rs := n.RefSummary
		ownOnBranch := n.Own && onBranch[rs.Commit]
		h, ok := held[n.Path+"|"+n.Blob]
		if !ok && !ownOnBranch {
			continue // this path never held the citing blob on this branch
		}
		// The commit reported is one of THIS branch at which the row's path
		// holds the citing blob, so a hop to (path, commit) on this branch
		// lands on it: the edge's source_commit when the row is the edge's
		// own node and that commit is on this branch (as before), else the
		// commit the branch records for it now, else the latest one in its
		// history.
		switch {
		case ownOnBranch:
		case h.now != "":
			rs.Commit = h.now
		default:
			rs.Commit = h.past
		}
		// Defense-in-depth: stale self-loops (same path & same commit on
		// both endpoints) navigate to nowhere; drop them. Legitimate
		// "previous version" edges have differing commits and pass through.
		if rs.Path == path && rs.Commit == commitHash {
			continue
		}
		rs.CommittedAt = dates[rs.Commit]
		out = append(out, rs)
	}
	return out, nil
}

// blobHolding is how a branch holds one (path, blob) pair: the commit it
// records for it now (branch_facts; "" if it no longer holds it there) and
// the latest commit of its history at which the path held it ("" if never).
type blobHolding struct{ now, past string }

// blobHoldings answers, in one statement, how the branch holds each
// candidate row's (path, blob); pairs it never held are absent from the map.
// A second statement tells which candidate source commits are on the branch.
// dates holds committed_at for every commit either statement returned.
func (gs *GraphStore) blobHoldings(ctx context.Context, branchID int64, rows []derivedFromNeighbour) (held map[string]blobHolding, onBranch map[string]bool, dates map[string]int64, err error) {
	type pair struct{ path, blob string }
	seen := map[pair]bool{}
	var values []string
	var args []any
	srcSeen := map[string]bool{}
	var srcArgs []any
	for _, r := range rows {
		p := pair{r.Path, r.Blob}
		if !seen[p] {
			seen[p] = true
			values = append(values, "(?, ?)")
			args = append(args, r.Path, r.Blob)
		}
		if r.Commit != "" && !srcSeen[r.Commit] {
			srcSeen[r.Commit] = true
			srcArgs = append(srcArgs, r.Commit)
		}
	}
	db := conn(ctx, gs.rh.DB())
	onBranch = map[string]bool{}
	dates = map[string]int64{}
	if len(srcArgs) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(srcArgs)), ",")
		rs, err := db.QueryContext(ctx, fmt.Sprintf(`
			SELECT bc.commit_hash, COALESCE((SELECT committed_at FROM commit_log WHERE commit_hash = bc.commit_hash LIMIT 1), 0)
			  FROM branch_commits bc
			 WHERE bc.branch_id = %d AND bc.commit_hash IN (%s)`, branchID, ph), srcArgs...)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("source commits on branch: %w", err)
		}
		for rs.Next() {
			var h string
			var ts int64
			if err := rs.Scan(&h, &ts); err != nil {
				rs.Close()
				return nil, nil, nil, fmt.Errorf("source commits on branch: %w", err)
			}
			onBranch[h] = true
			dates[h] = ts
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return nil, nil, nil, fmt.Errorf("source commits on branch: %w", err)
		}
	}
	// branchID is an int64 from BranchID, formatted into the statement; the
	// (path, blob) pairs are bound.
	q := fmt.Sprintf(`
		WITH c(path, blob) AS (VALUES %s),
		h AS (
			SELECT c.path, c.blob,
			       (SELECT bf.commit_hash FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id
			         WHERE bf.branch_id = %[2]d AND bf.path = c.path AND f.blob_hash = c.blob) AS now,
			       (SELECT pc.commit_hash FROM path_changes pc
			          JOIN branch_commits bc ON bc.commit_hash = pc.commit_hash AND bc.branch_id = %[2]d
			         WHERE pc.path = c.path AND pc.blob = c.blob
			         ORDER BY pc.fp_depth DESC, pc.commit_hash DESC LIMIT 1) AS past
			  FROM c
		)
		SELECT path, blob, COALESCE(now, ''), COALESCE(past, ''),
		       COALESCE((SELECT committed_at FROM commit_log WHERE commit_hash = now LIMIT 1), 0),
		       COALESCE((SELECT committed_at FROM commit_log WHERE commit_hash = past LIMIT 1), 0)
		  FROM h
		 WHERE now IS NOT NULL OR past IS NOT NULL`, strings.Join(values, ", "), branchID)
	rs, qerr := db.QueryContext(ctx, q, args...)
	err = qerr
	if err != nil {
		return nil, nil, nil, fmt.Errorf("citing blob held on branch: %w", err)
	}
	defer rs.Close()
	held = map[string]blobHolding{}
	for rs.Next() {
		var path, blob, now, past string
		var tNow, tPast int64
		if err := rs.Scan(&path, &blob, &now, &past, &tNow, &tPast); err != nil {
			return nil, nil, nil, fmt.Errorf("citing blob held on branch: %w", err)
		}
		if now != "" {
			dates[now] = tNow
		}
		if past != "" {
			dates[past] = tPast
		}
		held[path+"|"+blob] = blobHolding{now: now, past: past}
	}
	return held, onBranch, dates, rs.Err()
}

// OutgoingAtCommit returns the set of (target_path, target_commit) refs
// asserted by (path, commitHash) — DERIVED_FROM edges OUT of this Fact
// where source_commit edge property matches.
//
// Unlike IncomingAtCommit, this does not filter by branch_commits: outgoing
// edges naturally reflect the source's commit, which is already pinned by
// commitHash. The branch parameter is accepted for signature symmetry and
// reserved for future use (e.g. branch-relative target visibility).
//
// Candidates whose target_commit is missing from commit_log are dropped:
// returning them would expose self-links that 404. This is the only filter
// applied — branch reachability is intentionally not enforced here.
func (gs *GraphStore) OutgoingAtCommit(ctx context.Context, branch, path, commitHash string) ([]RefSummary, error) {
	gs.probe("OutgoingAtCommit")
	// Sparse-history walk-back: edges are stored anchored to the source's
	// actual write-commit. Resolve `commitHash` (the query anchor) into the
	// source path's effective write-commit ≤ commitHash. Without this, queries
	// at any commit other than the exact write-commit return 0. See the
	// historical-graph-invariant note.
	effectiveCommit, ok, err := gs.rh.ResolveActiveCommitForPath(ctx, branch, path, commitHash)
	if err != nil {
		return nil, fmt.Errorf("OutgoingAtCommit: resolve effective commit: %w", err)
	}
	if !ok {
		// Source path was never written in the ancestry of commitHash —
		// no edges to surface.
		return nil, nil
	}
	// Match edges by the source fact's BLOB-version node at effectiveCommit, not
	// by the edge's `source_commit` property. A merge commit records a
	// commit_log add/modify for the path (so ResolveActiveCommitForPath lands on
	// the merge), but carries the SAME blob forward and never re-anchors the
	// edges — which stay on the original content-write commit. Filtering by
	// source_commit == effectiveCommit therefore drops every edge at any anchor
	// that resolves to a merge (i.e. at HEAD after a merge-back). The Fact node
	// is keyed by (path, blob_hash) and the blob is stable across the merge, so
	// matching the node finds the edges regardless of which commit anchored them.
	blobHash, err := gs.rh.ReadBlobHashAtCommit(ctx, path, effectiveCommit)
	if err != nil {
		return nil, fmt.Errorf("OutgoingAtCommit: blob at %s: %w", effectiveCommit, err)
	}
	if blobHash == "" {
		return nil, nil
	}
	found, err := graphDerivedFromNeighbours(ctx, conn(ctx, gs.rh.DB()), path, blobHash, false)
	if err != nil {
		return nil, fmt.Errorf("OutgoingAtCommit: read edges: %w", err)
	}
	candidates := make([]RefSummary, 0, len(found))
	for _, rs := range found {
		// Defense-in-depth: drop stale self-loops where both endpoints
		// are the same (path, commit). resolveTargetCommit no longer
		// produces these, but legacy data may still hold them.
		if rs.Path == path && rs.Commit == commitHash {
			continue
		}
		candidates = append(candidates, rs)
	}

	if len(candidates) == 0 {
		return candidates, nil
	}
	args := make([]any, 0, len(candidates))
	placeholders := make([]string, 0, len(candidates))
	for _, c := range candidates {
		args = append(args, c.Commit)
		placeholders = append(placeholders, "?")
	}
	clRows, err := conn(ctx, gs.rh.DB()).QueryContext(ctx,
		`SELECT cl.commit_hash, COALESCE(cl.committed_at, 0)
		   FROM commit_log cl
		  WHERE cl.commit_hash IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("OutgoingAtCommit: commit_log lookup: %w", err)
	}
	defer clRows.Close()
	dates := make(map[string]int64, len(candidates))
	for clRows.Next() {
		var h string
		var ts int64
		if err := clRows.Scan(&h, &ts); err != nil {
			return nil, fmt.Errorf("OutgoingAtCommit: commit_log scan: %w", err)
		}
		dates[h] = ts
	}
	if err := clRows.Err(); err != nil {
		return nil, fmt.Errorf("OutgoingAtCommit: commit_log rows: %w", err)
	}

	out := make([]RefSummary, 0, len(candidates))
	for _, c := range candidates {
		ts, ok := dates[c.Commit]
		if !ok {
			continue
		}
		c.CommittedAt = ts
		out = append(out, c)
	}
	return out, nil
}
