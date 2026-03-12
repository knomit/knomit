package git

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-billy/v5/memfs"

	"knomit/internal/gitstorer"
)

// Store wraps go-git with knomit's logical operations.
// All fact reads/writes go through go-git's plumbing API — NO filesystem reads/writes.
type Store struct {
	mu     sync.Mutex
	repo   *gogit.Repository
	storer *gitstorer.Storer
	dbPath string
	branch string // e.g. "agent/laptop"
}

// DirEntry represents a single entry in a knomit directory listing.
type DirEntry struct {
	Name    string
	IsDir bool // true = subdirectory, false = .md file
}

// LogEntry represents a single git commit in a log listing.
type LogEntry struct {
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Message string `json:"message"`
}

// Init creates a new knomit git store at dbPath.
// It initialises the SQLite-backed git repository, sets git config, creates
// an initial commit with know.md, and creates the agent branch.
func Init(dbPath string) (*Store, error) {
	// 1. Create parent directory if it doesn't exist.
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("git.Init: mkdir: %w", err)
	}

	// 2. Open the SQLite storer.
	s, err := gitstorer.New(dbPath)
	if err != nil {
		return nil, fmt.Errorf("git.Init: storer: %w", err)
	}

	// 3. Initialise git with memfs worktree (memfs is never used directly).
	repo, err := gogit.Init(s, memfs.New())
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("git.Init: git init: %w", err)
	}

	// 4. Set git config.
	cfg, err := repo.Config()
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("git.Init: read config: %w", err)
	}
	cfg.User.Name = "knomit"
	cfg.User.Email = "knomit@local"
	// Disable GPG signing via raw config section.
	if cfg.Raw != nil {
		cfg.Raw.Section("commit").SetOption("gpgsign", "false")
	}
	if err := repo.SetConfig(cfg); err != nil {
		s.Close()
		return nil, fmt.Errorf("git.Init: set config: %w", err)
	}

	// 5. Create initial commit containing know.md.
	rootManifest := "# Knowledge Base\n\nRoot manifest.\n"
	initCommitHash, err := writeFileToStore(s, plumbing.ZeroHash, "know.md", rootManifest, "init: create knowledge base")
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("git.Init: initial commit: %w", err)
	}

	// 6. Determine agent branch name.
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "local"
	}
	agentBranch := "agent/" + hostname
	agentRefName := plumbing.NewBranchReferenceName(agentBranch)

	// 7. Create the agent branch ref pointing to the initial commit.
	agentRef := plumbing.NewHashReference(agentRefName, initCommitHash)
	if err := s.SetReference(agentRef); err != nil {
		s.Close()
		return nil, fmt.Errorf("git.Init: set agent ref: %w", err)
	}

	// Update HEAD to point to the agent branch.
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)
	if err := s.SetReference(headRef); err != nil {
		s.Close()
		return nil, fmt.Errorf("git.Init: set HEAD: %w", err)
	}

	// Also ensure main ref exists for the initial commit.
	mainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), initCommitHash)
	if err := s.SetReference(mainRef); err != nil {
		s.Close()
		return nil, fmt.Errorf("git.Init: set main ref: %w", err)
	}

	return &Store{
		repo:   repo,
		storer: s,
		dbPath: dbPath,
		branch: agentBranch,
	}, nil
}

// Open opens an existing knomit git store at dbPath.
func Open(dbPath string) (*Store, error) {
	s, err := gitstorer.New(dbPath)
	if err != nil {
		return nil, fmt.Errorf("git.Open: storer: %w", err)
	}

	repo, err := gogit.Open(s, memfs.New())
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("git.Open: git open: %w", err)
	}

	// Read HEAD to determine current branch name.
	head, err := repo.Head()
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("git.Open: read HEAD: %w", err)
	}

	branch := strings.TrimPrefix(head.Name().String(), "refs/heads/")

	return &Store{
		repo:   repo,
		storer: s,
		dbPath: dbPath,
		branch: branch,
	}, nil
}

