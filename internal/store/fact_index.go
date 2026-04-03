package store

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/store/git"
)

// Compile-time interface checks.
var _ FactIndex = (*factIndex)(nil)

// factIndex owns all git-backed fact operations: reading, writing, and commit-log
// management. It is embedded in Service so that Service satisfies FactIndex and
// gitReader without code duplication.
type factIndex struct {
	rh         *repoHandler
	branchMu   sync.Map // per-branch write serialization
	auth       transport.AuthMethod
	signer     ssh.Signer
	onCommit   func(branch, hash string)
	postCommit func(ctx context.Context, git gitReader, branch string) error // wired to si.Sync in Step 6
}

// lockBranch acquires the per-branch mutex and returns an unlock function.
func (fi *factIndex) lockBranch(branch string) func() {
	v, _ := fi.branchMu.LoadOrStore(branch, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// authorSig returns the author signature for a given operation.
func (fi *factIndex) authorSig(branch, operation string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "+" + operation + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// committerSig returns the committer signature (stable per agent).
func (fi *factIndex) committerSig(branch string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// notifyCommit calls appendCommitLog and then the optional external callback.
func (fi *factIndex) notifyCommit(ctx context.Context, branch string, hash plumbing.Hash) {
	fi.appendCommitLog(ctx, branch, hash)
	if fi.onCommit != nil {
		fi.onCommit(branch, hash.String())
	}
}

// HeadCommit returns the hash of the tip commit of branch as a hex string.
func (fi *factIndex) HeadCommit(ctx context.Context, branch string) (string, error) {
	return fi.rh.HeadCommit(ctx, branch)
}

// createBranch creates a new branch ref pointing at the tip of fromBranch.
// No-op if branch already exists.
func (fi *factIndex) createBranch(ctx context.Context, branch, fromBranch string) error {
	newRefName := plumbing.NewBranchReferenceName(branch)
	if _, err := fi.rh.gits.Reference(newRefName); err == nil {
		return nil // already exists
	}
	fromHash, err := fi.rh.resolveRef(ctx, fromBranch)
	if err != nil {
		return fmt.Errorf("createBranch: resolve source %q: %w", fromBranch, err)
	}
	if err := fi.rh.gits.SetReference(plumbing.NewHashReference(newRefName, fromHash)); err != nil {
		return fmt.Errorf("createBranch: set ref: %w", err)
	}
	log.Info().Str("branch", branch).Str("from", fromBranch).Msg("created branch")
	return nil
}

// ── Read-only operations ──────────────────────────────────────────────────────
// Read-only operations on the git store: file reads, directory listings, log,
// grep, and diffing. None of these methods modify the repository.

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
func (fi *factIndex) readFileLastCommit(ctx context.Context, branch, path, beforeCommitHash string) (content string, fromCommit string, err error) {
	path = strings.ToLower(path)
	startHash := plumbing.NewHash(beforeCommitHash)
	startCommit, err := fi.rh.repo.CommitObject(startHash)
	if err != nil {
		return "", "", fmt.Errorf("readFileLastCommit: commit: %w", err)
	}
	if len(startCommit.ParentHashes) == 0 {
		return "", "", fmt.Errorf("readFileLastCommit: %q: commit has no parents", path)
	}

	logIter, err := fi.rh.repo.Log(&gogit.LogOptions{
		From:     startCommit.ParentHashes[0],
		FileName: &path,
		Order:    gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return "", "", fmt.Errorf("readFileLastCommit: log: %w", err)
	}
	defer logIter.Close()

	lastCommit, err := logIter.Next()
	if err != nil {
		return "", "", fmt.Errorf("readFileLastCommit: %q: no prior commit found", path)
	}

	content, err = fi.rh.readFileAtCommitHash(ctx, path, lastCommit.Hash.String())
	return content, lastCommit.Hash.String(), err
}

// FileExists returns true if path exists at the tip of branch, false+nil if not found.
func (fi *factIndex) fileExists(ctx context.Context, branch, path string) (bool, error) {
	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return false, fmt.Errorf("fileExists: ref: %w", err)
	}

	commit, err := fi.rh.repo.CommitObject(headHash)
	if err != nil {
		return false, fmt.Errorf("fileExists: commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return false, fmt.Errorf("fileExists: tree: %w", err)
	}

	_, err = tree.FindEntry(path)
	if err == object.ErrEntryNotFound || err == object.ErrDirectoryNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("fileExists: find entry: %w", err)
	}
	return true, nil
}

// ReadFact reads a fact from the store. With nil opts it reads from branch HEAD.
func (fi *factIndex) ReadFact(ctx context.Context, branch, path string, opts *ReadFactOpts) (ReadFactResult, error) {
	if opts == nil {
		opts = &ReadFactOpts{}
	}
	switch {
	case opts.BeforeCommit != "":
		content, fromCommit, err := fi.readFileLastCommit(ctx, branch, path, opts.BeforeCommit)
		if err != nil {
			return ReadFactResult{}, err
		}
		return ReadFactResult{Content: content, FromCommit: fromCommit}, nil
	case opts.AtCommit != "":
		content, err := fi.rh.readFileAtCommit(ctx, branch, path, opts.AtCommit)
		if err != nil {
			return ReadFactResult{}, err
		}
		return ReadFactResult{Content: content}, nil
	case opts.WithHash:
		content, blobHash, err := fi.rh.readFileWithHash(ctx, branch, path)
		if err != nil {
			return ReadFactResult{}, err
		}
		return ReadFactResult{Content: content, BlobHash: blobHash}, nil
	default:
		content, err := fi.rh.readFile(ctx, branch, path)
		if err != nil {
			return ReadFactResult{}, err
		}
		return ReadFactResult{Content: content}, nil
	}
}

// FactExists returns true if a fact exists at path on branch HEAD.
func (fi *factIndex) FactExists(ctx context.Context, branch, path string) (bool, error) {
	return fi.fileExists(ctx, branch, path)
}

// ListDir returns entries under path at the tip of branch.
// Subdirectories have IsDir=true, .md files have IsDir=false.
func (fi *factIndex) ListDir(ctx context.Context, branch, path string) ([]DirEntry, error) {
	path = strings.ToLower(path)
	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("ListDir: ref: %w", err)
	}

	commit, err := fi.rh.repo.CommitObject(headHash)
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
func (fi *factIndex) LastCommitForPath(ctx context.Context, branch, path string) (string, error) {
	return fi.rh.LastCommitForPath(ctx, branch, path)
}

// ListAllWithHash returns all .md files at the tip of branch with their blob hashes.
// Single tree walk — no per-file I/O.
func (fi *factIndex) ListAllWithHash(ctx context.Context, branch string) ([]string, []string, error) {
	return fi.rh.ListAllWithHash(ctx, branch)
}

// ListAll returns paths of all .md files at the tip of branch.
func (fi *factIndex) ListAll(ctx context.Context, branch string) ([]string, error) {
	return fi.rh.ListAll(ctx, branch)
}

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
		if len(hash) > 8 {
			hash = hash[:8]
		}
		fl := c.Message
		if idx := strings.IndexByte(fl, '\n'); idx >= 0 {
			fl = fl[:idx]
		}
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
		fl := c.Message
		if idx := strings.IndexByte(fl, '\n'); idx >= 0 {
			fl = fl[:idx]
		}

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

// ── Write operations ──────────────────────────────────────────────────────────
// Write operations on the git store: single/batch file writes, deletes, and tagging.

// WriteFile writes content to path in a new commit with message on branch.
// Returns the commit hash and the blob hash of the written file.
func (fi *factIndex) writeFile(ctx context.Context, branch, path, content, message, operation string) (commitHash string, blobHash string, err error) {
	path = strings.ToLower(path)
	if path == "" {
		return "", "", fmt.Errorf("store: WriteFile: path must not be empty")
	}
	if strings.Contains(path, "..") {
		return "", "", fmt.Errorf("store: WriteFile: path must not contain '..'")
	}

	unlock := fi.lockBranch(branch)

	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		unlock()
		return "", "", fmt.Errorf("WriteFile: ref: %w", err)
	}

	author := fi.authorSig(branch, operation)
	committer := fi.committerSig(branch)
	newCommitHash, newBlobHash, err := writeFileToStore(fi.rh.gits, headHash, path, content, message, author, committer)
	if err != nil {
		unlock()
		return "", "", err
	}

	newCommitHash, err = signCommitInPlace(fi.rh.gits, fi.signer, newCommitHash)
	if err != nil {
		unlock()
		return "", "", err
	}

	// Update the branch ref to point to the new commit.
	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := fi.rh.gits.SetReference(plumbing.NewHashReference(branchRefName, newCommitHash)); err != nil {
		unlock()
		return "", "", err
	}
	unlock()

	// Notify outside the lock — appendCommitLog triggers index sync which
	// may call back into Service for reads.
	fi.notifyCommit(ctx, branch, newCommitHash)
	return newCommitHash.String(), newBlobHash.String(), nil
}

// DeleteFile removes path from branch and creates a commit.
// Returns the commit hash of the new commit.
func (fi *factIndex) deleteFile(ctx context.Context, branch, path, message, operation string) (commitHash string, err error) {
	path = strings.ToLower(path)
	if path == "" {
		return "", fmt.Errorf("store: DeleteFile: path must not be empty")
	}
	if strings.Contains(path, "..") {
		return "", fmt.Errorf("store: DeleteFile: path must not contain '..'")
	}

	unlock := fi.lockBranch(branch)

	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		unlock()
		return "", fmt.Errorf("DeleteFile: ref: %w", err)
	}

	// Check existence inside the lock to avoid a TOCTOU race.
	exists, err := fi.fileExists(ctx, branch, path)
	if err != nil {
		unlock()
		return "", fmt.Errorf("DeleteFile: check exists: %w", err)
	}
	if !exists {
		unlock()
		return "", fmt.Errorf("DeleteFile: file %q does not exist", path)
	}

	author := fi.authorSig(branch, operation)
	committer := fi.committerSig(branch)
	newCommitHash, err := deleteFileFromStore(fi.rh.gits, headHash, path, message, author, committer)
	if err != nil {
		unlock()
		return "", err
	}

	newCommitHash, err = signCommitInPlace(fi.rh.gits, fi.signer, newCommitHash)
	if err != nil {
		unlock()
		return "", err
	}

	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := fi.rh.gits.SetReference(plumbing.NewHashReference(branchRefName, newCommitHash)); err != nil {
		unlock()
		return "", err
	}
	unlock()

	fi.notifyCommit(ctx, branch, newCommitHash)
	return newCommitHash.String(), nil
}

// BatchWrite writes multiple files in one commit on branch.
// Returns the commit hash and a map of path → blob hash for each written file.
func (fi *factIndex) batchWrite(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error) {
	if len(files) == 0 {
		return "", nil, nil
	}

	// Lowercase all paths.
	lowered := make(map[string]string, len(files))
	for path, content := range files {
		lowered[strings.ToLower(path)] = content
	}
	files = lowered

	// Pre-flight validation: reject empty paths and paths containing "..".
	for path := range files {
		if path == "" {
			return "", nil, fmt.Errorf("store: batchWrite: path must not be empty")
		}
		if strings.Contains(path, "..") {
			return "", nil, fmt.Errorf("store: batchWrite: path must not contain '..'")
		}
	}

	unlock := fi.lockBranch(branch)
	cHash, blobHashes, err := fi.batchWriteLocked(ctx, branch, files, message, operation)
	unlock()
	if err != nil {
		return "", nil, err
	}

	fi.notifyCommit(ctx, branch, cHash)
	return cHash.String(), blobHashes, nil
}

// batchWriteLocked performs the actual batchWrite work. Caller must hold the branch lock.
func (fi *factIndex) batchWriteLocked(ctx context.Context, branch string, files map[string]string, message, operation string) (plumbing.Hash, map[string]string, error) {
	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: ref: %w", err)
	}

	parentHash := headHash

	// Read existing root tree.
	var rootTree *object.Tree
	if parentHash != plumbing.ZeroHash {
		parentCommit, err := object.GetCommit(fi.rh.gits, parentHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: get parent commit: %w", err)
		}
		rootTree, err = parentCommit.Tree()
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: get parent tree: %w", err)
		}
	}

	blobHashes := make(map[string]string, len(files))

	// Apply each file to the tree sequentially.
	var currentRootHash plumbing.Hash
	for path, content := range files {
		// Create blob.
		blobObj := fi.rh.gits.NewEncodedObject()
		blobObj.SetType(plumbing.BlobObject)
		bw, err := blobObj.Writer()
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: blob writer for %q: %w", path, err)
		}
		if _, err := io.WriteString(bw, content); err != nil {
			bw.Close()
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: blob write for %q: %w", path, err)
		}
		bw.Close()
		blobHash, err := fi.rh.gits.SetEncodedObject(blobObj)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: store blob for %q: %w", path, err)
		}
		blobHashes[path] = blobHash.String()

		// Update tree.
		currentRootHash, err = buildTree(fi.rh.gits, rootTree, path, blobHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: build tree for %q: %w", path, err)
		}

		// Load updated root tree for next iteration.
		rootTree, err = object.GetTree(fi.rh.gits, currentRootHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: get updated tree: %w", err)
		}
	}

	// Create single commit.
	author := fi.authorSig(branch, operation)
	committer := fi.committerSig(branch)
	commit := &object.Commit{
		Author:    author,
		Committer: committer,
		Message:   message,
		TreeHash:  currentRootHash,
	}
	if parentHash != plumbing.ZeroHash {
		commit.ParentHashes = []plumbing.Hash{parentHash}
	}

	commitObj := fi.rh.gits.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: encode commit: %w", err)
	}
	cHash, err := fi.rh.gits.SetEncodedObject(commitObj)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: store commit: %w", err)
	}

	cHash, err = signCommitInPlace(fi.rh.gits, fi.signer, cHash)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}

	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := fi.rh.gits.SetReference(plumbing.NewHashReference(branchRefName, cHash)); err != nil {
		return plumbing.ZeroHash, nil, err
	}
	return cHash, blobHashes, nil
}

