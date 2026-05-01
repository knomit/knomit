package store

import (
	"context"
	"database/sql"
	"fmt"
)

// resolveTargetCommit walks ancestors of sourceCommit on the given branch
// looking for the FIRST commit_log row that touches refPath, and returns
// (commit_hash, ok=true) if that row's action is "added" or "modified".
// Returns ("", false, nil) if no ancestor touches refPath, or if the first
// ancestor that touches it has action="deleted" — both are skip-the-edge
// cases per design spec write-path step 2.
//
// The walk uses commit_log + branch_commits to constrain to the branch's
// ancestry; the result is the most recent commit on the branch with
// committed_at <= committed_at(sourceCommit) that touches refPath. This
// honours git topology by filtering through branch_commits, which is
// populated by walking parents from the branch ref.
func (si *searchIndex) resolveTargetCommit(ctx context.Context, branch, refPath, sourceCommit string) (string, bool, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return "", false, fmt.Errorf("resolveTargetCommit: branchID: %w", err)
	}

	var sourceCommittedAt int64
	err = conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT committed_at FROM commit_log WHERE commit_hash = ? LIMIT 1`,
		sourceCommit,
	).Scan(&sourceCommittedAt)
	if err == sql.ErrNoRows {
		// Source commit isn't in commit_log yet (caller passed an arg before
		// commit_log is populated, or the arg is invalid). No edges resolvable.
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolveTargetCommit: source committed_at: %w", err)
	}

	var targetCommit, action string
	err = conn(ctx, si.rh.db).QueryRowContext(ctx, `
		SELECT cl.commit_hash, cl.action
		FROM commit_log cl
		JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
		WHERE bc.branch_id = ?
		  AND cl.path = ?
		  AND cl.committed_at <= ?
		ORDER BY cl.committed_at DESC, cl.rowid DESC -- rowid is monotonic (no WITHOUT ROWID)
		LIMIT 1
	`, branchID, refPath, sourceCommittedAt).Scan(&targetCommit, &action)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolveTargetCommit: lookup: %w", err)
	}
	if action == "deleted" {
		return "", false, nil
	}
	return targetCommit, true, nil
}