// WriteFile writes content to path in a new commit with message.
// Uses go-git plumbing API — NO filesystem.
func (s *Store) WriteFile(path, content, message string) error {
	if path == "" {
		return fmt.Errorf("git: WriteFile: path must not be empty")
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("git: WriteFile: path must not contain '..'")
	}

	headRef, err := s.repo.Head()
	if err != nil {
		return fmt.Errorf("WriteFile: head: %w", err)
	}

	newCommitHash, err := writeFileToStore(s.storer, headRef.Hash(), path, content, message)
	if err != nil {
		return err
	}

	// Update the branch ref to point to the new commit.
	branchRefName := plumbing.NewBranchReferenceName(s.branch)
	newRef := plumbing.NewHashReference(branchRefName, newCommitHash)
	return s.storer.SetReference(newRef)
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

// HeadCommit returns the hash of the current HEAD commit as a hex string.
func (s *Store) HeadCommit() (string, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return "", fmt.Errorf("HeadCommit: %w", err)
	}
	return headRef.Hash().String(), nil
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

// Branch returns the agent branch name (e.g. "agent/laptop").
func (s *Store) Branch() string {
	return s.branch
}

// Close closes the underlying gitstorer.
func (s *Store) Close() error {
	return s.storer.Close()
}

// Storer returns the underlying gitstorer (used by the git remote handler).
func (s *Store) Storer() *gitstorer.Storer {
	return s.storer
}

// SyncResult is returned by Sync.
type SyncResult struct {
	Synced bool
	Ahead  int
}

// DeleteFile removes path from HEAD and creates a commit.
func (s *Store) DeleteFile(path, message string) error {
	if path == "" {
		return fmt.Errorf("git: DeleteFile: path must not be empty")
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("git: DeleteFile: path must not contain '..'")
	}

	headRef, err := s.repo.Head()
	if err != nil {
		return fmt.Errorf("DeleteFile: head: %w", err)
	}

	newCommitHash, err := deleteFileFromStore(s.storer, headRef.Hash(), path, message)
	if err != nil {
		return err
	}

	branchRefName := plumbing.NewBranchReferenceName(s.branch)
	newRef := plumbing.NewHashReference(branchRefName, newCommitHash)
	return s.storer.SetReference(newRef)
}

// Tag creates a lightweight tag ref at HEAD.
func (s *Store) Tag(name string) error {
	headRef, err := s.repo.Head()
	if err != nil {
		return fmt.Errorf("Tag: head: %w", err)
	}

	tagRefName := plumbing.NewTagReferenceName(name)
	tagRef := plumbing.NewHashReference(tagRefName, headRef.Hash())
	return s.storer.SetReference(tagRef)
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

// TagsContaining returns tag names whose target is reachable from hash.
// Optimization: collect all commits reachable from hash in one walk, then
// check each tag's target against that set — O(depth + tags) instead of
// O(tags × depth).
func (s *Store) TagsContaining(hash string) ([]string, error) {
	targetHash := plumbing.NewHash(hash)

	// Build set of all commits reachable from targetHash (one walk).
	reachable := make(map[plumbing.Hash]bool)
	logIter, err := s.repo.Log(&gogit.LogOptions{From: targetHash})
	if err != nil {
		return nil, fmt.Errorf("TagsContaining: log from target: %w", err)
	}
	_ = logIter.ForEach(func(c *object.Commit) error {
		reachable[c.Hash] = true
		return nil
	})
	logIter.Close()

	refIter, err := s.storer.IterReferences()
	if err != nil {
		return nil, fmt.Errorf("TagsContaining: iter refs: %w", err)
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
		return nil, fmt.Errorf("TagsContaining: %w", err)
	}

	sort.Strings(tags)
	return tags, nil
}

// Sync fetches from origin and merges origin/main into the agent branch.
// If no remote exists, returns SyncResult{Synced: false}, nil.
func (s *Store) Sync(remoteAuth interface{}) (SyncResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if origin remote exists.
	_, err := s.repo.Remote("origin")
	if err != nil {
		// No remote — nothing to sync.
		return SyncResult{Synced: false}, nil
	}

	// Fetch from origin.
	err = s.repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return SyncResult{}, fmt.Errorf("Sync: fetch: %w", err)
	}

	// Resolve origin/main ref.
	originMainRef, err := s.storer.Reference(plumbing.NewRemoteReferenceName("origin", "main"))
	if err != nil {
		// origin/main doesn't exist — nothing to merge.
		return SyncResult{Synced: false}, nil
	}
	originMainHash := originMainRef.Hash()

	// Get current agent branch HEAD.
	agentRefName := plumbing.NewBranchReferenceName(s.branch)
	agentRef, err := s.storer.Reference(agentRefName)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: agent ref: %w", err)
	}
	agentHash := agentRef.Hash()

	// Count commits in origin/main not in agent branch.
	ahead, err := s.countAhead(originMainHash, agentHash)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: count ahead: %w", err)
	}

	// If origin/main is already an ancestor of agent branch, nothing to merge.
	isAncestor, err := s.isAncestor(originMainHash, agentHash)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: check ancestor: %w", err)
	}
	if isAncestor {
		return SyncResult{Synced: false, Ahead: ahead}, nil
	}

	// Create a merge commit: agent branch HEAD + origin/main as parents,
	// using origin/main's tree merged on top of agent's tree.
	// Strategy: create a merge commit with two parents, using the to-tree from origin/main
	// overlaid on the agent tree (origin/main wins for conflicts — simple strategy).
	originCommit, err := s.repo.CommitObject(originMainHash)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: origin commit: %w", err)
	}

	agentCommit, err := s.repo.CommitObject(agentHash)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: agent commit: %w", err)
	}

	// Build merged tree: start from agent tree, apply all files from origin/main tree.
	mergedTreeHash, err := s.mergeTrees(agentCommit, originCommit)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: merge trees: %w", err)
	}

	now := time.Now()
	sig := object.Signature{
		Name:  "knomit",
		Email: "knomit@local",
		When:  now,
	}
	mergeCommit := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      fmt.Sprintf("sync: merge origin/main into %s", s.branch),
		TreeHash:     mergedTreeHash,
		ParentHashes: []plumbing.Hash{agentHash, originMainHash},
	}

	commitObj := s.storer.NewEncodedObject()
	if err := mergeCommit.Encode(commitObj); err != nil {
		return SyncResult{}, fmt.Errorf("Sync: encode merge commit: %w", err)
	}
	mergeHash, err := s.storer.SetEncodedObject(commitObj)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: store merge commit: %w", err)
	}

	newRef := plumbing.NewHashReference(agentRefName, mergeHash)
	if err := s.storer.SetReference(newRef); err != nil {
		return SyncResult{}, fmt.Errorf("Sync: update ref: %w", err)
	}

	return SyncResult{Synced: true, Ahead: ahead}, nil
}

