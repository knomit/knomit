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
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-billy/v5/memfs"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/ssh"

	"github.com/rs/zerolog/log"
	storegit "knomit/internal/store/git"
)

// Store wraps go-git with knomit's logical operations.
// All fact reads/writes go through go-git's plumbing API — NO filesystem reads/writes.
type Store struct {
	mu        sync.Mutex
	repo      *gogit.Repository
	storer    *storegit.Storer
	db        *sql.DB // non-nil when commit_log is available
	commitLog bool    // true once commit_log table is confirmed populated
	ownsDB    bool    // true when Init/Open opened the DB (legacy path)
	ownedDB   *sql.DB // non-nil when ownsDB is true
	branch    string  // e.g. "agent/laptop"
	agentID   string  // e.g. "laptop" (branch with "agent/" prefix stripped)
	auth      transport.AuthMethod
	signer    ssh.Signer // signs commits when set
	onCommit  func(hash string)
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

// FileRecency represents a file path and the timestamp of the commit that last changed it.
type FileRecency struct {
	Path      string
	Timestamp time.Time
}

// CommitDetailResult contains metadata and changed files for a single commit.
type CommitDetailResult struct {
	Commit    string        `json:"commit"`
	Date      string        `json:"date"`
	Message   string        `json:"message"`
	Operation string        `json:"operation,omitempty"`
	Files     []ChangedFile `json:"files"`
}

// FileCounts summarizes the number of files added, modified, and deleted in a commit.
type FileCounts struct {
	Added    int `json:"added,omitempty"`
	Modified int `json:"modified,omitempty"`
	Deleted  int `json:"deleted,omitempty"`
}

// LogEntryWithTags extends LogEntry with tag names associated with the commit.
type LogEntryWithTags struct {
	Commit    string     `json:"commit"`
	Date      string     `json:"date"`
	Message   string     `json:"message"`
	Operation string     `json:"operation,omitempty"`
	Files     FileCounts `json:"files,omitempty"`
}

// ActivityResult holds commit-activity metrics for a path over several time windows.
type ActivityResult struct {
	LastCommit string `json:"last_commit"` // ISO-8601 timestamp of most recent commit, or ""
	Total      int    `json:"total"`       // total commits touching this path
	Changes7d  int    `json:"changes_7d"`
	Changes30d int    `json:"changes_30d"`
	Changes90d int    `json:"changes_90d"`
}

// SyncResult is returned by Sync to report what happened during synchronization.
type SyncResult struct {
	Synced      bool   // true if tree changed (merge or fast-forward)
	FastForward bool   // true if fast-forward (no merge commit)
	MergeCommit string // hash of merge commit (empty if ff or no-op)
}

// gitSchema is the minimal schema for standalone Init/Open (legacy path).
// Includes commit_log so SQL-based Activity and WalkChangedFiles work.
const gitSchema = `
CREATE TABLE IF NOT EXISTS objects (hash TEXT NOT NULL, type INTEGER NOT NULL, size INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY (hash, type));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target TEXT NOT NULL, is_symbolic INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS commit_log (commit_hash TEXT NOT NULL, path TEXT NOT NULL, committed_at INTEGER NOT NULL, message TEXT NOT NULL, operation TEXT NOT NULL DEFAULT '', author_email TEXT NOT NULL DEFAULT '', action TEXT NOT NULL DEFAULT '', PRIMARY KEY (commit_hash, path));
CREATE INDEX IF NOT EXISTS commit_log_path_time ON commit_log (path, committed_at DESC);
CREATE INDEX IF NOT EXISTS commit_log_time ON commit_log (committed_at DESC);
CREATE INDEX IF NOT EXISTS commit_log_operation ON commit_log (operation, committed_at DESC);
`

// InitWithStorer creates a new knomit git store using an externally provided storer.
// The storer's schema must already be applied.
func InitWithStorer(s *storegit.Storer, initFiles map[string]string, agentBranch string) (*Store, error) {
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
	initSig := object.Signature{Name: "knomit", Email: "knomit@local", When: time.Now()}
	lastCommit, _, err := writeFileToStore(s, plumbing.ZeroHash, "kb.md", rootManifest, "init: create knowledge base", initSig, initSig)
	if err != nil {
		return nil, fmt.Errorf("git.Init: initial commit: %w", err)
	}

	for path, content := range initFiles {
		lastCommit, _, err = writeFileToStore(s, lastCommit, path, content, "init: "+path, initSig, initSig)
		if err != nil {
			return nil, fmt.Errorf("git.Init: write %s: %w", path, err)
		}
	}

	if agentBranch == "" {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "local"
		}
		agentBranch = "agent/" + hostname
	}
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
	gs := &Store{
		repo:    repo,
		storer:  s,
		db:      s.DB(),
		branch:  agentBranch,
		agentID: deriveAgentID(agentBranch),
	}
	if err := gs.populateCommitLog(); err != nil {
		log.Warn().Err(err).Msg("commit_log: initial populate failed")
	}
	return gs, nil
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
	gs := &Store{
		repo:    repo,
		storer:  s,
		db:      s.DB(),
		branch:  branch,
		agentID: deriveAgentID(branch),
	}
	if err := gs.populateCommitLog(); err != nil {
		log.Warn().Err(err).Msg("commit_log: open populate failed")
	}
	return gs, nil
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
	store, err := InitWithStorer(s, initFiles, "")
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

// deriveAgentID extracts the agent identifier from a branch name.
// "agent/laptop-abc" → "laptop-abc", "main" → "main".
func deriveAgentID(branch string) string {
	if after, ok := strings.CutPrefix(branch, "agent/"); ok {
		return after
	}
	return branch
}

// AgentID returns the agent identifier derived from the branch name.
func (s *Store) AgentID() string { return s.agentID }

// authorSig returns the author signature for a given operation.
func (s *Store) authorSig(operation string) object.Signature {
	return object.Signature{
		Name:  s.agentID,
		Email: s.agentID + "+" + operation + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// committerSig returns the committer signature (stable per agent).
func (s *Store) committerSig() object.Signature {
	return object.Signature{
		Name:  s.agentID,
		Email: s.agentID + "@agents.knomit.io",
		When:  time.Now(),
	}
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

// SwitchBranch creates a new branch from the current HEAD and switches to it.
// The old branch is left intact. No-op if already on the target branch.
func (s *Store) SwitchBranch(newBranch string) error {
	if s.branch == newBranch {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Resolve current HEAD commit.
	head, err := s.repo.Head()
	if err != nil {
		return fmt.Errorf("SwitchBranch: read HEAD: %w", err)
	}

	newRefName := plumbing.NewBranchReferenceName(newBranch)

	// Create new branch pointing at current HEAD.
	if err := s.storer.SetReference(plumbing.NewHashReference(newRefName, head.Hash())); err != nil {
		return fmt.Errorf("SwitchBranch: set new ref: %w", err)
	}

	// Update HEAD to point to new branch.
	if err := s.storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, newRefName)); err != nil {
		return fmt.Errorf("SwitchBranch: update HEAD: %w", err)
	}

	log.Info().Str("from", s.branch).Str("to", newBranch).Msg("switched branch")
	s.branch = newBranch
	return nil
}

// Storer returns the underlying storer (used by the git remote handler).
func (s *Store) Storer() *storegit.Storer {
	return s.storer
}

// SetAuth sets the transport authentication method used by Sync and Push.
func (s *Store) SetAuth(auth transport.AuthMethod) {
	s.auth = auth
}

// SetSigner sets the SSH signer used for commit signing.
func (s *Store) SetSigner(signer ssh.Signer) {
	s.signer = signer
}

// InitFromRemote initializes a knomit git store by fetching from a remote origin.
// If the remote has an existing agent branch for this hostname, it is used.
// Otherwise a new agent branch is created from origin/main.
// If the remote is empty (no refs), falls back to InitWithStorer.
func InitFromRemote(s *storegit.Storer, originURL string, auth transport.AuthMethod, agentBranch string) (*Store, error) {
	repo, err := gogit.Init(s, memfs.New())
	if err != nil {
		return nil, fmt.Errorf("InitFromRemote: git init: %w", err)
	}

	cfg, err := repo.Config()
	if err != nil {
		return nil, fmt.Errorf("InitFromRemote: read config: %w", err)
	}
	cfg.User.Name = "knomit"
	cfg.User.Email = "knomit@local"
	if cfg.Raw != nil {
		cfg.Raw.Section("commit").SetOption("gpgsign", "false")
	}
	if err := repo.SetConfig(cfg); err != nil {
		return nil, fmt.Errorf("InitFromRemote: set config: %w", err)
	}

	_, err = repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{originURL},
		Fetch: []gogitconfig.RefSpec{
			"+refs/heads/*:refs/remotes/origin/*",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("InitFromRemote: create remote: %w", err)
	}

	err = repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
	})
	if err == transport.ErrEmptyRemoteRepository {
		// Repo already initialized above; create initial content inline
		// (can't call InitWithStorer which would try gogit.Init again).
		rootManifest := "# Knowledge Base\n\nRoot manifest.\n"
		initSig := object.Signature{Name: "knomit", Email: "knomit@local", When: time.Now()}
		lastCommit, _, writeErr := writeFileToStore(s, plumbing.ZeroHash, "kb.md", rootManifest, "init: create knowledge base", initSig, initSig)
		if writeErr != nil {
			return nil, fmt.Errorf("InitFromRemote: empty remote fallback: %w", writeErr)
		}
		if agentBranch == "" {
			hostname, _ := os.Hostname()
			if hostname == "" {
				hostname = "local"
			}
			agentBranch = "agent/" + hostname
		}
		agentRefName := plumbing.NewBranchReferenceName(agentBranch)
		if writeErr = s.SetReference(plumbing.NewHashReference(agentRefName, lastCommit)); writeErr != nil {
			return nil, fmt.Errorf("InitFromRemote: empty remote set agent ref: %w", writeErr)
		}
		if writeErr = s.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)); writeErr != nil {
			return nil, fmt.Errorf("InitFromRemote: empty remote set HEAD: %w", writeErr)
		}
		if writeErr = s.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), lastCommit)); writeErr != nil {
			return nil, fmt.Errorf("InitFromRemote: empty remote set main: %w", writeErr)
		}
		log.Info().Str("branch", agentBranch).Str("origin", originURL).Msg("git store initialized (empty remote)")
		gs := &Store{
			repo:    repo,
			storer:  s,
			db:      s.DB(),
			branch:  agentBranch,
			agentID: deriveAgentID(agentBranch),
			auth:    auth,
		}
		if err := gs.populateCommitLog(); err != nil {
			log.Warn().Err(err).Msg("commit_log: empty-remote populate failed")
		}
		return gs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("InitFromRemote: fetch: %w", err)
	}

	if agentBranch == "" {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "local"
		}
		agentBranch = "agent/" + hostname
	}
	agentRefName := plumbing.NewBranchReferenceName(agentBranch)

	// Check for existing remote agent branch.
	remoteAgentRef, err := s.Reference(plumbing.NewRemoteReferenceName("origin", agentBranch))
	if err == nil {
		// Remote agent branch exists — use it.
		localRef := plumbing.NewHashReference(agentRefName, remoteAgentRef.Hash())
		if err := s.SetReference(localRef); err != nil {
			return nil, fmt.Errorf("InitFromRemote: set agent ref: %w", err)
		}
	} else {
		// No remote agent branch — create from origin/main.
		originMainRef, err := s.Reference(plumbing.NewRemoteReferenceName("origin", "main"))
		if err != nil {
			return nil, fmt.Errorf("InitFromRemote: resolve origin/main: %w", err)
		}
		localRef := plumbing.NewHashReference(agentRefName, originMainRef.Hash())
		if err := s.SetReference(localRef); err != nil {
			return nil, fmt.Errorf("InitFromRemote: set agent ref from main: %w", err)
		}
	}

	// Set HEAD to agent branch.
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)
	if err := s.SetReference(headRef); err != nil {
		return nil, fmt.Errorf("InitFromRemote: set HEAD: %w", err)
	}

	// Create local main pointing at origin/main.
	originMainRef, err := s.Reference(plumbing.NewRemoteReferenceName("origin", "main"))
	if err != nil {
		return nil, fmt.Errorf("InitFromRemote: resolve origin/main for local main: %w", err)
	}
	mainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), originMainRef.Hash())
	if err := s.SetReference(mainRef); err != nil {
		return nil, fmt.Errorf("InitFromRemote: set main ref: %w", err)
	}

	log.Info().Str("branch", agentBranch).Str("origin", originURL).Msg("git store initialized from remote")
	gs := &Store{
		repo:    repo,
		storer:  s,
		db:      s.DB(),
		branch:  agentBranch,
		agentID: deriveAgentID(agentBranch),
		auth:    auth,
	}
	if err := gs.populateCommitLog(); err != nil {
		log.Warn().Err(err).Msg("commit_log: remote populate failed")
	}
	return gs, nil
}

