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
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-billy/v5/memfs"
	_ "github.com/mattn/go-sqlite3"

	"github.com/rs/zerolog/log"
	storegit "knomit/internal/store/git"
)

// Store wraps go-git with knomit's logical operations.
// All fact reads/writes go through go-git's plumbing API — NO filesystem reads/writes.
type Store struct {
	mu       sync.Mutex
	repo     *gogit.Repository
	storer   *storegit.Storer
	ownsDB   bool    // true when Init/Open opened the DB (legacy path)
	ownedDB  *sql.DB // non-nil when ownsDB is true
	branch   string  // e.g. "agent/laptop"
	onCommit func(hash string)
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

// ChangedFile represents a file changed in a commit.
type ChangedFile struct {
	Path   string `json:"path"`
	Action string `json:"action"` // "added", "modified", "deleted"
}

// CommitDetailResult contains metadata and changed files for a single commit.
type CommitDetailResult struct {
	Commit  string        `json:"commit"`
	Date    string        `json:"date"`
	Message string        `json:"message"`
	Tags    []string      `json:"tags"`
	Files   []ChangedFile `json:"files"`
}

// LogEntryWithTags extends LogEntry with tag names associated with the commit.
type LogEntryWithTags struct {
	Commit  string   `json:"commit"`
	Date    string   `json:"date"`
	Message string   `json:"message"`
	Tags    []string `json:"tags"`
}

// SyncResult is returned by Sync to report what happened during synchronization.
type SyncResult struct {
	Synced      bool   // true if tree changed (merge or fast-forward)
	FastForward bool   // true if fast-forward (no merge commit)
	MergeCommit string // hash of merge commit (empty if ff or no-op)
}

// gitSchema is the minimal schema for standalone Init/Open (legacy path).
const gitSchema = `
CREATE TABLE IF NOT EXISTS objects (hash TEXT NOT NULL, type INTEGER NOT NULL, size INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY (hash, type));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target TEXT NOT NULL, is_symbolic INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value BLOB NOT NULL);
`

// InitWithStorer creates a new knomit git store using an externally provided storer.
// The storer's schema must already be applied.
func InitWithStorer(s *storegit.Storer, initFiles map[string]string) (*Store, error) {
	repo, err := gogit.Init(s, memfs.New())
	if err != nil {
		return nil, fmt.Errorf("git.Init: git init: %w", err)
	}

	cfg, err := repo.Config()
	if err != nil {
		return nil, fmt.Errorf("git.Init: read config: %w", err)
	}
	cfg.User.Name = "knomit"
	cfg.User.Email = "knomit@local"
	if cfg.Raw != nil {
		cfg.Raw.Section("commit").SetOption("gpgsign", "false")
	}
	if err := repo.SetConfig(cfg); err != nil {
		return nil, fmt.Errorf("git.Init: set config: %w", err)
	}

	rootManifest := "# Knowledge Base\n\nRoot manifest.\n"
	lastCommit, _, err := writeFileToStore(s, plumbing.ZeroHash, "kb.md", rootManifest, "init: create knowledge base")
	if err != nil {
		return nil, fmt.Errorf("git.Init: initial commit: %w", err)
	}

	for path, content := range initFiles {
		lastCommit, _, err = writeFileToStore(s, lastCommit, path, content, "init: "+path)
		if err != nil {
			return nil, fmt.Errorf("git.Init: write %s: %w", path, err)
		}
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "local"
	}
	agentBranch := "agent/" + hostname
	agentRefName := plumbing.NewBranchReferenceName(agentBranch)

	agentRef := plumbing.NewHashReference(agentRefName, lastCommit)
	if err := s.SetReference(agentRef); err != nil {
		return nil, fmt.Errorf("git.Init: set agent ref: %w", err)
	}

	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)
	if err := s.SetReference(headRef); err != nil {
		return nil, fmt.Errorf("git.Init: set HEAD: %w", err)
	}

	mainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), lastCommit)
	if err := s.SetReference(mainRef); err != nil {
		return nil, fmt.Errorf("git.Init: set main ref: %w", err)
	}

	log.Info().Str("branch", agentBranch).Msg("git store initialized")
	return &Store{
		repo:   repo,
		storer: s,
		branch: agentBranch,
	}, nil
}

// OpenWithStorer opens an existing knomit git store using an externally provided storer.
func OpenWithStorer(s *storegit.Storer) (*Store, error) {
	repo, err := gogit.Open(s, memfs.New())
	if err != nil {
		return nil, fmt.Errorf("git.Open: git open: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("git.Open: read HEAD: %w", err)
	}

	branch := strings.TrimPrefix(head.Name().String(), "refs/heads/")

	log.Info().Str("branch", branch).Msg("git store opened")
	return &Store{
		repo:   repo,
		storer: s,
		branch: branch,
	}, nil
}

// Init creates a new knomit git store at dbPath.
// Deprecated: use store.Open + InitWithStorer instead.
func Init(dbPath string, initFiles map[string]string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("git.Init: mkdir: %w", err)
	}

	dsn := dbPath
	if dbPath != ":memory:" {
		dsn = dbPath + "?_journal_mode=WAL&_busy_timeout=5000"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("git.Init: open db: %w", err)
	}
	if _, err := db.Exec(gitSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("git.Init: schema: %w", err)
	}

	s := storegit.NewStorer(db)
	store, err := InitWithStorer(s, initFiles)
	if err != nil {
		db.Close()
		return nil, err
	}
	store.ownsDB = true
	store.ownedDB = db
	return store, nil
}

// Open opens an existing knomit git store at dbPath.
// Deprecated: use store.Open + OpenWithStorer instead.
func Open(dbPath string) (*Store, error) {
	dsn := dbPath
	if dbPath != ":memory:" {
		dsn = dbPath + "?_journal_mode=WAL&_busy_timeout=5000"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("git.Open: open db: %w", err)
	}
	if _, err := db.Exec(gitSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("git.Open: schema: %w", err)
	}

	s := storegit.NewStorer(db)
	store, err := OpenWithStorer(s)
	if err != nil {
		db.Close()
		return nil, err
	}
	store.ownsDB = true
	store.ownedDB = db
	return store, nil
}

// Close closes the underlying database if this Store owns it.
func (s *Store) Close() error {
	if s.ownsDB && s.ownedDB != nil {
		return s.ownedDB.Close()
	}
	return nil
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

// SetOnCommit registers a callback invoked after every branch ref update.
// Must be called before any writes (during init).
func (s *Store) SetOnCommit(fn func(hash string)) {
	s.onCommit = fn
}

func (s *Store) notifyCommit(hash string) {
	if s.onCommit != nil {
		s.onCommit(hash)
	}
}

// Storer returns the underlying storer (used by the git remote handler).
func (s *Store) Storer() *storegit.Storer {
	return s.storer
}