// BatchWrite writes multiple files in one commit.
func (s *Store) BatchWrite(files map[string]string, message string) error {
	if len(files) == 0 {
		return nil
	}

	// Pre-flight validation: reject empty paths and paths containing "..".
	for path := range files {
		if path == "" {
			return fmt.Errorf("git: BatchWrite: path must not be empty")
		}
		if strings.Contains(path, "..") {
			return fmt.Errorf("git: BatchWrite: path must not contain '..'")
		}
	}

	headRef, err := s.repo.Head()
	if err != nil {
		return fmt.Errorf("BatchWrite: head: %w", err)
	}

	parentHash := headRef.Hash()

	// Read existing root tree.
	var rootTree *object.Tree
	if parentHash != plumbing.ZeroHash {
		parentCommit, err := object.GetCommit(s.storer, parentHash)
		if err != nil {
			return fmt.Errorf("BatchWrite: get parent commit: %w", err)
		}
		rootTree, err = parentCommit.Tree()
		if err != nil {
			return fmt.Errorf("BatchWrite: get parent tree: %w", err)
		}
	}

	// Apply each file to the tree sequentially.
	var currentRootHash plumbing.Hash
	for path, content := range files {
		// Create blob.
		blobObj := s.storer.NewEncodedObject()
		blobObj.SetType(plumbing.BlobObject)
		bw, err := blobObj.Writer()
		if err != nil {
			return fmt.Errorf("BatchWrite: blob writer for %q: %w", path, err)
		}
		if _, err := io.WriteString(bw, content); err != nil {
			bw.Close()
			return fmt.Errorf("BatchWrite: blob write for %q: %w", path, err)
		}
		bw.Close()
		blobHash, err := s.storer.SetEncodedObject(blobObj)
		if err != nil {
			return fmt.Errorf("BatchWrite: store blob for %q: %w", path, err)
		}

		// Update tree.
		currentRootHash, err = buildTree(s.storer, rootTree, path, blobHash)
		if err != nil {
			return fmt.Errorf("BatchWrite: build tree for %q: %w", path, err)
		}

		// Load updated root tree for next iteration.
		rootTree, err = object.GetTree(s.storer, currentRootHash)
		if err != nil {
			return fmt.Errorf("BatchWrite: get updated tree: %w", err)
		}
	}

	// Create single commit.
	now := time.Now()
	sig := object.Signature{
		Name:  "knomit",
		Email: "knomit@local",
		When:  now,
	}
	commit := &object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   message,
		TreeHash:  currentRootHash,
	}
	if parentHash != plumbing.ZeroHash {
		commit.ParentHashes = []plumbing.Hash{parentHash}
	}

	commitObj := s.storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return fmt.Errorf("BatchWrite: encode commit: %w", err)
	}
	commitHash, err := s.storer.SetEncodedObject(commitObj)
	if err != nil {
		return fmt.Errorf("BatchWrite: store commit: %w", err)
	}

	branchRefName := plumbing.NewBranchReferenceName(s.branch)
	newRef := plumbing.NewHashReference(branchRefName, commitHash)
	return s.storer.SetReference(newRef)
}

