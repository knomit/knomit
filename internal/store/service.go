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

	gits := storegit.NewStorer(db)
	rh := newRepoHandler(db, gits)
	si := &searchIndex{rh: rh}
	rh.onDrop = si.GC
	fi := &factIndex{rh: rh}
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
func (s *Service) Storer() storer.Storer { return s.rh.gits }

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


// ConfigureRemote sets up (or reconfigures) the "origin" remote with the given
// URL and fetch refspec for branch. Idempotent — returns nil if already correct.
func (s *Service) ConfigureRemote(ctx context.Context, url, branch string) error {
	s.rh.configMu.Lock()
	defer s.rh.configMu.Unlock()

	cfg, err := s.rh.repo.Config()
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
	_ = s.rh.repo.DeleteRemote("origin")
	_, err = s.rh.repo.CreateRemote(&gogitconfig.RemoteConfig{
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


// HasSharedHistory checks whether localBranch shares any commits with remoteBranch on the remote service.
// Uses a bounded walk (max 1000 commits) to avoid scanning huge histories.
func (s *Service) HasSharedHistory(ctx context.Context, localBranch string, remote *Service, remoteBranch string) (bool, error) {
	const maxCommits = 1000

	localHash, err := s.rh.resolveRef(ctx, localBranch)
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: local ref: %w", err)
	}
	localHashes := make(map[plumbing.Hash]struct{})
	localIter, err := s.rh.repo.Log(&gogit.LogOptions{From: localHash})
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

	remoteHash, err := remote.rh.resolveRef(ctx, remoteBranch)
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: remote ref: %w", err)
	}
	remoteIter, err := remote.rh.repo.Log(&gogit.LogOptions{From: remoteHash})
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

// deriveAgentID extracts the agent identifier from a branch name.
// "agent/laptop-abc" → "laptop-abc", "main" → "main".
func deriveAgentID(branch string) string {
	if after, ok := strings.CutPrefix(branch, "agent/"); ok {
		return after
	}
	return branch
}
