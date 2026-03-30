// Package git provides a Git-backed knowledge store for knomit.
//
// The store uses go-git's plumbing API with a SQLite-backed storer — no
// filesystem reads or writes are performed. All fact data lives as git
// blobs/trees/commits inside the SQLite database.
//
// The package is split across several files:
//
//   - store.go    — Core types (Store, DirEntry, LogEntry, SyncResult),
//     lifecycle (Init, Open, Close), and metadata accessors.
//   - read.go     — Read-only operations: ReadFile, FileExists, ListDir,
//     ListAll, Log, Grep, DiffFiles.
//   - write.go    — Write operations: WriteFile, DeleteFile, BatchWrite,
//     Tag, TagsContaining.
//   - sync.go     — Remote synchronization: Sync, countAhead, isAncestor,
//     mergeTrees.
//   - plumbing.go — Low-level git tree manipulation: writeFileToStore,
//     buildTree, upsertEntry, deleteFileFromStore,
//     deleteFromTree, removeEntry.
//   - config.go   — Configuration helpers.
package git

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/store/git"

	"github.com/rs/zerolog/log"
)

// Store wraps go-git with knomit's logical operations.
// All fact reads/writes go through go-git's plumbing API — NO filesystem reads/writes.
type Store struct {
	branchMu    sync.Map     // keyed by branch name → *sync.Mutex
	configMu    sync.Mutex   // guards ConfigureRemote
	repo        *gogit.Repository
	storer      *storegit.Storer
	auth        transport.AuthMethod
	signer      ssh.Signer // signs commits when set
	onCommit    func(branch, hash string)
	handlerOnce sync.Once
	handler     http.Handler
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

// resolveRef returns the commit hash at the tip of branch.
func (s *Store) resolveRef(ctx context.Context, branch string) (plumbing.Hash, error) {
	ref, err := s.storer.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolveRef %q: %w", branch, err)
	}
	return ref.Hash(), nil
}

// lockBranch acquires the per-branch mutex and returns an unlock function.
// Redundant with SQLite write serialization at the Index level, but kept until
// go-git's storer interface supports context-propagated transactions.
func (s *Store) lockBranch(branch string) func() {
	v, _ := s.branchMu.LoadOrStore(branch, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

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
		repo:   repo,
		storer: s,
	}
	if err := gs.populateCommitLog(context.Background(), agentBranch); err != nil {
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
		repo:   repo,
		storer: s,
	}
	if err := gs.populateCommitLog(context.Background(), branch); err != nil {
		log.Warn().Err(err).Msg("commit_log: open populate failed")
	}
	return gs, nil
}

// Close is a no-op. The database lifecycle is managed by the caller (via store.Service).
func (s *Store) Close(_ context.Context) error { return nil }

// deriveAgentID extracts the agent identifier from a branch name.
// "agent/laptop-abc" → "laptop-abc", "main" → "main".
func deriveAgentID(branch string) string {
	if after, ok := strings.CutPrefix(branch, "agent/"); ok {
		return after
	}
	return branch
}

// authorSig returns the author signature for a given operation.
func (s *Store) authorSig(branch, operation string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "+" + operation + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// committerSig returns the committer signature (stable per agent).
func (s *Store) committerSig(branch string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// HeadCommit returns the hash of the tip commit of branch as a hex string.
func (s *Store) HeadCommit(ctx context.Context, branch string) (string, error) {
	hash, err := s.resolveRef(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("HeadCommit: %w", err)
	}
	return hash.String(), nil
}

// SetOnCommit registers a callback invoked after every branch ref update.
// Must be called before any writes (during init).
func (s *Store) SetOnCommit(fn func(branch, hash string)) {
	s.onCommit = fn
}

func (s *Store) notifyCommit(branch, hash string) {
	if s.onCommit != nil {
		s.onCommit(branch, hash)
	}
}

// CreateBranch creates a new branch ref pointing at the tip of fromBranch.
// No-op if branch already exists. Does not modify HEAD or any Store state.
func (s *Store) CreateBranch(ctx context.Context, branch, fromBranch string) error {
	newRefName := plumbing.NewBranchReferenceName(branch)
	if _, err := s.storer.Reference(newRefName); err == nil {
		return nil // already exists
	}
	fromHash, err := s.resolveRef(ctx, fromBranch)
	if err != nil {
		return fmt.Errorf("CreateBranch: resolve source %q: %w", fromBranch, err)
	}
	if err := s.storer.SetReference(plumbing.NewHashReference(newRefName, fromHash)); err != nil {
		return fmt.Errorf("CreateBranch: set ref: %w", err)
	}
	log.Info().Str("branch", branch).Str("from", fromBranch).Msg("created branch")
	return nil
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
			repo:   repo,
			storer: s,
			auth:   auth,
		}
		if err := gs.populateCommitLog(context.Background(), agentBranch); err != nil {
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
		repo:   repo,
		storer: s,
		auth:   auth,
	}
	if err := gs.populateCommitLog(context.Background(), agentBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: remote populate failed")
	}
	return gs, nil
}

// DefaultBranch resolves the default branch name from the repo's HEAD ref.
func (s *Store) DefaultBranch(ctx context.Context) (string, error) {
	head, err := s.storer.Reference(plumbing.HEAD)
	if err != nil {
		return "", fmt.Errorf("DefaultBranch: resolve HEAD: %w", err)
	}
	if head.Type() == plumbing.SymbolicReference {
		return strings.TrimPrefix(head.Target().String(), "refs/heads/"), nil
	}
	// Detached HEAD — return empty string.
	return "", nil
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
		repo:   repo,
		storer: storer,
		auth:   auth,
	}, nil
}

// HasSharedHistory checks whether localBranch shares any commits with remoteBranch on the remote store.
// Uses a bounded walk (max 1000 commits) to avoid scanning huge histories.
func (s *Store) HasSharedHistory(ctx context.Context, localBranch string, remote *Store, remoteBranch string) (bool, error) {
	const maxCommits = 1000

	localHash, err := s.resolveRef(ctx, localBranch)
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: local ref: %w", err)
	}
	localHashes := make(map[plumbing.Hash]struct{})
	localIter, err := s.repo.Log(&gogit.LogOptions{From: localHash})
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

	remoteHash, err := remote.resolveRef(ctx, remoteBranch)
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: remote ref: %w", err)
	}
	remoteIter, err := remote.repo.Log(&gogit.LogOptions{From: remoteHash})
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
	}); err != nil && !found {
		return false, fmt.Errorf("HasSharedHistory: remote walk: %w", err)
	}
	return found, nil
}
