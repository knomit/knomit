package store

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
func (si *searchIndex) IncomingAtCommit(ctx context.Context, branch, path, commitHash string) ([]RefSummary, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("IncomingAtCommit: branchID: %w", err)
	}

	// 1. Cypher: candidate (source_path, source_title, source_commit, source_deleted) rows.
	// Note: "commit" is a reserved SQL keyword; use alias "sc" (source commit).
	// Retracted sources are returned with deleted=true so the UI can render
	// them with the retracted treatment instead of hiding the edge entirely
	// (mirrors how OutgoingAtCommit exposes retracted targets).
	cypherQ := fmt.Sprintf(
		`MATCH (s:%s)-[r:%s]->(t:%s {path: "%s"}) WHERE r.target_commit = "%s" RETURN s.path AS path, s.title AS title, s.type AS type, r.source_commit AS sc, s.deleted AS deleted`,
		NodeFact, EdgeDerivedFrom, NodeFact,
		escapeCypherKey(path), escapeCypherKey(commitHash),
	)
	q := `SELECT json_extract(value, '$.path'), json_extract(value, '$.title'), json_extract(value, '$.type'), json_extract(value, '$.sc'), json_extract(value, '$.deleted') FROM json_each(cypher('` + cypherQ + `'))`
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("IncomingAtCommit: cypher: %w", err)
	}
	defer rows.Close()

	var candidates []RefSummary
	for rows.Next() {
		var rs RefSummary
		var del any
		if err := rows.Scan(&rs.Path, &rs.Title, &rs.Type, &rs.Commit, &del); err != nil {
			return nil, fmt.Errorf("IncomingAtCommit: scan: %w", err)
		}
		if rs.Path == "" {
			continue
		}
		// Defense-in-depth: stale self-loops (same path & same commit on
		// both endpoints) navigate to nowhere; drop them. Legitimate
		// "previous version" edges have differing commits and pass through.
		if rs.Path == path && rs.Commit == commitHash {
			continue
		}
		rs.Deleted = isDeletedVal(del)
		candidates = append(candidates, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("IncomingAtCommit: rows: %w", err)
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
	bcRows, err := conn(ctx, si.rh.db).QueryContext(ctx,
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
		if ts, ok := dates[c.Commit]; ok {
			c.CommittedAt = ts
			out = append(out, c)
		}
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
func (si *searchIndex) OutgoingAtCommit(ctx context.Context, branch, path, commitHash string) ([]RefSummary, error) {
	_ = branch
	cypherQ := fmt.Sprintf(
		`MATCH (s:%s {path: "%s"})-[r:%s]->(t:%s) WHERE r.source_commit = "%s" RETURN t.path AS path, t.title AS title, t.type AS type, r.target_commit AS tc, t.deleted AS deleted`,
		NodeFact, escapeCypherKey(path), EdgeDerivedFrom, NodeFact,
		escapeCypherKey(commitHash),
	)
	q := `SELECT json_extract(value, '$.path'), json_extract(value, '$.title'), json_extract(value, '$.type'), json_extract(value, '$.tc'), json_extract(value, '$.deleted') FROM json_each(cypher('` + cypherQ + `'))`
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("OutgoingAtCommit: cypher: %w", err)
	}
	defer rows.Close()

	var candidates []RefSummary
	for rows.Next() {
		var rs RefSummary
		var del any
		if err := rows.Scan(&rs.Path, &rs.Title, &rs.Type, &rs.Commit, &del); err != nil {
			return nil, fmt.Errorf("OutgoingAtCommit: scan: %w", err)
		}
		if rs.Path == "" {
			continue
		}
		// Defense-in-depth: drop stale self-loops where both endpoints
		// are the same (path, commit). resolveTargetCommit no longer
		// produces these, but legacy data may still hold them.
		if rs.Path == path && rs.Commit == commitHash {
			continue
		}
		rs.Deleted = isDeletedVal(del)
		candidates = append(candidates, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("OutgoingAtCommit: rows: %w", err)
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
	clRows, err := conn(ctx, si.rh.db).QueryContext(ctx,
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
