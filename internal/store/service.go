package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/store/git"
	"knomit/internal/store/migrate"
)

// BlobObjectType is the go-git integer for plumbing.BlobObject.
const BlobObjectType = 3

// Compile-time checks.
var _ BranchIndex      = (*repoHandler)(nil)
var _ gitReader        = (*factIndex)(nil)
var _ gitReader        = (*Service)(nil)
var _ FactIndex        = (*factIndex)(nil)
var _ SearchIndex      = (*searchIndex)(nil)
var _ PipelineIndex    = (*pipelineIndex)(nil)
var _ ToolSessionIndex = (*toolIndex)(nil)

// Service is the single entry point for all database and git access. It opens one
// SQLite file with sqlite-vec + GraphQLite extensions, runs the embedded
// schema, and provides both a go-git Storer and typed index accessors.
//
// Git-backed fact operations are delegated to fi (*factIndex), which is
// populated by the repo constructors (OpenRepo, InitRepo, etc.) and is nil/zero
// when Service is used in DB-only mode via Open().
type Service struct {
	rh          *repoHandler
	crypt       *Crypt // nil if no key material provided
	handlerOnce sync.Once
	handler     http.Handler
	fi          *factIndex
	si          *searchIndex
	pi          *pipelineIndex
	ti          *toolIndex
}

// Open opens (or creates) a unified SQLite database at path, initializes the
// schema, and returns a Service that provides access to both the git storer
// and the search index.
func Open(path string) (*Service, error) {
	registerVec() // one-time sqlite-vec + GraphQLite driver registration

	dsn := path
	if path == ":memory:" {
		dsn = path + "?_foreign_keys=1"
	} else {
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"
	}
	db, err := sql.Open("sqlite3_knomit", dsn)
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	// SQLite serializes writes internally; limiting the pool avoids SQLITE_BUSY
	// contention between pooled connections competing for the write lock.
	db.SetMaxOpenConns(4)

	// Per-connection performance pragmas are applied in the ConnectHook (vec.go)
	// so every pooled connection is configured, not just the first one.

	// Update query planner statistics (one-time hint, not per-connection).
	db.Exec("PRAGMA optimize")

	if err := migrate.All(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: %w", err)
	}

	rh := newRepoHandler(db)
	gits := storegit.NewStorer(db)
	si := &searchIndex{rh: rh}
	rh.onDrop = si.GC
	fi := &factIndex{rh: rh, gits: gits}
	fi.postCommit = si.Sync
	return &Service{
		rh: rh,
		fi: fi,
		si: si,
		pi: &pipelineIndex{rh: rh},
		ti: &toolIndex{rh: rh},
	}, nil
}

// SetCrypt sets the encryption provider for credential storage.
func (s *Service) SetCrypt(c *Crypt) { s.crypt = c }

// Facts returns the FactIndex for git-backed fact operations.
func (s *Service) Facts() FactIndex { return s.fi }

// Search returns the SearchIndex for full-text and vector search.
func (s *Service) Search() SearchIndex { return s.si }

// Pipeline returns the PipelineIndex for pipeline session management.
func (s *Service) Pipeline() PipelineIndex { return s.pi }

// ToolSession returns the ToolSessionIndex for tool session persistence.
func (s *Service) ToolSession() ToolSessionIndex { return s.ti }

// Branches returns the branch index.
func (s *Service) Branches() BranchIndex { return s.rh }

// Storer returns the go-git storer interface for use with git transport
// (e.g. server.MapLoader for in-memory cloning).
func (s *Service) Storer() storer.Storer { return s.fi.gits }

