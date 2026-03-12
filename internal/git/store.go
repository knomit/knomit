// Package git provides a Git-backed knowledge store for knomit.
//
// The store uses go-git's plumbing API with a SQLite-backed storer — no
// filesystem reads or writes are performed. All fact data lives as git
// blobs/trees/commits inside the SQLite database.
//
// The package is split across several files:
//
//   - store.go    — Core types (Store, DirEntry, LogEntry, SyncResult),
//                   lifecycle (Init, Open, Close), and metadata accessors.
//   - read.go     — Read-only operations: ReadFile, FileExists, ListDir,
//                   ListAll, Log, Grep, DiffFiles.
//   - write.go    — Write operations: WriteFile, DeleteFile, BatchWrite,
//                   Tag, TagsContaining.
//   - sync.go     — Remote synchronization: Sync, countAhead, isAncestor,
//                   mergeTrees.
//   - plumbing.go — Low-level git tree manipulation: writeFileToStore,
//                   buildTree, upsertEntry, deleteFileFromStore,
//                   deleteFromTree, removeEntry.
//   - config.go   — Configuration helpers.
package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-billy/v5/memfs"

	"github.com/rs/zerolog/log"
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
	Name  string
	IsDir bool // true = subdirectory, false = .md file
}

// LogEntry represents a single git commit in a log listing.
type LogEntry struct {
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Message string `json:"message"`
}

// SyncResult is returned by Sync to report what happened during synchronization.
type SyncResult struct {
	Synced bool
	Ahead  int
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

	log.Info().Str("branch", agentBranch).Str("db", dbPath).Msg("git store initialized")
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

	log.Info().Str("branch", branch).Str("db", dbPath).Msg("git store opened")
	return &Store{
		repo:   repo,
		storer: s,
		dbPath: dbPath,
		branch: branch,
	}, nil
}

// Close closes the underlying gitstorer.
func (s *Store) Close() error {
	return s.storer.Close()
}

// Branch returns the agent branch name (e.g. "agent/laptop").
func (s *Store) Branch() string {
	return s.branch
}

// HeadCommit returns the hash of the current HEAD commit as a hex string.
func (s *Store) HeadCommit() (string, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return "", fmt.Errorf("HeadCommit: %w", err)
	}
	return headRef.Hash().String(), nil
}

// Storer returns the underlying gitstorer (used by the git remote handler).
func (s *Store) Storer() *gitstorer.Storer {
	return s.storer
}
