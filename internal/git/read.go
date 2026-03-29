// Read-only operations on the git store: file reads, directory listings, log,
// grep, and diffing. None of these methods modify the repository.
package git

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	storegit "knomit/internal/store/git"
)

// ReadFileWithHash returns both the file content and the blob hash for the given path.
func (s *Store) ReadFileWithHash(branch, path string) (string, string, error) {
	path = strings.ToLower(path)
	headHash, err := s.resolveRef(branch)
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: ref: %w", err)
	}
	commit, err := s.repo.CommitObject(headHash)
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: tree: %w", err)
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: entry %s: %w", path, err)
	}
	blob, err := s.repo.BlobObject(entry.Hash)
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: blob: %w", err)
	}
	r, err := blob.Reader()
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: reader: %w", err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: read: %w", err)
	}
	return string(b), entry.Hash.String(), nil
}

// readFileAtCommitHash reads the content of path from a specific commit.
// If the exact path is not found, it falls back to a case-insensitive tree
// walk so that normalised (lowercase) index paths resolve correctly against
// pre-normalisation commits that stored paths with mixed case.
func (s *Store) readFileAtCommitHash(path, commitHash string) (string, error) {
	hash := plumbing.NewHash(commitHash)
	commit, err := s.repo.CommitObject(hash)
	if err != nil {
		return "", fmt.Errorf("readFileAtCommitHash: commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("readFileAtCommitHash: tree: %w", err)
	}
	if f, err := tree.File(path); err == nil {
		return f.Contents()
	}
	// Exact lookup failed — try case-insensitive walk.
	content, err := treeFileInsensitive(s.repo, tree, path)
	if err != nil {
		return "", fmt.Errorf("readFileAtCommitHash: file %q not found (case-insensitive): %w", path, err)
	}
	return content, nil
}

// ReadFileAtCommit reads the content of path at the given commit.
// branch is accepted for interface consistency; the commit hash uniquely
// identifies the version without branch resolution.
func (s *Store) ReadFileAtCommit(branch, path, commitHash string) (string, error) {
	return s.readFileAtCommitHash(path, commitHash)
}

// treeFileInsensitive walks a git tree matching each path component
// case-insensitively and returns the file contents.
func treeFileInsensitive(repo *gogit.Repository, tree *object.Tree, path string) (string, error) {
	parts := strings.Split(path, "/")
	cur := tree
	for i, part := range parts {
		lower := strings.ToLower(part)
		var matched *object.TreeEntry
		for j := range cur.Entries {
			if strings.ToLower(cur.Entries[j].Name) == lower {
				matched = &cur.Entries[j]
				break
			}
		}
		if matched == nil {
			return "", fmt.Errorf("component %q not found", part)
		}
		if i == len(parts)-1 {
			blob, err := repo.BlobObject(matched.Hash)
			if err != nil {
				return "", err
			}
			r, err := blob.Reader()
			if err != nil {
				return "", err
			}
			defer r.Close()
			b, err := io.ReadAll(r)
			return string(b), err
		}
		sub, err := repo.TreeObject(matched.Hash)
		if err != nil {
			return "", fmt.Errorf("subtree %q: %w", part, err)
		}
		cur = sub
	}
	return "", fmt.Errorf("empty path")
}

// ReadFileLastCommit finds the most recent ancestor of beforeCommitHash where
// path existed and returns its content and commit hash. Used to read facts
// that were deleted in beforeCommitHash (e.g. retract commits).
func (s *Store) ReadFileLastCommit(branch, path, beforeCommitHash string) (content string, fromCommit string, err error) {
	path = strings.ToLower(path)
	startHash := plumbing.NewHash(beforeCommitHash)
	startCommit, err := s.repo.CommitObject(startHash)
	if err != nil {
		return "", "", fmt.Errorf("ReadFileLastCommit: commit: %w", err)
	}
	if len(startCommit.ParentHashes) == 0 {
		return "", "", fmt.Errorf("ReadFileLastCommit: %q: commit has no parents", path)
	}

	logIter, err := s.repo.Log(&gogit.LogOptions{
		From:     startCommit.ParentHashes[0],
		FileName: &path,
		Order:    gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return "", "", fmt.Errorf("ReadFileLastCommit: log: %w", err)
	}
	defer logIter.Close()

	lastCommit, err := logIter.Next()
	if err != nil {
		return "", "", fmt.Errorf("ReadFileLastCommit: %q: no prior commit found", path)
	}

	content, err = s.readFileAtCommitHash(path, lastCommit.Hash.String())
	return content, lastCommit.Hash.String(), err
}

// ReadFile reads the content of path from the tip of branch.
func (s *Store) ReadFile(branch, path string) (string, error) {
	content, _, err := s.ReadFileWithHash(branch, path)
	return content, err
}

// FileExists returns true if path exists at the tip of branch, false+nil if not found.
func (s *Store) FileExists(branch, path string) (bool, error) {
	headHash, err := s.resolveRef(branch)
	if err != nil {
		return false, fmt.Errorf("FileExists: ref: %w", err)
	}

	commit, err := s.repo.CommitObject(headHash)
	if err != nil {
		return false, fmt.Errorf("FileExists: commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return false, fmt.Errorf("FileExists: tree: %w", err)
	}

	_, err = tree.FindEntry(path)
	if err == object.ErrEntryNotFound || err == object.ErrDirectoryNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("FileExists: find entry: %w", err)
	}
	return true, nil
}

// ListDir returns entries under path at the tip of branch.
// Subdirectories have IsDir=true, .md files have IsDir=false.
func (s *Store) ListDir(branch, path string) ([]DirEntry, error) {
	path = strings.ToLower(path)
	headHash, err := s.resolveRef(branch)
	if err != nil {
		return nil, fmt.Errorf("ListDir: ref: %w", err)
	}

	commit, err := s.repo.CommitObject(headHash)
	if err != nil {
		return nil, fmt.Errorf("ListDir: commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("ListDir: tree: %w", err)
	}

	// Navigate to the subtree at path (use root tree directly when path is empty).
	var subtree *object.Tree
	if path == "" {
		subtree = tree
	} else {
		subtree, err = tree.Tree(path)
		if err != nil {
			return nil, fmt.Errorf("ListDir: subtree %q: %w", path, err)
		}
	}

	var entries []DirEntry
	for _, e := range subtree.Entries {
		if e.Mode == filemode.Dir {
			entries = append(entries, DirEntry{Name: e.Name, IsDir: true})
		} else if strings.HasSuffix(e.Name, ".md") {
			entries = append(entries, DirEntry{Name: e.Name, IsDir: false})
		}
		// Omit non-.md files
	}
	return entries, nil
}

// LastCommitForPath returns the hash of the most recent non-merge commit
// that touched path. Merges are skipped because they duplicate authoring
// commits from the merged branch.
func (s *Store) LastCommitForPath(branch, path string) (string, error) {
	headHash, err := s.resolveRef(branch)
	if err != nil {
		return "", fmt.Errorf("LastCommitForPath: ref: %w", err)
	}

	logIter, err := s.repo.Log(&gogit.LogOptions{
		From:     headHash,
		FileName: &path,
		Order:    gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return "", fmt.Errorf("LastCommitForPath: log: %w", err)
	}
	defer logIter.Close()

	for {
		c, err := logIter.Next()
		if err != nil {
			return "", fmt.Errorf("LastCommitForPath: %q: no commit found", path)
		}
		// Skip merge commits (more than one parent).
		if c.NumParents() <= 1 {
			return c.Hash.String(), nil
		}
	}
}

// pathHashSorter sorts two parallel slices (paths and hashes) together by path.
type pathHashSorter struct{ paths, hashes []string }

func (s pathHashSorter) Len() int           { return len(s.paths) }
func (s pathHashSorter) Less(i, j int) bool { return s.paths[i] < s.paths[j] }
func (s pathHashSorter) Swap(i, j int) {
	s.paths[i], s.paths[j] = s.paths[j], s.paths[i]
	s.hashes[i], s.hashes[j] = s.hashes[j], s.hashes[i]
}

// ListAllWithHash returns all .md files at the tip of branch with their blob hashes.
// Single tree walk — no per-file I/O.
func (s *Store) ListAllWithHash(branch string) ([]string, []string, error) {
	headHash, err := s.resolveRef(branch)
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: ref: %w", err)
	}

	commit, err := s.repo.CommitObject(headHash)
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: commit: %w", err)
	}

	fileIter, err := commit.Files()
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: files: %w", err)
	}
	defer fileIter.Close()

	var paths, blobHashes []string
	err = fileIter.ForEach(func(f *object.File) error {
		if strings.HasSuffix(f.Name, ".md") {
			paths = append(paths, f.Name)
			blobHashes = append(blobHashes, f.Hash.String())
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: iterate: %w", err)
	}

	sort.Sort(pathHashSorter{paths, blobHashes})
	return paths, blobHashes, nil
}

// ListAll returns paths of all .md files at the tip of branch.
func (s *Store) ListAll(branch string) ([]string, error) {
	paths, _, err := s.ListAllWithHash(branch)
	return paths, err
}

// Log returns log entries for commits that modified path (up to 50).
func (s *Store) Log(branch, path string) ([]LogEntry, error) {
	headHash, err := s.resolveRef(branch)
	if err != nil {
		return nil, fmt.Errorf("Log: ref: %w", err)
	}

	logIter, err := s.repo.Log(&gogit.LogOptions{
		From:     headHash,
		FileName: &path,
		Order:    gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("Log: %w", err)
	}
	defer logIter.Close()

	var entries []LogEntry
	err = logIter.ForEach(func(c *object.Commit) error {
		if len(entries) >= 50 {
			return io.EOF
		}
		hash := c.Hash.String()
		if len(hash) > 8 {
			hash = hash[:8]
		}
		firstLine := c.Message
		if idx := strings.IndexByte(firstLine, '\n'); idx >= 0 {
			firstLine = firstLine[:idx]
		}
		entries = append(entries, LogEntry{
			Commit:  hash,
			Date:    c.Committer.When.UTC().Format(time.RFC3339),
			Message: firstLine,
		})
		return nil
	})
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("Log: iterate: %w", err)
	}

	return entries, nil
}

// LogPaginated returns log entries with pagination and tags.
// It returns (entries, next, prev, error) where next is a cursor for loading
// older commits and prev is a cursor for loading newer commits (empty = none).
//
//   - from   (inclusive seek): result window starts at that commit on page 1.
//   - after  (exclusive, older): normal down-scroll pagination.
//   - before (exclusive, newer): up-scroll pagination — returns commits
//     strictly newer than the cursor, newest-first, up to limit.
//
// When commit_log is available the query is SQL-based (supports merge commits
// that go-git's PathFilter excludes). Falls back to go-git walk otherwise.
func (s *Store) LogPaginated(branch, path string, limit int, after, from, before string) ([]LogEntryWithTags, string, string, error) {
	if s.storer.CommitLogAvailable() {
		entries, next, prev, err := s.logPaginatedSQL(path, limit, after, from, before)
		if err == nil {
			return entries, next, prev, nil
		}
	}
	entries, next, err := s.logPaginatedGit(branch, path, limit, after)
	return entries, next, "", err
}

// logPaginatedSQL queries the commit_log table for paginated history.
// Returns (entries, next, prev, error).
func (s *Store) logPaginatedSQL(path string, limit int, after, from, before string) ([]LogEntryWithTags, string, string, error) {
	var cursor storegit.CommitLogCursor
	switch {
	case before != "":
		cursor = storegit.CommitLogCursor{Type: storegit.CommitLogCursorBefore, Hash: before}
	case from != "":
		cursor = storegit.CommitLogCursor{Type: storegit.CommitLogCursorFrom, Hash: from}
	case after != "":
		cursor = storegit.CommitLogCursor{Type: storegit.CommitLogCursorAfter, Hash: after}
	}

	rows, hasMore, err := s.storer.CommitLogQuery(path, cursor, limit)
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
		s.enrichFileCounts(entries)
	}
	return entries, nextCursor, prevCursor, nil
}

// logPaginatedGit is the go-git fallback for LogPaginated.
// Note: go-git's PathFilter may exclude merge commits from directory results.
func (s *Store) logPaginatedGit(branch, path string, limit int, after string) ([]LogEntryWithTags, string, error) {
	headHash, err := s.resolveRef(branch)
	if err != nil {
		return nil, "", fmt.Errorf("LogPaginated: ref: %w", err)
	}

	opts := &gogit.LogOptions{
		From:  headHash,
		Order: gogit.LogOrderCommitterTime,
	}
	if path != "" {
		if strings.HasSuffix(path, ".md") {
			// Specific file: exact match.
			opts.FileName = &path
		} else {
			// Directory: prefix match using PathFilter.
			prefix := path + "/"
			opts.PathFilter = func(p string) bool {
				return strings.HasPrefix(p, prefix)
			}
		}
	}

	logIter, err := s.repo.Log(opts)
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
		firstLine := c.Message
		if idx := strings.IndexByte(firstLine, '\n'); idx >= 0 {
			firstLine = firstLine[:idx]
		}

		entries = append(entries, LogEntryWithTags{
			Commit:    hash,
			Date:      c.Committer.When.UTC().Format(time.RFC3339),
			Message:   firstLine,
			Operation: parseOperation(c.Author.Email),
		})
		return nil
	})

	// Batch-fetch file change counts from commit_log if available.
	if s.storer.CommitLogAvailable() && len(entries) > 0 {
		s.enrichFileCounts(entries)
	}

	return entries, nextCursor, nil
}