// --- internal plumbing helpers ---

// writeFileToStore creates a blob+tree+commit for path/content.
// parentCommitHash is ZeroHash for the initial commit.
func writeFileToStore(s *gitstorer.Storer, parentCommitHash plumbing.Hash, path, content, message string) (plumbing.Hash, error) {
	// 1. Create blob.
	blobObj := s.NewEncodedObject()
	blobObj.SetType(plumbing.BlobObject)
	bw, err := blobObj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("writeFileToStore: blob writer: %w", err)
	}
	if _, err := io.WriteString(bw, content); err != nil {
		bw.Close()
		return plumbing.ZeroHash, fmt.Errorf("writeFileToStore: blob write: %w", err)
	}
	bw.Close()
	blobHash, err := s.SetEncodedObject(blobObj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("writeFileToStore: store blob: %w", err)
	}

	// 2. Read existing root tree (if any).
	var existingTree *object.Tree
	if parentCommitHash != plumbing.ZeroHash {
		parentCommit, err := object.GetCommit(s, parentCommitHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("writeFileToStore: get parent commit: %w", err)
		}
		existingTree, err = parentCommit.Tree()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("writeFileToStore: get parent tree: %w", err)
		}
	}

	// 3. Build new root tree with path added/replaced.
	newRootHash, err := buildTree(s, existingTree, path, blobHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("writeFileToStore: build tree: %w", err)
	}

	// 4. Create commit object.
	now := time.Now()
	sig := object.Signature{
		Name:  "knomit",
		Email: "knomit@local",
		When:  now,
	}
	commit := &object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   message,
		TreeHash:  newRootHash,
	}
	if parentCommitHash != plumbing.ZeroHash {
		commit.ParentHashes = []plumbing.Hash{parentCommitHash}
	}

	commitObj := s.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("writeFileToStore: encode commit: %w", err)
	}
	commitHash, err := s.SetEncodedObject(commitObj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("writeFileToStore: store commit: %w", err)
	}

	return commitHash, nil
}