// WriteFact writes a fact to the store and returns the commit and blob hashes.
func (fi *factIndex) WriteFact(ctx context.Context, branch, path, content, message, operation string) (WriteFactResult, error) {
	commitHash, blobHash, err := fi.writeFile(ctx, branch, path, content, message, operation)
	if err != nil {
		return WriteFactResult{}, err
	}
	return WriteFactResult{CommitHash: commitHash, BlobHash: blobHash}, nil
}

// DeleteFact deletes a fact and syncs the index so the deletion is immediately visible.
func (fi *factIndex) DeleteFact(ctx context.Context, branch, path, message string) (string, error) {
	commitHash, err := fi.deleteFile(ctx, branch, path, message, "retract")
	if err != nil {
		return "", fmt.Errorf("DeleteFact git: %w", err)
	}
	if fi.postCommit != nil {
		if err := fi.postCommit(ctx, fi.rh, branch); err != nil {
			return "", fmt.Errorf("DeleteFact sync: %w", err)
		}
	}
	return commitHash, nil
}

// BatchWriteFacts writes multiple facts in a single commit.
func (fi *factIndex) BatchWriteFacts(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error) {
	return fi.batchWrite(ctx, branch, files, message, operation)
}

// tag creates a lightweight tag ref at the tip of branch.
func (fi *factIndex) tag(ctx context.Context, branch, name string) error {
	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return fmt.Errorf("tag: ref: %w", err)
	}

	tagRefName := plumbing.NewTagReferenceName(name)
	return fi.rh.gits.SetReference(plumbing.NewHashReference(tagRefName, headHash))
}