// Checkpoint flushes the WAL to the main database file so the .db file is
// self-contained (e.g. before file-level copy). This is a no-op if WAL mode
// is not enabled.
func (s *Service) Checkpoint() error {
	_, err := s.rh.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// Close closes the underlying database connection.
func (s *Service) Close() error { return s.rh.db.Close() }

// SetAuth sets the transport authentication method used by Sync and Push.
func (s *Service) SetAuth(auth transport.AuthMethod) {
	s.fi.auth = auth
}

// SetSigner sets the SSH signer used for commit signing.
func (s *Service) SetSigner(signer ssh.Signer) {
	s.fi.signer = signer
}

// SetOnCommit registers a callback invoked after every commit (after internal
// bookkeeping). This allows callers to observe commits for side-effects such
// as SSE broadcasting. The callback receives the branch name and commit hash.
func (s *Service) SetOnCommit(fn func(branch, hash string)) {
	s.fi.onCommit = fn
}

// CreateBranch creates a new branch ref pointing at the tip of fromBranch.
// No-op if branch already exists.
func (s *Service) CreateBranch(ctx context.Context, branch, fromBranch string) error {
	return s.fi.createBranch(ctx, branch, fromBranch)
}

// ConfigureRemote sets up (or reconfigures) the "origin" remote with the given
// URL and fetch refspec for branch. Idempotent — returns nil if already correct.
func (s *Service) ConfigureRemote(ctx context.Context, url, branch string) error {
	s.fi.configMu.Lock()
	defer s.fi.configMu.Unlock()

	cfg, err := s.fi.repo.Config()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch)

	if rc, ok := cfg.Remotes["origin"]; ok {
		if len(rc.URLs) > 0 && rc.URLs[0] == url {
			for _, rs := range rc.Fetch {
				if string(rs) == refspec {
					return nil // already configured
				}
			}
		}
	}

	// Delete existing origin if present, then create fresh.
	_ = s.fi.repo.DeleteRemote("origin")
	_, err = s.fi.repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{url},
		Fetch: []gogitconfig.RefSpec{
			gogitconfig.RefSpec(refspec),
		},
	})
	if err != nil {
		return fmt.Errorf("create remote: %w", err)
	}
	return nil
}

// DefaultBranch resolves the default branch name from the repo's HEAD ref.
func (s *Service) DefaultBranch(ctx context.Context) (string, error) {
	head, err := s.fi.gits.Reference(plumbing.HEAD)
	if err != nil {
		return "", fmt.Errorf("DefaultBranch: resolve HEAD: %w", err)
	}
	if head.Type() == plumbing.SymbolicReference {
		return strings.TrimPrefix(head.Target().String(), "refs/heads/"), nil
	}
	// Detached HEAD — return empty string.
	return "", nil
}

// SetDefaultBranch sets the symbolic HEAD to point at the given branch.
func (s *Service) SetDefaultBranch(branch string) error {
	return s.fi.gits.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branch)),
	)
}

// HasSharedHistory checks whether localBranch shares any commits with remoteBranch on the remote service.
// Uses a bounded walk (max 1000 commits) to avoid scanning huge histories.
func (s *Service) HasSharedHistory(ctx context.Context, localBranch string, remote *Service, remoteBranch string) (bool, error) {
	const maxCommits = 1000

	localHash, err := s.fi.resolveRef(ctx, localBranch)
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: local ref: %w", err)
	}
	localHashes := make(map[plumbing.Hash]struct{})
	localIter, err := s.fi.repo.Log(&gogit.LogOptions{From: localHash})
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

	remoteHash, err := remote.fi.resolveRef(ctx, remoteBranch)
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: remote ref: %w", err)
	}
	remoteIter, err := remote.fi.repo.Log(&gogit.LogOptions{From: remoteHash})
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

// gitReader forwarding methods — delegate to fi so *Service satisfies gitReader
// and can be passed to SearchIndex.Sync / SearchIndex.Rebuild.

func (s *Service) DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error) {
	return s.fi.DiffFiles(ctx, branch, fromCommit)
}

func (s *Service) readFile(ctx context.Context, branch, path string) (string, error) {
	return s.fi.readFile(ctx, branch, path)
}

func (s *Service) readFileWithHash(ctx context.Context, branch, path string) (string, string, error) {
	return s.fi.readFileWithHash(ctx, branch, path)
}

func (s *Service) HeadCommit(ctx context.Context, branch string) (string, error) {
	return s.fi.HeadCommit(ctx, branch)
}

func (s *Service) ListAll(ctx context.Context, branch string) ([]string, error) {
	return s.fi.ListAll(ctx, branch)
}

func (s *Service) ListAllWithHash(ctx context.Context, branch string) ([]string, []string, error) {
	return s.fi.ListAllWithHash(ctx, branch)
}

func (s *Service) LastCommitForPath(ctx context.Context, branch, path string) (string, error) {
	return s.fi.LastCommitForPath(ctx, branch, path)
}

func (s *Service) readFileAtCommit(ctx context.Context, branch, path, commitHash string) (string, error) {
	return s.fi.readFileAtCommit(ctx, branch, path, commitHash)
}

// deriveAgentID extracts the agent identifier from a branch name.
// "agent/laptop-abc" → "laptop-abc", "main" → "main".
func deriveAgentID(branch string) string {
	if after, ok := strings.CutPrefix(branch, "agent/"); ok {
		return after
	}
	return branch
}