// enrichFileCounts batch-queries commit_log for A/M/D counts per commit.
func (s *Store) enrichFileCounts(entries []LogEntryWithTags) {
	hashes := make([]string, len(entries))
	idx := make(map[string]int, len(entries))
	for i, e := range entries {
		hashes[i] = e.Commit
		idx[e.Commit] = i
	}

	counts, err := s.storer.CommitLogFileCounts(hashes)
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
// path may be a directory prefix or a specific .md file.
func (s *Store) Activity(branch, path string) (ActivityResult, error) {
	if s.storer.CommitLogAvailable() {
		return s.activitySQL(path)
	}
	return s.activityGit(branch, path)
}

func (s *Store) activitySQL(path string) (ActivityResult, error) {
	cutoff7 := commitLogAge(7)
	cutoff30 := commitLogAge(30)
	cutoff90 := commitLogAge(90)

	r, err := s.storer.CommitLogActivity(path, cutoff7, cutoff30, cutoff90)
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

func (s *Store) activityGit(branch, path string) (ActivityResult, error) {
	const maxCommits = 500

	headHash, err := s.resolveRef(branch)
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

	logIter, err := s.repo.Log(opts)
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
// It diffs the commit's tree against its parent to determine which files changed.
func (s *Store) CommitDetail(commitHash string) (*CommitDetailResult, error) {
	hash := plumbing.NewHash(commitHash)
	commit, err := s.repo.CommitObject(hash)
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
// Uses a SQL aggregate query when commit_log is available, or a full git walk
// otherwise. Returns files ordered most-recently-changed first, and the HEAD
// commit hash for session compatibility (SQL path ignores fromCommit).
func (s *Store) WalkChangedFiles(branch, fromCommit string, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error) {
	if s.storer.CommitLogAvailable() {
		return s.walkChangedFilesSQL(branch, prefix, seen, limit)
	}
	return s.walkChangedFilesGit(branch, fromCommit, prefix, seen, limit)
}

func (s *Store) walkChangedFilesSQL(branch, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error) {
	rows, err := s.storer.CommitLogWalkChanged(prefix, seen, limit)
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

	// Return HEAD hash for session compatibility (fromCommit is ignored in SQL path).
	headHash, err := s.resolveRef(branch)
	if err != nil {
		return results, "", nil
	}
	return results, headHash.String(), nil
}

func (s *Store) walkChangedFilesGit(branch, fromCommit string, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error) {
	var from plumbing.Hash
	if fromCommit != "" {
		from = plumbing.NewHash(fromCommit)
	} else {
		headHash, err := s.resolveRef(branch)
		if err != nil {
			return nil, "", fmt.Errorf("WalkChangedFiles: ref: %w", err)
		}
		from = headHash
	}

	logIter, err := s.repo.Log(&gogit.LogOptions{
		From:  from,
		Order: gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, "", fmt.Errorf("WalkChangedFiles: log: %w", err)
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
		return nil, "", fmt.Errorf("WalkChangedFiles: iterate: %w", err)
	}
	return results, lastHash, nil
}

// Grep searches all .md files at the tip of branch for pattern, returns matching paths.
func (s *Store) Grep(branch, pattern string) ([]string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("Grep: compile pattern: %w", err)
	}

	headHash, err := s.resolveRef(branch)
	if err != nil {
		return nil, fmt.Errorf("Grep: ref: %w", err)
	}

	commit, err := s.repo.CommitObject(headHash)
	if err != nil {
		return nil, fmt.Errorf("Grep: commit: %w", err)
	}

	fileIter, err := commit.Files()
	if err != nil {
		return nil, fmt.Errorf("Grep: files: %w", err)
	}
	defer fileIter.Close()

	var matches []string
	err = fileIter.ForEach(func(f *object.File) error {
		if !strings.HasSuffix(f.Name, ".md") {
			return nil
		}
		content, err := f.Contents()
		if err != nil {
			return fmt.Errorf("Grep: read %q: %w", f.Name, err)
		}
		if re.MatchString(content) {
			matches = append(matches, f.Name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("Grep: iterate: %w", err)
	}

	sort.Strings(matches)
	return matches, nil
}

// DiffFiles returns paths added/modified/deleted between fromCommit and the tip of branch.
// Only .md files are returned. If fromCommit is empty, diffs from empty tree.
func (s *Store) DiffFiles(branch, fromCommit string) (added, modified, deleted []string, err error) {
	headHash, err := s.resolveRef(branch)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: ref: %w", err)
	}

	toCommit, err := s.repo.CommitObject(headHash)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: to commit: %w", err)
	}
	toTree, err := toCommit.Tree()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: to tree: %w", err)
	}

	var fromTree *object.Tree
	if fromCommit != "" {
		fromHash := plumbing.NewHash(fromCommit)
		fc, err := s.repo.CommitObject(fromHash)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("DiffFiles: from commit: %w", err)
		}
		fromTree, err = fc.Tree()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("DiffFiles: from tree: %w", err)
		}
	}

	changes, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: diff tree: %w", err)
	}

	for _, ch := range changes {
		from := ch.From.Name
		to := ch.To.Name

		switch {
		case from == "" && to != "":
			// Added
			if strings.HasSuffix(to, ".md") {
				added = append(added, to)
			}
		case from != "" && to == "":
			// Deleted
			if strings.HasSuffix(from, ".md") {
				deleted = append(deleted, from)
			}
		default:
			// Modified (or renamed — treat as modified for now)
			if strings.HasSuffix(to, ".md") {
				modified = append(modified, to)
			}
		}
	}

	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(deleted)
	return added, modified, deleted, nil
}
