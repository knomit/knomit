package store

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	storegit "knomit/internal/store/git"
)

// Log returns log entries for commits that modified path (up to 50).
func (fi *factIndex) Log(ctx context.Context, branch, path string) ([]LogEntry, error) {
	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("log: ref: %w", err)
	}

	logIter, err := fi.rh.repo.Log(&gogit.LogOptions{
		From:     headHash,
		FileName: &path,
		Order:    gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("log: %w", err)
	}
	defer logIter.Close()

	var entries []LogEntry
	err = logIter.ForEach(func(c *object.Commit) error {
		if len(entries) >= 50 {
			return io.EOF
		}
		hash := c.Hash.String()
		fl := firstLine(c.Message)
		entries = append(entries, LogEntry{
			Commit:  hash,
			Date:    c.Committer.When.UTC().Format(time.RFC3339),
			Message: fl,
		})
		return nil
	})
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("log: iterate: %w", err)
	}

	return entries, nil
}

// LogPaginated returns log entries with pagination and tags.
// It returns (entries, next, prev, error) where next is a cursor for loading
// older commits and prev is a cursor for loading newer commits (empty = none).
func (fi *factIndex) LogPaginated(ctx context.Context, branch, path string, limit int, after, from, before string) ([]LogEntryWithTags, string, string, error) {
	if fi.rh.gits.CommitLogAvailable() {
		entries, next, prev, err := fi.logPaginatedSQL(ctx, path, limit, after, from, before)
		if err == nil {
			return entries, next, prev, nil
		}
	}
	entries, next, err := fi.logPaginatedGit(ctx, branch, path, limit, after)
	return entries, next, "", err
}

// logPaginatedSQL queries the commit_log table for paginated history.
func (fi *factIndex) logPaginatedSQL(ctx context.Context, path string, limit int, after, from, before string) ([]LogEntryWithTags, string, string, error) {
	var cursor storegit.CommitLogCursor
	switch {
	case before != "":
		cursor = storegit.CommitLogCursor{Type: storegit.CommitLogCursorBefore, Hash: before}
	case from != "":
		cursor = storegit.CommitLogCursor{Type: storegit.CommitLogCursorFrom, Hash: from}
	case after != "":
		cursor = storegit.CommitLogCursor{Type: storegit.CommitLogCursorAfter, Hash: after}
	}

	rows, hasMore, err := fi.rh.gits.CommitLogQuery(path, cursor, limit)
	if err != nil {
		return nil, "", "", fmt.Errorf("logPaginatedSQL: %w", err)
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
		fi.enrichFileCounts(entries)
	}
	return entries, nextCursor, prevCursor, nil
}

// logPaginatedGit is the go-git fallback for LogPaginated.
func (fi *factIndex) logPaginatedGit(ctx context.Context, branch, path string, limit int, after string) ([]LogEntryWithTags, string, error) {
	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return nil, "", fmt.Errorf("LogPaginated: ref: %w", err)
	}

	opts := &gogit.LogOptions{
		From:  headHash,
		Order: gogit.LogOrderCommitterTime,
	}
	if path != "" {
		if strings.HasSuffix(path, ".md") {
			opts.FileName = &path
		} else {
			prefix := path + "/"
			opts.PathFilter = func(p string) bool {
				return strings.HasPrefix(p, prefix)
			}
		}
	}

	logIter, err := fi.rh.repo.Log(opts)
	if err != nil {
		return nil, "", fmt.Errorf("LogPaginated: %w", err)
	}
	defer logIter.Close()

	skipping := after != ""
	afterHash := plumbing.NewHash(after)

	var entries []LogEntryWithTags
	var nextCursor string

	_ = logIter.ForEach(func(c *object.Commit) error {
		if skipping {
			if c.Hash == afterHash {
				skipping = false
			}
			return nil
		}

		if len(entries) >= limit {
			nextCursor = c.Hash.String()
			return io.EOF
		}

		hash := c.Hash.String()
		fl := firstLine(c.Message)

		entries = append(entries, LogEntryWithTags{
			Commit:    hash,
			Date:      c.Committer.When.UTC().Format(time.RFC3339),
			Message:   fl,
			Operation: parseOperation(c.Author.Email),
		})
		return nil
	})

	// Batch-fetch file change counts from commit_log if available.
	if fi.rh.gits.CommitLogAvailable() && len(entries) > 0 {
		fi.enrichFileCounts(entries)
	}

	return entries, nextCursor, nil
}