// DefaultBranch resolves the default branch name from the repo's HEAD ref.
func (s *Store) DefaultBranch() (string, error) {
	head, err := s.storer.Reference(plumbing.HEAD)
	if err != nil {
		return "", fmt.Errorf("DefaultBranch: resolve HEAD: %w", err)
	}
	if head.Type() == plumbing.SymbolicReference {
		return strings.TrimPrefix(head.Target().String(), "refs/heads/"), nil
	}
	// Detached HEAD — fall back to the branch field.
	return s.branch, nil
}

// progressWriter adapts a progress callback to io.Writer for use with CloneOptions.Progress.
type progressWriter struct {
	fn func(string)
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	pw.fn(string(p))
	return len(p), nil
}

// CloneInto clones the remote URL into the given storer, returning a new Store.
func CloneInto(storer *storegit.Storer, url string, auth transport.AuthMethod, progress func(string)) (*Store, error) {
	opts := &gogit.CloneOptions{
		URL:  url,
		Auth: auth,
	}
	if progress != nil {
		opts.Progress = &progressWriter{fn: progress}
	}

	repo, err := gogit.Clone(storer, memfs.New(), opts)
	if err != nil {
		return nil, fmt.Errorf("CloneInto: clone: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("CloneInto: resolve HEAD: %w", err)
	}
	branch := strings.TrimPrefix(head.Name().String(), "refs/heads/")

	log.Info().Str("branch", branch).Str("url", url).Msg("cloned remote into storer")
	return &Store{
		repo:    repo,
		storer:  storer,
		db:      storer.DB(),
		branch:  branch,
		agentID: deriveAgentID(branch),
		auth:    auth,
	}, nil
}

// HasSharedHistory checks whether the current repo shares any commits with the given remote store.
// Uses a bounded walk (max 1000 commits) to avoid scanning huge histories.
func (s *Store) HasSharedHistory(remote *Store) (bool, error) {
	const maxCommits = 1000

	// Collect local commit hashes.
	localHashes := make(map[plumbing.Hash]struct{})
	localHead, err := s.repo.Head()
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: local HEAD: %w", err)
	}
	localIter, err := s.repo.Log(&gogit.LogOptions{From: localHead.Hash()})
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: local log: %w", err)
	}
	count := 0
	if err := localIter.ForEach(func(c *object.Commit) error {
		if count >= maxCommits {
			return storer.ErrStop
		}
		localHashes[c.Hash] = struct{}{}
		count++
		return nil
	}); err != nil {
		return false, fmt.Errorf("HasSharedHistory: local walk: %w", err)
	}

	// Walk remote commits and check for overlap.
	remoteHead, err := remote.repo.Head()
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: remote HEAD: %w", err)
	}
	remoteIter, err := remote.repo.Log(&gogit.LogOptions{From: remoteHead.Hash()})
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: remote log: %w", err)
	}
	count = 0
	found := false
	if err := remoteIter.ForEach(func(c *object.Commit) error {
		if count >= maxCommits {
			return storer.ErrStop
		}
		if _, ok := localHashes[c.Hash]; ok {
			found = true
			return storer.ErrStop
		}
		count++
		return nil
	}); err != nil {
		return false, fmt.Errorf("HasSharedHistory: remote walk: %w", err)
	}

	return found, nil
}