// buildTree constructs a new root tree by adding/replacing path (which may be
// nested, e.g. "know/sub/foo.md") with blobHash. existing may be nil.
func buildTree(s *gitstorer.Storer, existing *object.Tree, path string, blobHash plumbing.Hash) (plumbing.Hash, error) {
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]

	if len(parts) == 1 {
		// Leaf: insert/replace the file entry in this tree.
		return upsertEntry(s, existing, object.TreeEntry{
			Name: name,
			Mode: filemode.Regular,
			Hash: blobHash,
		})
	}

	// Recurse: find existing subtree for parts[0], recurse into it.
	rest := parts[1]
	var subtree *object.Tree
	if existing != nil {
		for _, e := range existing.Entries {
			if e.Name == name && e.Mode == filemode.Dir {
				var err error
				subtree, err = object.GetTree(s, e.Hash)
				if err != nil {
					return plumbing.ZeroHash, fmt.Errorf("buildTree: get subtree %q: %w", name, err)
				}
				break
			}
		}
	}

	subHash, err := buildTree(s, subtree, rest, blobHash)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return upsertEntry(s, existing, object.TreeEntry{
		Name: name,
		Mode: filemode.Dir,
		Hash: subHash,
	})
}

// deleteFileFromStore creates a commit that removes path from the tree.
func deleteFileFromStore(s *gitstorer.Storer, parentCommitHash plumbing.Hash, path, message string) (plumbing.Hash, error) {
	parentCommit, err := object.GetCommit(s, parentCommitHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: get parent commit: %w", err)
	}
	existingTree, err := parentCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: get parent tree: %w", err)
	}

	newRootHash, err := deleteFromTree(s, existingTree, path)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: delete from tree: %w", err)
	}

	now := time.Now()
	sig := object.Signature{
		Name:  "knomit",
		Email: "knomit@local",
		When:  now,
	}
	commit := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      message,
		TreeHash:     newRootHash,
		ParentHashes: []plumbing.Hash{parentCommitHash},
	}

	commitObj := s.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: encode commit: %w", err)
	}
	commitHash, err := s.SetEncodedObject(commitObj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: store commit: %w", err)
	}
	return commitHash, nil
}

// deleteFromTree removes path from the tree rooted at existing, recursively.
func deleteFromTree(s *gitstorer.Storer, existing *object.Tree, path string) (plumbing.Hash, error) {
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]

	if len(parts) == 1 {
		// Leaf: remove the entry from this tree.
		return removeEntry(s, existing, name)
	}

	// Recurse into subtree.
	rest := parts[1]
	var subtree *object.Tree
	for _, e := range existing.Entries {
		if e.Name == name && e.Mode == filemode.Dir {
			var err error
			subtree, err = object.GetTree(s, e.Hash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("deleteFromTree: get subtree %q: %w", name, err)
			}
			break
		}
	}
	if subtree == nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFromTree: subtree %q not found", name)
	}

	subHash, err := deleteFromTree(s, subtree, rest)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return upsertEntry(s, existing, object.TreeEntry{
		Name: name,
		Mode: filemode.Dir,
		Hash: subHash,
	})
}

// removeEntry removes the entry with name from a copy of existing tree,
// encodes, stores and returns the new tree hash.
func removeEntry(s *gitstorer.Storer, existing *object.Tree, name string) (plumbing.Hash, error) {
	var entries []object.TreeEntry
	if existing != nil {
		for _, e := range existing.Entries {
			if e.Name != name {
				entries = append(entries, e)
			}
		}
	}

	sort.Sort(object.TreeEntrySorter(entries))
	tree := &object.Tree{Entries: entries}
	treeObj := s.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("removeEntry: encode tree: %w", err)
	}
	hash, err := s.SetEncodedObject(treeObj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("removeEntry: store tree: %w", err)
	}
	return hash, nil
}

