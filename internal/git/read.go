// Read-only operations on the git store: file reads, directory listings, log,
// grep, and diffing. None of these methods modify the repository.
package git

import (
	"database/sql"
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
)

// ReadFileWithHash returns both the file content and the blob hash for the given path.
func (s *Store) ReadFileWithHash(path string) (string, string, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: head: %w", err)
	}
	commit, err := s.repo.CommitObject(headRef.Hash())
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

// ReadFileAtCommit reads the content of path from a specific commit.
func (s *Store) ReadFileAtCommit(path, commitHash string) (string, error) {
	hash := plumbing.NewHash(commitHash)
	commit, err := s.repo.CommitObject(hash)
	if err != nil {
		return "", fmt.Errorf("ReadFileAtCommit: commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("ReadFileAtCommit: tree: %w", err)
	}
	f, err := tree.File(path)
	if err != nil {
		return "", fmt.Errorf("ReadFileAtCommit: file %q: %w", path, err)
	}
	return f.Contents()
}

// ReadFileLastCommit finds the most recent ancestor of beforeCommitHash where
// path existed and returns its content and commit hash. Used to read facts
// that were deleted in beforeCommitHash (e.g. retract commits).
func (s *Store) ReadFileLastCommit(path, beforeCommitHash string) (content string, fromCommit string, err error) {
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

	content, err = s.ReadFileAtCommit(path, lastCommit.Hash.String())
	return content, lastCommit.Hash.String(), err
}

// ReadFile reads the content of path from the HEAD commit.
func (s *Store) ReadFile(path string) (string, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return "", fmt.Errorf("ReadFile: head: %w", err)
	}

	commit, err := s.repo.CommitObject(headRef.Hash())
	if err != nil {
		return "", fmt.Errorf("ReadFile: commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("ReadFile: tree: %w", err)
	}

	f, err := tree.File(path)
	if err != nil {
		return "", fmt.Errorf("ReadFile: file %q: %w", path, err)
	}

	return f.Contents()
}

// FileExists returns true if path exists in HEAD, false+nil if not found.
func (s *Store) FileExists(path string) (bool, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return false, fmt.Errorf("FileExists: head: %w", err)
	}

	commit, err := s.repo.CommitObject(headRef.Hash())
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

// ListDir returns entries under path in HEAD's tree.
// Subdirectories have IsDir=true, .md files have IsDir=false.
func (s *Store) ListDir(path string) ([]DirEntry, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("ListDir: head: %w", err)
	}

	commit, err := s.repo.CommitObject(headRef.Hash())
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

// ListAll returns paths of all .md files under HEAD.
func (s *Store) ListAll() ([]string, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("ListAll: head: %w", err)
	}

	commit, err := s.repo.CommitObject(headRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("ListAll: commit: %w", err)
	}

	fileIter, err := commit.Files()
	if err != nil {
		return nil, fmt.Errorf("ListAll: files: %w", err)
	}
	defer fileIter.Close()

	var paths []string
	err = fileIter.ForEach(func(f *object.File) error {
		if strings.HasSuffix(f.Name, ".md") {
			paths = append(paths, f.Name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ListAll: iterate: %w", err)
	}

	sort.Strings(paths)
	return paths, nil
}

// Log returns log entries for commits that modified path (up to 50).
func (s *Store) Log(path string) ([]LogEntry, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("Log: head: %w", err)
	}

	logIter, err := s.repo.Log(&gogit.LogOptions{
		From:     headRef.Hash(),
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
// If path is empty, returns all commits. If after is non-empty, skips
// commits until that hash is found, then returns the next `limit` entries.
// Returns the entries, a "next" cursor (empty string if no more), and error.
func (s *Store) LogPaginated(path string, limit int, after string) ([]LogEntryWithTags, string, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return nil, "", fmt.Errorf("LogPaginated: head: %w", err)
	}

	opts := &gogit.LogOptions{
		From:  headRef.Hash(),
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

	tagIndex, err := s.buildTagIndex()
	if err != nil {
		return nil, "", fmt.Errorf("LogPaginated: tags: %w", err)
	}

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

		tags := tagIndex[c.Hash]
		if tags == nil {
			tags = []string{}
		}

		entries = append(entries, LogEntryWithTags{
			Commit:    hash,
			Date:      c.Committer.When.UTC().Format(time.RFC3339),
			Message:   firstLine,
			Tags:      tags,
			Operation: parseOperation(c.Author.Email),
		})
		return nil
	})

	return entries, nextCursor, nil
}

// Activity computes commit-activity metrics for path using a SQL aggregate
// query when commit_log is available, or a capped go-git walk otherwise.
// path may be a directory prefix or a specific .md file.
func (s *Store) Activity(path string) (ActivityResult, error) {
	if s.commitLog {
		return s.activitySQL(path)
	}
	return s.activityGit(path)
}

func (s *Store) activitySQL(path string) (ActivityResult, error) {
	cutoff7 := commitLogAge(7)
	cutoff30 := commitLogAge(30)
	cutoff90 := commitLogAge(90)

	var filter string
	args := []any{cutoff7, cutoff30, cutoff90}
	if path == "" {
		filter = "1=1"
	} else if strings.HasSuffix(path, ".md") {
		filter = "path = ?"
		args = append(args, path)
	} else {
		filter = "path GLOB ?"
		args = append(args, path+"/*")
	}

	q := fmt.Sprintf(`
		SELECT MAX(committed_at),
		       COUNT(DISTINCT commit_hash),
		       COUNT(DISTINCT CASE WHEN committed_at > ? THEN commit_hash END),
		       COUNT(DISTINCT CASE WHEN committed_at > ? THEN commit_hash END),
		       COUNT(DISTINCT CASE WHEN committed_at > ? THEN commit_hash END)
		FROM commit_log WHERE %s`, filter)

	var lastTS sql.NullInt64
	var total, c7, c30, c90 int
	if err := s.db.QueryRow(q, args...).Scan(&lastTS, &total, &c7, &c30, &c90); err != nil {
		return ActivityResult{}, fmt.Errorf("activitySQL: %w", err)
	}

	var lastCommit string
	if lastTS.Valid {
		lastCommit = time.Unix(lastTS.Int64, 0).UTC().Format(time.RFC3339)
	}
	return ActivityResult{
		LastCommit: lastCommit,
		Total:      total,
		Changes7d:  c7,
		Changes30d: c30,
		Changes90d: c90,
	}, nil
}

func (s *Store) activityGit(path string) (ActivityResult, error) {
	const maxCommits = 500

	headRef, err := s.repo.Head()
	if err != nil {
		return ActivityResult{}, fmt.Errorf("Activity: head: %w", err)
	}

	opts := &gogit.LogOptions{
		From:  headRef.Hash(),
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

// buildTagIndex returns a map from commit hash to tag names.
func (s *Store) buildTagIndex() (map[plumbing.Hash][]string, error) {
	idx := make(map[plumbing.Hash][]string)
	refIter, err := s.storer.IterReferences()
	if err != nil {
		return nil, err
	}
	_ = refIter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if strings.HasPrefix(name, "refs/tags/") {
			tagName := strings.TrimPrefix(name, "refs/tags/")
			idx[ref.Hash()] = append(idx[ref.Hash()], tagName)
		}
		return nil
	})
	return idx, nil
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

	firstLine := commit.Message
	if idx := strings.IndexByte(firstLine, '\n'); idx >= 0 {
		firstLine = firstLine[:idx]
	}

	tagIndex, _ := s.buildTagIndex()
	tags := tagIndex[hash]
	if tags == nil {
		tags = []string{}
	}

	return &CommitDetailResult{
		Commit:    hash.String(),
		Date:      commit.Committer.When.UTC().Format(time.RFC3339),
		Message:   firstLine,
		Tags:      tags,
		Operation: parseOperation(commit.Author.Email),
		Files:     files,
	}, nil
}

// WalkChangedFiles returns .md files under prefix most recently changed,
// excluding already-seen paths, up to limit results.
// Uses a SQL aggregate query when commit_log is available, or a full git walk
// otherwise. Returns files ordered most-recently-changed first, and the HEAD
// commit hash for session compatibility (SQL path ignores fromCommit).
func (s *Store) WalkChangedFiles(fromCommit string, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error) {
	if s.commitLog {
		return s.walkChangedFilesSQL(prefix, seen, limit)
	}
	return s.walkChangedFilesGit(fromCommit, prefix, seen, limit)
}

func (s *Store) walkChangedFilesSQL(prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error) {
	var whereParts []string
	var args []any

	if prefix != "" {
		whereParts = append(whereParts, "path GLOB ?")
		args = append(args, prefix+"/*")
	}
	if len(seen) > 0 {
		placeholders := make([]string, 0, len(seen))
		for p := range seen {
			placeholders = append(placeholders, "?")
			args = append(args, p)
		}
		whereParts = append(whereParts, "path NOT IN ("+strings.Join(placeholders, ",")+")")
	}

	where := "1=1"
	if len(whereParts) > 0 {
		where = strings.Join(whereParts, " AND ")
	}

	q := fmt.Sprintf(`
		SELECT path, MAX(committed_at) AS ts, MAX(rowid) AS last_rowid
		FROM commit_log
		WHERE %s
		GROUP BY path
		ORDER BY ts DESC, last_rowid DESC
		LIMIT ?`, where)
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("walkChangedFilesSQL: query: %w", err)
	}
	defer rows.Close()

	var results []FileRecency
	for rows.Next() {
		var path string
		var ts, lastRowid int64
		if err := rows.Scan(&path, &ts, &lastRowid); err != nil {
			return nil, "", fmt.Errorf("walkChangedFilesSQL: scan: %w", err)
		}
		results = append(results, FileRecency{
			Path:      path,
			Timestamp: time.Unix(ts, 0).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("walkChangedFilesSQL: rows: %w", err)
	}

	// Return HEAD hash for session compatibility (fromCommit is ignored in SQL path).
	headRef, err := s.repo.Head()
	if err != nil {
		return results, "", nil
	}
	return results, headRef.Hash().String(), nil
}

func (s *Store) walkChangedFilesGit(fromCommit string, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error) {
	var from plumbing.Hash
	if fromCommit != "" {
		from = plumbing.NewHash(fromCommit)
	} else {
		headRef, err := s.repo.Head()
		if err != nil {
			return nil, "", fmt.Errorf("WalkChangedFiles: head: %w", err)
		}
		from = headRef.Hash()
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

// Grep searches all .md files in HEAD for pattern, returns matching paths.
func (s *Store) Grep(pattern string) ([]string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("Grep: compile pattern: %w", err)
	}

	headRef, err := s.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("Grep: head: %w", err)
	}

	commit, err := s.repo.CommitObject(headRef.Hash())
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

// DiffFiles returns paths added/modified/deleted between fromCommit and HEAD.
// Only .md files are returned. If fromCommit is empty, diffs from empty tree.
func (s *Store) DiffFiles(fromCommit string) (added, modified, deleted []string, err error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: head: %w", err)
	}

	toCommit, err := s.repo.CommitObject(headRef.Hash())
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
