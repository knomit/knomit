package store

import (
	"context"
	"fmt"
	"io"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"

	storegit "knomit/internal/store/git"
)

// Log returns up to 50 log entries for commits that modified path on branch.
// Queries commit_log directly; branch is accepted for interface compatibility.
func (si *searchIndex) Log(ctx context.Context, branch, path string) ([]LogEntry, error) {
	rows, _, err := si.rh.commitLogQuery(ctx, branch, path, "", "", "", 50)
	if err != nil {
		return nil, fmt.Errorf("Log: %w", err)
	}
	entries := make([]LogEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, LogEntry{
			Commit:  r.Hash,
			Date:    time.Unix(r.Timestamp, 0).UTC().Format(time.RFC3339),
			Message: firstLine(r.Message),
		})
	}
	return entries, nil
}

// LogPaginated returns paginated log entries with file-count tags.
func (si *searchIndex) LogPaginated(ctx context.Context, branch, path string, limit int, after, from, before string) ([]LogEntryWithTags, string, string, error) {
	rows, hasMore, err := si.rh.commitLogQuery(ctx, branch, path, after, from, before, limit)
	if err != nil {
		return nil, "", "", fmt.Errorf("LogPaginated: %w", err)
	}

	entries := make([]LogEntryWithTags, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, LogEntryWithTags{
			Commit:    r.Hash,
			Date:      time.Unix(r.Timestamp, 0).UTC().Format(time.RFC3339),
			Message:   firstLine(r.Message),
			Operation: r.Operation,
		})
	}

	var nextCursor, prevCursor string
	switch {
	case before != "":
		if hasMore && len(entries) > 0 {
			prevCursor = entries[0].Commit
		}
	case from != "":
		if len(entries) > 0 {
			prevCursor = entries[0].Commit
		}
		if hasMore {
			nextCursor = entries[len(entries)-1].Commit
		}
	default:
		if hasMore {
			nextCursor = entries[len(entries)-1].Commit
		}
	}

	if len(entries) > 0 {
		si.enrichFileCounts(entries)
	}
	return entries, nextCursor, prevCursor, nil
}

// enrichFileCounts batch-queries commit_log for A/M/D counts per commit.
func (si *searchIndex) enrichFileCounts(entries []LogEntryWithTags) {
	hashes := make([]string, len(entries))
	idx := make(map[string]int, len(entries))
	for i, e := range entries {
		hashes[i] = e.Commit
		idx[e.Commit] = i
	}

	counts, err := si.rh.commitLogFileCounts(hashes)
	if err != nil {
		return
	}

	for hash, actionCounts := range counts {
		i, ok := idx[hash]
		if !ok {
			continue
		}
		entries[i].Files.Added = actionCounts["added"]
		entries[i].Files.Modified = actionCounts["modified"]
		entries[i].Files.Deleted = actionCounts["deleted"]
	}
}

// CommitDetail returns metadata and changed files for a specific commit,
// queried from commit_log.
func (si *searchIndex) CommitDetail(ctx context.Context, commitHash string) (*CommitDetailResult, error) {
	db := conn(ctx, si.rh.db)

	var committedAt int64
	var message, operation string
	err := db.QueryRowContext(ctx,
		`SELECT committed_at, message, operation FROM commit_log WHERE commit_hash = ? LIMIT 1`,
		commitHash,
	).Scan(&committedAt, &message, &operation)
	if err != nil {
		return nil, fmt.Errorf("CommitDetail: commit not found in history index: %s", commitHash)
	}

	fileRows, err := db.QueryContext(ctx,
		`SELECT DISTINCT path, action FROM commit_log WHERE commit_hash = ? ORDER BY path`,
		commitHash,
	)
	if err != nil {
		return nil, fmt.Errorf("CommitDetail: files: %w", err)
	}
	defer fileRows.Close()

	var files []ChangedFile
	for fileRows.Next() {
		var path, action string
		if err := fileRows.Scan(&path, &action); err != nil {
			return nil, fmt.Errorf("CommitDetail: scan: %w", err)
		}
		files = append(files, ChangedFile{Path: path, Action: action})
	}
	if files == nil {
		files = []ChangedFile{}
	}

	return &CommitDetailResult{
		Commit:    commitHash,
		Date:      time.Unix(committedAt, 0).UTC().Format(time.RFC3339),
		Message:   firstLine(message),
		Operation: operation,
		Files:     files,
	}, nil
}