// enrichFileCounts batch-queries commit_log for A/M/D counts per commit.
func (fi *factIndex) enrichFileCounts(entries []LogEntryWithTags) {
	hashes := make([]string, len(entries))
	idx := make(map[string]int, len(entries))
	for i, e := range entries {
		hashes[i] = e.Commit
		idx[e.Commit] = i
	}

	counts, err := fi.rh.gits.CommitLogFileCounts(hashes)
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

// Activity computes commit-activity metrics for path using a SQL aggregate
// query when commit_log is available, or a capped go-git walk otherwise.
func (fi *factIndex) Activity(ctx context.Context, branch, path string) (ActivityResult, error) {
	if fi.rh.gits.CommitLogAvailable() {
		return fi.activitySQL(ctx, path)
	}
	return fi.activityGit(ctx, branch, path)
}

func (fi *factIndex) activitySQL(ctx context.Context, path string) (ActivityResult, error) {
	cutoff7 := commitLogAge(7)
	cutoff30 := commitLogAge(30)
	cutoff90 := commitLogAge(90)

	r, err := fi.rh.gits.CommitLogActivity(path, cutoff7, cutoff30, cutoff90)
	if err != nil {
		return ActivityResult{}, fmt.Errorf("activitySQL: %w", err)
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

func (fi *factIndex) activityGit(ctx context.Context, branch, path string) (ActivityResult, error) {
	const maxCommits = 500

	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return ActivityResult{}, fmt.Errorf("Activity: ref: %w", err)
	}

	opts := &gogit.LogOptions{
		From:  headHash,
		Order: gogit.LogOrderCommitterTime,
	}
	if path != "" {
		if strings.HasSuffix(path, ".md") {
			opts.FileName = &path
		} else {
			prefix := path + "/"
			opts.PathFilter = func(p string) bool { return strings.HasPrefix(p, prefix) }
		}
	}

	logIter, err := fi.rh.repo.Log(opts)
	if err != nil {
		return ActivityResult{}, fmt.Errorf("Activity: log: %w", err)
	}
	defer logIter.Close()

	now := time.Now()
	cutoff7 := now.AddDate(0, 0, -7)
	cutoff30 := now.AddDate(0, 0, -30)
	cutoff90 := now.AddDate(0, 0, -90)

	var result ActivityResult
	_ = logIter.ForEach(func(c *object.Commit) error {
		t := c.Committer.When
		if result.Total == 0 {
			result.LastCommit = t.UTC().Format(time.RFC3339)
		}
		result.Total++
		if t.After(cutoff7) {
			result.Changes7d++
		}
		if t.After(cutoff30) {
			result.Changes30d++
		}
		if t.After(cutoff90) {
			result.Changes90d++
		}
		if result.Total >= maxCommits {
			return io.EOF
		}
		return nil
	})
	return result, nil
}

// CommitDetail returns metadata and changed files for a specific commit.
func (fi *factIndex) CommitDetail(ctx context.Context, commitHash string) (*CommitDetailResult, error) {
	hash := plumbing.NewHash(commitHash)
	commit, err := fi.rh.repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("CommitDetail: commit: %w", err)
	}

	toTree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("CommitDetail: tree: %w", err)
	}

	var fromTree *object.Tree
	if commit.NumParents() > 0 {
		parent, err := commit.Parent(0)
		if err != nil {
			return nil, fmt.Errorf("CommitDetail: parent: %w", err)
		}
		fromTree, err = parent.Tree()
		if err != nil {
			return nil, fmt.Errorf("CommitDetail: parent tree: %w", err)
		}
	}

	changes, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return nil, fmt.Errorf("CommitDetail: diff: %w", err)
	}

	files := []ChangedFile{}
	for _, ch := range changes {
		from := ch.From.Name
		to := ch.To.Name
		switch {
		case from == "" && to != "":
			if strings.HasSuffix(to, ".md") {
				files = append(files, ChangedFile{Path: to, Action: "added"})
			}
		case from != "" && to == "":
			if strings.HasSuffix(from, ".md") {
				files = append(files, ChangedFile{Path: from, Action: "deleted"})
			}
		default:
			if strings.HasSuffix(to, ".md") {
				files = append(files, ChangedFile{Path: to, Action: "modified"})
			}
		}
	}

	return &CommitDetailResult{
		Commit:    hash.String(),
		Date:      commit.Committer.When.UTC().Format(time.RFC3339),
		Message:   firstLine(commit.Message),
		Operation: parseOperation(commit.Author.Email),
		Files:     files,
	}, nil
}

// WalkChangedFiles returns .md files under prefix most recently changed,
// excluding already-seen paths, up to limit results.
func (fi *factIndex) WalkChangedFiles(ctx context.Context, branch, fromCommit string, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error) {
	if fi.rh.gits.CommitLogAvailable() {
		return fi.walkChangedFilesSQL(ctx, branch, prefix, seen, limit)
	}
	return fi.walkChangedFilesGit(ctx, branch, fromCommit, prefix, seen, limit)
}

