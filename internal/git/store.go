package git

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	IsWorld bool // true = subdirectory, false = .md file
}

// LogEntry represents a single git commit in a log listing.
type LogEntry struct {
	Commit  string
	Date    string
	Message string
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
// Subdirectories have IsWorld=true, .md files have IsWorld=false.
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
			entries = append(entries, DirEntry{Name: e.Name, IsWorld: true})
		} else if strings.HasSuffix(e.Name, ".md") {
			entries = append(entries, DirEntry{Name: e.Name, IsWorld: false})
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