// tagsContaining returns tag names whose target is reachable from hash.
func (fi *factIndex) tagsContaining(ctx context.Context, hash string) ([]string, error) {
	targetHash := plumbing.NewHash(hash)

	// Build set of all commits reachable from targetHash (one walk).
	reachable := make(map[plumbing.Hash]bool)
	logIter, err := fi.rh.repo.Log(&gogit.LogOptions{From: targetHash})
	if err != nil {
		return nil, fmt.Errorf("tagsContaining: log from target: %w", err)
	}
	_ = logIter.ForEach(func(c *object.Commit) error {
		reachable[c.Hash] = true
		return nil
	})
	logIter.Close()

	refIter, err := fi.rh.gits.IterReferences()
	if err != nil {
		return nil, fmt.Errorf("tagsContaining: iter refs: %w", err)
	}
	defer refIter.Close()

	var tags []string
	err = refIter.ForEach(func(ref *plumbing.Reference) error {
		if !strings.HasPrefix(ref.Name().String(), "refs/tags/") {
			return nil
		}
		if reachable[ref.Hash()] {
			tagName := strings.TrimPrefix(ref.Name().String(), "refs/tags/")
			tags = append(tags, tagName)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("tagsContaining: %w", err)
	}

	sort.Strings(tags)
	return tags, nil
}

// ── FactsIter constructor ─────────────────────────────────────────────────────

// FactsIter opens a cursor over facts for the given branch ordered by
// fact_id DESC. The caller must call Close() when done to release the
// underlying database cursor.
func (fi *factIndex) FactsIter(ctx context.Context, branch string) (*FactsIter, error) {
	branchID, err := fi.rh.branchID(ctx, branch)
	if err != nil {
		return nil, err
	}
	rows, err := conn(ctx, fi.rh.db).QueryContext(ctx,
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

// ── Commit log operations ─────────────────────────────────────────────────────
// commit_log population and append helpers.
// The commit_log table is a denormalized index of (commit_hash, path, committed_at, message)
// that enables O(1) activity aggregates and efficient path-history queries.
//
// Queries use commit_hash as a tiebreaker when commits share the same committed_at timestamp,
// giving deterministic and stable pagination without depending on insertion order.

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
		log.Warn().Err(err).Str("hash", hash.String()[:8]).Msg("commit_log: get commit")
		return
	}
	files, err := changedFilesInCommit(c)
	if err != nil {
		log.Warn().Err(err).Str("hash", hash.String()[:8]).Msg("commit_log: changed files")
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
		log.Warn().Err(err).Str("hash", hash.String()[:8]).Msg("commit_log: append sync failed")
	}
}

// commitLogAge is used for SQL activity queries.
func commitLogAge(days int) int64 {
	return time.Now().Unix() - int64(days)*86400
}