func (fi *factIndex) walkChangedFilesSQL(ctx context.Context, branch, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error) {
	rows, err := fi.rh.gits.CommitLogWalkChanged(prefix, seen, limit)
	if err != nil {
		return nil, "", fmt.Errorf("walkChangedFilesSQL: %w", err)
	}

	results := make([]FileRecency, 0, len(rows))
	for _, r := range rows {
		results = append(results, FileRecency{
			Path:      r.Path,
			Timestamp: time.Unix(r.UpdatedAt, 0).UTC(),
		})
	}

	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return results, "", nil
	}
	return results, headHash.String(), nil
}

func (fi *factIndex) walkChangedFilesGit(ctx context.Context, branch, fromCommit string, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error) {
	var from plumbing.Hash
	if fromCommit != "" {
		from = plumbing.NewHash(fromCommit)
	} else {
		headHash, err := fi.rh.resolveRef(ctx, branch)
		if err != nil {
			return nil, "", fmt.Errorf("walkChangedFiles: ref: %w", err)
		}
		from = headHash
	}

	logIter, err := fi.rh.repo.Log(&gogit.LogOptions{
		From:  from,
		Order: gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, "", fmt.Errorf("walkChangedFiles: log: %w", err)
	}
	defer logIter.Close()

	localSeen := make(map[string]bool, len(seen))
	for k, v := range seen {
		localSeen[k] = v
	}

	prefixDir := prefix + "/"
	var results []FileRecency
	var lastHash string

	err = logIter.ForEach(func(c *object.Commit) error {
		lastHash = c.Hash.String()

		toTree, err := c.Tree()
		if err != nil {
			return fmt.Errorf("tree: %w", err)
		}
		var fromTree *object.Tree
		if c.NumParents() > 0 {
			parent, err := c.Parent(0)
			if err != nil {
				return fmt.Errorf("parent: %w", err)
			}
			fromTree, err = parent.Tree()
			if err != nil {
				return fmt.Errorf("parent tree: %w", err)
			}
		}
		changes, err := object.DiffTree(fromTree, toTree)
		if err != nil {
			return fmt.Errorf("diff: %w", err)
		}
		for _, ch := range changes {
			path := ch.To.Name
			if path == "" {
				path = ch.From.Name
			}
			if !strings.HasSuffix(path, ".md") {
				continue
			}
			if prefix != "" && path != prefix+".md" && !strings.HasPrefix(path, prefixDir) {
				continue
			}
			if localSeen[path] {
				continue
			}
			localSeen[path] = true
			results = append(results, FileRecency{
				Path:      path,
				Timestamp: c.Committer.When.UTC(),
			})
			if len(results) >= limit {
				return io.EOF
			}
		}
		return nil
	})
	if err != nil && err != io.EOF {
		return nil, "", fmt.Errorf("walkChangedFiles: iterate: %w", err)
	}
	return results, lastHash, nil
}

// BranchInfo returns all branches partitioned into regular branches, agent
// branches (prefixed "agent/"), and the agent branch matching localAgent (if any).
func (fi *factIndex) BranchInfo(localAgent string) (branches, agentBranches []string, matchedAgent string) {
	refIter, err := fi.rh.gits.IterReferences()
	if err != nil {
		return
	}
	defer refIter.Close()

	agentSet := make(map[string]struct{})
	for {
		ref, err := refIter.Next()
		if err != nil {
			break
		}
		name := ref.Name().String()
		var short string
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			short = strings.TrimPrefix(name, "refs/heads/")
		case strings.HasPrefix(name, "refs/remotes/origin/"):
			short = strings.TrimPrefix(name, "refs/remotes/origin/")
		default:
			continue
		}
		if strings.HasPrefix(short, "agent/") {
			if _, seen := agentSet[short]; !seen {
				agentSet[short] = struct{}{}
				if short == localAgent {
					matchedAgent = short
				}
			}
		} else if strings.HasPrefix(name, "refs/heads/") {
			branches = append(branches, short)
		}
	}
	agentBranches = make([]string, 0, len(agentSet))
	for b := range agentSet {
		agentBranches = append(agentBranches, b)
	}
	return
}

// DiffFiles returns paths added/modified/deleted between fromCommit and the tip of branch.
// Only .md files are returned. If fromCommit is empty, diffs from empty tree.
func (fi *factIndex) DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error) {
	return fi.rh.DiffFiles(ctx, branch, fromCommit)
}

