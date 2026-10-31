package vcs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	storegit "knomit/internal/engine/store/git"
)

// LiveVersionAt is the version of path live at commit x on the branch with
// branchID, read from the derived history alone (no git object is decoded):
//   - commit is the change live at x AS THE ENTRY IT RESOLVES TO (a merge
//     carrying the content resolves to the commit that wrote it), exactly as
//     index.recordedCommit records branch_facts.commit_hash, and blob is that
//     change's blob;
//   - live is false when there is no such change, or when the path was
//     DELETED on x's first-parent line after that change (a commit_log
//     'deleted' row deeper than the change and on the line, visible on the
//     branch). A deletion followed by a re-add is live: the re-add is a newer
//     change.
//
// This is the historical form of "what branch_facts held at x": the commit is
// the one that holds the blob (recordedCommit), never the earliest
// introduction. x must be derived (every indexed commit is); an underived x
// answers not live.
func LiveVersionAt(ctx context.Context, q storegit.CtxExecer, t ChangeTables, path, x string, branchID int64) (commit, blob string, live bool, err error) {
	entry, row, err := LiveChange(ctx, q, t, path, x, branchID)
	if err != nil || row == "" {
		return "", "", false, err
	}
	var rowDepth int
	err = q.QueryRowContext(ctx, `SELECT blob, fp_depth FROM `+t.Changes+` WHERE path = ? AND commit_hash = ?`, path, row).
		Scan(&blob, &rowDepth)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("live version: row: %w", err)
	}
	var xDepth int
	if err := q.QueryRowContext(ctx, `SELECT depth FROM `+t.FP+` WHERE commit_hash = ?`, x).Scan(&xDepth); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("live version: depth: %w", err)
	}
	join := ""
	if branchID != 0 {
		join = ` JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash AND bc.branch_id = ` + fmt.Sprint(branchID)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT cl.commit_hash, fp.depth FROM commit_log cl
		  JOIN `+t.FP+` fp ON fp.commit_hash = cl.commit_hash`+join+`
		 WHERE cl.path = ? AND cl.action = 'deleted' AND fp.depth > ? AND fp.depth <= ?
		 ORDER BY fp.depth DESC, cl.commit_hash DESC`, path, rowDepth, xDepth)
	if err != nil {
		return "", "", false, fmt.Errorf("live version: deletions: %w", err)
	}
	type del struct {
		commit string
		depth  int
	}
	var dels []del
	for rows.Next() {
		var d del
		if err := rows.Scan(&d.commit, &d.depth); err != nil {
			rows.Close()
			return "", "", false, err
		}
		dels = append(dels, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", false, err
	}
	for _, d := range dels {
		on, err := onFirstParentLine(ctx, q, t, d.commit, d.depth, x, xDepth)
		if err != nil {
			return "", "", false, err
		}
		if on {
			return entry, blob, false, nil
		}
	}
	return entry, blob, true, nil
}
