package index

import (
	"context"
	"fmt"
	"strings"
)

// IncomingAtCommit returns the set of (source_path, source_commit) tuples
// that asserted a ref to (path, commitHash) on the given branch. Walks
// DERIVED_FROM edges INTO this Fact node where the edge's target_commit
// property equals commitHash, then SQL-filters source_commit to be
// reachable on the requested branch.
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
	found, err := graphDerivedFromNeighbours(ctx, conn(ctx, gs.rh.DB()), path, blobHash, true)
	if err != nil {
		return nil, fmt.Errorf("IncomingAtCommit: read edges: %w", err)
	}
	candidates := make([]RefSummary, 0, len(found))
	for _, rs := range found {
		// Defense-in-depth: stale self-loops (same path & same commit on
		// both endpoints) navigate to nowhere; drop them. Legitimate
		// "previous version" edges have differing commits and pass through.
		if rs.Path == path && rs.Commit == commitHash {
			continue
		}
		candidates = append(candidates, rs)
	}

	// 2. SQL post-filter: keep only edges whose source_commit is reachable
	// on this branch.
	if len(candidates) == 0 {
		return candidates, nil
	}
	args := make([]any, 0, len(candidates)+1)
	args = append(args, branchID)
	placeholders := make([]string, 0, len(candidates))
	for _, c := range candidates {
		args = append(args, c.Commit)
		placeholders = append(placeholders, "?")
	}
	bcRows, err := conn(ctx, gs.rh.DB()).QueryContext(ctx,
		`SELECT bc.commit_hash, COALESCE(cl.committed_at, 0)
		   FROM branch_commits bc
		   LEFT JOIN commit_log cl ON cl.commit_hash = bc.commit_hash
		  WHERE bc.branch_id = ? AND bc.commit_hash IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("IncomingAtCommit: branch filter: %w", err)
	}
	defer bcRows.Close()
	dates := make(map[string]int64, len(candidates))
	for bcRows.Next() {
		var h string
		var ts int64
		if err := bcRows.Scan(&h, &ts); err != nil {
			return nil, fmt.Errorf("IncomingAtCommit: branch filter scan: %w", err)
		}
		dates[h] = ts
	}
	if err := bcRows.Err(); err != nil {
		return nil, fmt.Errorf("IncomingAtCommit: branch filter rows: %w", err)
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
	return gs.outgoingOfVersion(ctx, path, commitHash, blobHash)
}

// OutgoingAtCommits is OutgoingAtCommit for many (path, commit) pairs, the
// same answer for each (an error for one is the call's error, as a loop
// would return it). It resolves every pair's effective commit as
// OutgoingAtCommit does, then every blob in ONE tree pass
// (vcs.Repo.BlobHashesAtCommits), where OutgoingAtCommit decodes the commit
// and trees again per pair; a pair the pass cannot answer goes through
// OutgoingAtCommit itself, so its answer (or error) is unchanged. The edges
// and their dates are read per pair by the same code, in the same order.
func (gs *GraphStore) OutgoingAtCommits(ctx context.Context, branch string, pairs []PathAtCommit) (map[PathAtCommit][]RefSummary, error) {
	gs.probe("OutgoingAtCommits")
	out := make(map[PathAtCommit][]RefSummary, len(pairs))
	effective := make(map[PathAtCommit]PathAtCommit, len(pairs))
	var blobPairs []PathAtCommit
	for _, p := range pairs {
		if _, done := effective[p]; done {
			continue
		}
		eff, ok, err := gs.rh.ResolveActiveCommitForPath(ctx, branch, p.Path, p.Commit)
		if err != nil {
			return nil, fmt.Errorf("OutgoingAtCommit: resolve effective commit: %w", err)
		}
		if !ok {
			effective[p] = PathAtCommit{}
			out[p] = nil
			continue
		}
		e := PathAtCommit{Path: p.Path, Commit: eff}
		effective[p] = e
		blobPairs = append(blobPairs, e)
	}
	blobs, err := gs.rh.BlobHashesAtCommits(ctx, blobPairs)
	if err != nil {
		return nil, fmt.Errorf("OutgoingAtCommits: %w", err)
	}
	for p, e := range effective {
		if e.Commit == "" {
			continue
		}
		blob, ok := blobs[e]
		var res []RefSummary
		if ok {
			res, err = gs.outgoingOfVersion(ctx, p.Path, p.Commit, blob)
		} else {
			res, err = gs.OutgoingAtCommit(ctx, branch, p.Path, p.Commit)
		}
		if err != nil {
			return nil, err
		}
		out[p] = res
	}
	return out, nil
}

// outgoingOfVersion is OutgoingAtCommit once the version's blob is known:
// its DERIVED_FROM edges, minus stale self-loops, with their target commits'
// dates; an edge whose target commit has no commit_log row is dropped.
func (gs *GraphStore) outgoingOfVersion(ctx context.Context, path, commitHash, blobHash string) ([]RefSummary, error) {
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