// parseOperation extracts the operation from an author email using the +tag subaddress convention.
// "agent+learn@agents.knomit.io" → "learn", "bob+learn@gmail.com" → "learn", "bob@gmail.com" → "".
func parseOperation(email string) string {
	plusIdx := strings.IndexByte(email, '+')
	if plusIdx < 0 {
		return ""
	}
	atIdx := strings.IndexByte(email, '@')
	if atIdx < 0 || atIdx < plusIdx {
		return ""
	}
	return email[plusIdx+1 : atIdx]
}

// firstLine returns the first line of s.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// changedFileEntry represents a file changed in a commit (internal to commitlog).
// Named changedFileEntry to avoid conflict with the exported ChangedFile in types.go.
type changedFileEntry struct {
	path   string
	action string // "added", "modified", "deleted"
}

// commitEntries converts changedFileEntry results from a commit into CommitLogEntry structs.
func commitEntries(c *object.Commit, files []changedFileEntry) []storegit.CommitLogEntry {
	hashStr := c.Hash.String()
	ts := c.Committer.When.Unix()
	msg := firstLine(c.Message)
	authorEmail := c.Author.Email
	op := parseOperation(authorEmail)

	entries := make([]storegit.CommitLogEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, storegit.CommitLogEntry{
			Hash:        hashStr,
			Path:        f.path,
			Message:     msg,
			Operation:   op,
			AuthorEmail: authorEmail,
			Action:      f.action,
			CommittedAt: ts,
		})
	}
	return entries
}

// changedFilesInCommit returns the .md files added/modified/deleted in c.
func changedFilesInCommit(c *object.Commit) ([]changedFileEntry, error) {
	toTree, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("changedFilesInCommit: tree: %w", err)
	}
	var fromTree *object.Tree
	if c.NumParents() > 0 {
		parent, err := c.Parent(0)
		if err != nil {
			return nil, fmt.Errorf("changedFilesInCommit: parent: %w", err)
		}
		fromTree, err = parent.Tree()
		if err != nil {
			return nil, fmt.Errorf("changedFilesInCommit: parent tree: %w", err)
		}
	}
	changes, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return nil, fmt.Errorf("changedFilesInCommit: diff: %w", err)
	}
	var files []changedFileEntry
	for _, ch := range changes {
		var path, action string
		switch {
		case ch.From.Name == "" && ch.To.Name != "":
			path, action = ch.To.Name, "added"
		case ch.From.Name != "" && ch.To.Name == "":
			path, action = ch.From.Name, "deleted"
		default:
			path, action = ch.To.Name, "modified"
		}
		if strings.HasSuffix(path, ".md") {
			files = append(files, changedFileEntry{path: strings.ToLower(path), action: action})
		}
	}
	return files, nil
}

// populateCommitLog backfills commit_log from the tip of branch.
// Commits are streamed directly from the iterator to CommitLogSync, which stops
// as soon as it encounters a hash already in the table (dedup / incremental update).
func (fi *factIndex) populateCommitLog(ctx context.Context, branch string) error {
	hash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		// Branch not found (empty repo) — just mark available if table exists.
		_ = fi.rh.gits.CommitLogAvailable()
		return nil
	}

	logIter, err := fi.rh.repo.Log(&gogit.LogOptions{
		From:  hash,
		Order: gogit.LogOrderDefault,
	})
	if err != nil {
		return fmt.Errorf("populateCommitLog: log: %w", err)
	}
	defer logIter.Close()

	var count int
	err = fi.rh.gits.CommitLogSync(branch, func() (string, []storegit.CommitLogEntry, error) {
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
// New commits always get the highest rowid, preserving recency ordering.
// Errors are logged and swallowed — commit_log is an index, not source of truth.
func (fi *factIndex) appendCommitLog(ctx context.Context, branch string, hash plumbing.Hash) {
	if !fi.rh.gits.CommitLogAvailable() {
		return
	}
	c, err := fi.rh.repo.CommitObject(hash)
	if err != nil {
		log.Warn().Err(err).Str("hash", hash.String()).Msg("commit_log: get commit")
		return
	}
	files, err := changedFilesInCommit(c)
	if err != nil {
		log.Warn().Err(err).Str("hash", hash.String()).Msg("commit_log: changed files")
		return
	}
	done := false
	entries := commitEntries(c, files)
	if err := fi.rh.gits.CommitLogSync(branch, func() (string, []storegit.CommitLogEntry, error) {
		if done {
			return "", nil, nil
		}
		done = true
		return hash.String(), entries, nil
	}); err != nil {
		log.Warn().Err(err).Str("hash", hash.String()).Msg("commit_log: append sync failed")
	}
}

// commitLogAge is used for SQL activity queries.
func commitLogAge(days int) int64 {
	return time.Now().Unix() - int64(days)*86400
}