// Activity returns commit-activity metrics for path using SQL aggregates.
func (si *searchIndex) Activity(ctx context.Context, branch, path string) (ActivityResult, error) {
	cutoff7 := commitLogAge(7)
	cutoff30 := commitLogAge(30)
	cutoff90 := commitLogAge(90)

	r, err := si.rh.commitLogActivity(ctx, branch, path, cutoff7, cutoff30, cutoff90)
	if err != nil {
		return ActivityResult{}, fmt.Errorf("Activity: %w", err)
	}

	var lastCommit string
	if r.LastCommit.Valid {
		lastCommit = time.Unix(r.LastCommit.Int64, 0).UTC().Format(time.RFC3339)
	}
	return ActivityResult{
		LastCommit: lastCommit,
		Total:      r.Total,
		Changes7d:  r.Changes7d,
		Changes30d: r.Changes30d,
		Changes90d: r.Changes90d,
	}, nil
}

// WalkChangedFiles returns .md files under prefix most recently changed,
// excluding already-seen paths, up to limit results.
func (si *searchIndex) WalkChangedFiles(ctx context.Context, branch, fromCommit, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error) {
	rows, err := si.rh.commitLogWalkChanged(ctx, branch, prefix, seen, limit)
	if err != nil {
		return nil, "", fmt.Errorf("WalkChangedFiles: %w", err)
	}

	results := make([]FileRecency, 0, len(rows))
	for _, r := range rows {
		results = append(results, FileRecency{
			Path:      r.Path,
			Timestamp: time.Unix(r.UpdatedAt, 0).UTC(),
		})
	}

	headHash, err := si.rh.HeadCommit(ctx, branch)
	if err != nil {
		return results, "", nil
	}
	return results, headHash, nil
}

// FactsIter opens a cursor over facts for the given branch ordered by fact_id DESC.
// The caller must call Close() when done to release the underlying database cursor.
func (si *searchIndex) FactsIter(ctx context.Context, branch string) (*FactsIter, error) {
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return nil, err
	}
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx,
		`SELECT bf.path, f.blob_hash, bf.commit_hash
		 FROM branch_facts bf
		 JOIN facts f ON f.id = bf.fact_id
		 WHERE bf.branch_id = ?
		 ORDER BY bf.fact_id DESC`,
		branchID,
	)
	if err != nil {
		return nil, err
	}
	return &FactsIter{rows: rows, seen: make(map[string]struct{})}, nil
}

// populateCommitLog backfills commit_log from the tip of branch.
func (si *searchIndex) populateCommitLog(ctx context.Context, branch string) error {
	hash, err := si.rh.resolveRef(ctx, branch)
	if err != nil {
		// Branch not found (empty repo) — just mark available if table exists.
		_ = si.rh.gits.CommitLogAvailable()
		return nil
	}

	logIter, err := si.rh.repo.Log(&gogit.LogOptions{
		From:  hash,
		Order: gogit.LogOrderDefault,
	})
	if err != nil {
		return fmt.Errorf("populateCommitLog: log: %w", err)
	}
	defer logIter.Close()

	var count int
	err = si.rh.gits.CommitLogSync(branch, func() (string, []storegit.CommitLogEntry, error) {
		c, err := logIter.Next()
		if err == io.EOF {
			return "", nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		count++
		files, err := changedFilesInCommit(c)
		if err != nil {
			return "", nil, err
		}
		return c.Hash.String(), commitEntries(c, files), nil
	})
	if err != nil {
		return fmt.Errorf("populateCommitLog: sync: %w", err)
	}

	log.Debug().Int("commits", count).Msg("commit_log: populated")
	return nil
}

// appendCommitLog inserts a single new commit into commit_log.
func (si *searchIndex) appendCommitLog(ctx context.Context, branch, hashStr string) {
	if !si.rh.gits.CommitLogAvailable() {
		return
	}
	hash := plumbing.NewHash(hashStr)
	c, err := si.rh.repo.CommitObject(hash)
	if err != nil {
		log.Warn().Err(err).Str("hash", hashStr).Msg("commit_log: get commit")
		return
	}
	files, err := changedFilesInCommit(c)
	if err != nil {
		log.Warn().Err(err).Str("hash", hashStr).Msg("commit_log: changed files")
		return
	}
	done := false
	entries := commitEntries(c, files)
	if err := si.rh.gits.CommitLogSync(branch, func() (string, []storegit.CommitLogEntry, error) {
		if done {
			return "", nil, nil
		}
		done = true
		return hash.String(), entries, nil
	}); err != nil {
		log.Warn().Err(err).Str("hash", hash.String()).Msg("commit_log: append sync failed")
	}
}
