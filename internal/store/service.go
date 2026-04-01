package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/store/git"
	"knomit/internal/store/migrate"
)

// BlobObjectType is the go-git integer for plumbing.BlobObject.
const BlobObjectType = 3

// Compile-time check that *Service satisfies the GitReader interface.
var _ GitReader = (*Service)(nil)

// Service is the single entry point for all database and git access. It opens one
// SQLite file with sqlite-vec + GraphQLite extensions, runs the embedded
// schema, and provides both a go-git Storer and an Index over the shared *sql.DB.
//
// The git-related fields (repo, branchMu, configMu, auth, signer, handler,
// handlerOnce) are populated by the repo constructors (OpenRepo, InitRepo, etc.)
// and are nil/zero when Service is used in DB-only mode via Open().
type Service struct {
	db    *sql.DB
	idx   *Index
	gits  *storegit.Storer
	crypt *Crypt // nil if no key material provided

	// Git-store fields — populated by OpenRepo / InitRepo / CloneFrom / InitFromRemote.
	repo        *gogit.Repository
	branchMu    sync.Map   // keyed by branch name → *sync.Mutex
	configMu    sync.Mutex // guards configureRemote
	auth        transport.AuthMethod
	signer      ssh.Signer // signs commits when set
	handlerOnce sync.Once
	handler     http.Handler
	onCommit    func(branch, hash string) // optional external callback
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

	gits := storegit.NewStorer(db)
	idx := newIndex(db)

	return &Service{db: db, idx: idx, gits: gits}, nil
}

// SetCrypt sets the encryption provider for credential storage.
func (s *Service) SetCrypt(c *Crypt) { s.crypt = c }

// Index returns the search index.
func (s *Service) Index() *Index { return s.idx }

// Storer returns the go-git storer interface for use with git transport
// (e.g. server.MapLoader for in-memory cloning).
func (s *Service) Storer() storer.Storer { return s.gits }

// Checkpoint flushes the WAL to the main database file so the .db file is
// self-contained (e.g. before file-level copy). This is a no-op if WAL mode
// is not enabled.
func (s *Service) Checkpoint() error {
	_, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// Close closes the underlying database connection.
func (s *Service) Close() error { return s.db.Close() }

// DeleteFact deletes a fact from the git store on the given branch and
// syncs the index so the deletion is immediately visible.
func (s *Service) DeleteFact(ctx context.Context, branch, path, message string) error {
	if _, err := s.DeleteFile(ctx, branch, path, message, "retract"); err != nil {
		return fmt.Errorf("DeleteFact git: %w", err)
	}

	// Sync the index so the deletion is reflected immediately.
	if err := s.idx.Sync(ctx, s, branch); err != nil {
		return fmt.Errorf("DeleteFact sync: %w", err)
	}

	return nil
}

// SetAuth sets the transport authentication method used by Sync and Push.
func (s *Service) SetAuth(auth transport.AuthMethod) {
	s.auth = auth
}

// SetSigner sets the SSH signer used for commit signing.
func (s *Service) SetSigner(signer ssh.Signer) {
	s.signer = signer
}

// resolveRef returns the commit hash at the tip of branch.
func (s *Service) resolveRef(ctx context.Context, branch string) (plumbing.Hash, error) {
	ref, err := s.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolveRef %q: %w", branch, err)
	}
	return ref.Hash(), nil
}

// lockBranch acquires the per-branch mutex and returns an unlock function.
func (s *Service) lockBranch(branch string) func() {
	v, _ := s.branchMu.LoadOrStore(branch, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// deriveAgentID extracts the agent identifier from a branch name.
// "agent/laptop-abc" → "laptop-abc", "main" → "main".
func deriveAgentID(branch string) string {
	if after, ok := strings.CutPrefix(branch, "agent/"); ok {
		return after
	}
	return branch
}

// authorSig returns the author signature for a given operation.
func (s *Service) authorSig(branch, operation string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "+" + operation + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// committerSig returns the committer signature (stable per agent).
func (s *Service) committerSig(branch string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// SetOnCommit registers a callback invoked after every commit (after internal
// bookkeeping). This allows callers to observe commits for side-effects such
// as SSE broadcasting. The callback receives the branch name and commit hash.
func (s *Service) SetOnCommit(fn func(branch, hash string)) {
	s.onCommit = fn
}

// notifyCommit calls appendCommitLog directly and then the optional external
// callback registered via SetOnCommit.
func (s *Service) notifyCommit(ctx context.Context, branch string, hash plumbing.Hash) {
	s.appendCommitLog(ctx, branch, hash)
	if s.onCommit != nil {
		s.onCommit(branch, hash.String())
	}
}

// HeadCommit returns the hash of the tip commit of branch as a hex string.
func (s *Service) HeadCommit(ctx context.Context, branch string) (string, error) {
	hash, err := s.resolveRef(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("HeadCommit: %w", err)
	}
	return hash.String(), nil
}

// createBranch creates a new branch ref pointing at the tip of fromBranch.
// No-op if branch already exists.
func (s *Service) createBranch(ctx context.Context, branch, fromBranch string) error {
	newRefName := plumbing.NewBranchReferenceName(branch)
	if _, err := s.gits.Reference(newRefName); err == nil {
		return nil // already exists
	}
	fromHash, err := s.resolveRef(ctx, fromBranch)
	if err != nil {
		return fmt.Errorf("createBranch: resolve source %q: %w", fromBranch, err)
	}
	if err := s.gits.SetReference(plumbing.NewHashReference(newRefName, fromHash)); err != nil {
		return fmt.Errorf("createBranch: set ref: %w", err)
	}
	log.Info().Str("branch", branch).Str("from", fromBranch).Msg("created branch")
	return nil
}

// CreateBranch creates a new branch ref pointing at the tip of fromBranch.
// No-op if branch already exists.
func (s *Service) CreateBranch(ctx context.Context, branch, fromBranch string) error {
	return s.createBranch(ctx, branch, fromBranch)
}

// ConfigureRemote sets up (or reconfigures) the "origin" remote with the given
// URL and fetch refspec for branch. Idempotent — returns nil if already correct.
func (s *Service) ConfigureRemote(ctx context.Context, url, branch string) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()

	cfg, err := s.repo.Config()
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
	_ = s.repo.DeleteRemote("origin")
	_, err = s.repo.CreateRemote(&gogitconfig.RemoteConfig{
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
	head, err := s.gits.Reference(plumbing.HEAD)
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
	return s.gits.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branch)),
	)
}

// HasSharedHistory checks whether localBranch shares any commits with remoteBranch on the remote service.
// Uses a bounded walk (max 1000 commits) to avoid scanning huge histories.
func (s *Service) HasSharedHistory(ctx context.Context, localBranch string, remote *Service, remoteBranch string) (bool, error) {
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