// countAhead returns the number of commits reachable from tip that are not
// reachable from base.
func (s *Store) countAhead(tip, base plumbing.Hash) (int, error) {
	// Collect all commits reachable from base.
	baseSet := make(map[plumbing.Hash]bool)
	baseIter, err := s.repo.Log(&gogit.LogOptions{From: base})
	if err != nil {
		return 0, err
	}
	defer baseIter.Close()
	_ = baseIter.ForEach(func(c *object.Commit) error {
		baseSet[c.Hash] = true
		return nil
	})

	count := 0
	tipIter, err := s.repo.Log(&gogit.LogOptions{From: tip})
	if err != nil {
		return 0, err
	}
	defer tipIter.Close()
	_ = tipIter.ForEach(func(c *object.Commit) error {
		if baseSet[c.Hash] {
			return io.EOF
		}
		count++
		return nil
	})
	return count, nil
}

// isAncestor returns true if candidate is an ancestor of (or equal to) tip.
func (s *Store) isAncestor(candidate, tip plumbing.Hash) (bool, error) {
	if candidate == tip {
		return true, nil
	}
	iter, err := s.repo.Log(&gogit.LogOptions{From: tip})
	if err != nil {
		return false, err
	}
	defer iter.Close()
	found := false
	_ = iter.ForEach(func(c *object.Commit) error {
		if c.Hash == candidate {
			found = true
			return io.EOF
		}
		return nil
	})
	return found, nil
}

// mergeTrees creates a merged tree: starts from agentCommit's tree, then
// overlays all files from originCommit's tree (origin/main wins).
func (s *Store) mergeTrees(agentCommit, originCommit *object.Commit) (plumbing.Hash, error) {
	originFileIter, err := originCommit.Files()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("mergeTrees: origin files: %w", err)
	}
	defer originFileIter.Close()

	// Load agent root tree.
	agentTree, err := agentCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("mergeTrees: agent tree: %w", err)
	}

	currentTree := agentTree
	var currentRootHash plumbing.Hash

	err = originFileIter.ForEach(func(f *object.File) error {
		content, err := f.Contents()
		if err != nil {
			return fmt.Errorf("mergeTrees: read origin file %q: %w", f.Name, err)
		}

		// Create blob.
		blobObj := s.storer.NewEncodedObject()
		blobObj.SetType(plumbing.BlobObject)
		bw, err := blobObj.Writer()
		if err != nil {
			return err
		}
		if _, err := io.WriteString(bw, content); err != nil {
			bw.Close()
			return err
		}
		bw.Close()
		blobHash, err := s.storer.SetEncodedObject(blobObj)
		if err != nil {
			return err
		}

		currentRootHash, err = buildTree(s.storer, currentTree, f.Name, blobHash)
		if err != nil {
			return err
		}
		currentTree, err = object.GetTree(s.storer, currentRootHash)
		return err
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("mergeTrees: overlay: %w", err)
	}

	if currentRootHash == plumbing.ZeroHash {
		// No files from origin — use agent tree hash.
		agentCommitObj, err := s.repo.CommitObject(agentCommit.Hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		t, err := agentCommitObj.Tree()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return t.Hash, nil
	}
	return currentRootHash, nil
}

// upsertEntry adds or replaces entry in a copy of existing (nil → empty tree),
// encodes the resulting tree, stores it, and returns its hash.
func upsertEntry(s *gitstorer.Storer, existing *object.Tree, entry object.TreeEntry) (plumbing.Hash, error) {
	var entries []object.TreeEntry

	if existing != nil {
		for _, e := range existing.Entries {
			if e.Name != entry.Name {
				entries = append(entries, e)
			}
		}
	}
	entries = append(entries, entry)

	// go-git requires entries to be sorted.
	sort.Sort(object.TreeEntrySorter(entries))

	tree := &object.Tree{Entries: entries}
	treeObj := s.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("upsertEntry: encode tree: %w", err)
	}
	hash, err := s.SetEncodedObject(treeObj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("upsertEntry: store tree: %w", err)
	}
	return hash, nil
}
