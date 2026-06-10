package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/store/git"
	"knomit/internal/store/migrate"
)

// sessionSchema is the full DDL for the ephemeral session database, applied
// wholesale to the fresh DB at Open (see initSessionSchema). It is NOT run
// through the versioned migration runner — the DB is disposable and recreated
// empty each start, so there is nothing to version.
//
//go:embed session_schema.sql
var sessionSchema string

// blobObjectType is the go-git integer for plumbing.BlobObject.
const blobObjectType = 3

// Compile-time check (SearchIndex has no assertion in its own file).
var _ SearchIndex = (*searchIndex)(nil)

// Service is the single entry point for all database and git access. It opens one
// SQLite file with sqlite-vec + GraphQLite extensions, runs the embedded
// schema, and provides both a go-git Storer and typed index accessors.
//
// Git-backed fact operations are delegated to fi (*factIndex), which is
// populated by the repo constructors (OpenRepo, InitRepo, etc.) and is nil/zero
// when Service is used in DB-only mode via Open().
type Service struct {
	rh          *repoHandler
	handlerOnce sync.Once
	handler     http.Handler
	fi          *factIndex
	si          *searchIndex
	pi          *pipelineIndex
	ti          *toolIndex
	ri          *remoteIndex
	dbPath      string

	// sessionDB is a separate, ephemeral SQLite file holding all in-flight
	// session/work-queue state (tool paging cursors + pipeline work-stealing).
	// It uses the stock sqlite3 driver (no vec/GraphQLite extensions, so none of
	// the _txlock=immediate/warm-pool machinery the main DB needs) and is
	// recreated empty on every Open and deleted on Close — cursors are not
	// meaningfully durable across restarts. It is always non-nil on a Service
	// returned by Open: if the session DB cannot be set up, Open fails rather
	// than returning a Service without it (there is no main-DB fallback — the
	// session tables no longer exist there). It is set to nil only by Close.
	sessionDB     *sql.DB
	sessionDBPath string
}

// Open opens (or creates) a unified SQLite database at path, initializes the
// schema, and returns a Service that provides access to both the git storer
// and the search index.
func Open(path string) (*Service, error) {
	registerVec() // one-time sqlite-vec + GraphQLite driver registration

	// A bare :memory: database is per-connection — multiple pooled connections
	// would each be a SEPARATE empty database. It is only used as a defensive
	// fallback; pin it to a single connection so schema + data stay coherent and
	// skip the file-DB write-serialization machinery below (a single connection
	// cannot have cross-connection contention).
	memory := path == ":memory:"

	dsn := path
	if memory {
		dsn = path + "?_foreign_keys=1"
	} else {
		// _txlock=immediate makes every transaction BEGIN IMMEDIATE, acquiring
		// the write lock up front so concurrent writers serialize cleanly at the
		// storage layer (the loser blocks on _busy_timeout) instead of failing
		// mid-statement. This is required because GraphQLite issues writes
		// mid-SELECT on the calling connection and never retries on SQLITE_BUSY —
		// under the default deferred BEGIN, two writers on different branches
		// race the shared graph and one fails with "Failed to execute MATCH for
		// DELETE". See warmPool below for why this needs a pre-warmed pool.
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1&_txlock=immediate"
	}
	db, err := sql.Open("sqlite3_knomit", dsn)
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}

	if memory {
		db.SetMaxOpenConns(1)
	} else {
		// Bound the pool AND keep every connection warm. MaxIdleConns must equal
		// MaxOpenConns: otherwise database/sql closes connections above the idle
		// limit and re-opens them lazily on demand — and a lazy open runs the
		// ConnectHook, which loads the GraphQLite extension (a write to its EAV
		// schema). Under _txlock=immediate, if that lazy open happens while
		// another statement on the same goroutine holds the write lock (e.g.
		// rebuildGraph's in-tx read needing a second connection), the new
		// connection's extension init blocks on the held write lock — a
		// self-deadlock. Keeping all connections warm + pre-opening them below
		// guarantees no connection is ever initialized while a write lock is held.
		const poolSize = 4
		db.SetMaxOpenConns(poolSize)
		db.SetMaxIdleConns(poolSize)
		db.SetConnMaxLifetime(0) // never expire — keep connections warm for the process lifetime
		db.SetConnMaxIdleTime(0) // never close idle connections

		// Per-connection performance pragmas are applied in the ConnectHook
		// (vec.go) so every pooled connection is configured, not just the first.

		// Pre-warm the pool: force every connection open now (running the
		// ConnectHook + GraphQLite extension load) while NO write lock is held,
		// so later acquisitions reuse warm connections instead of initializing
		// one mid-transaction. See SetMaxIdleConns rationale above.
		if err := warmPool(db, poolSize); err != nil {
			db.Close()
			return nil, fmt.Errorf("store.Open: warm pool: %w", err)
		}
	}

	// Update query planner statistics (one-time hint, not per-connection).
	db.Exec("PRAGMA optimize")

	if err := migrate.All(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: %w", err)
	}

	gits := storegit.NewStorer(db)
	rh := newRepoHandler(db, gits)
	// Derive repo name from dbPath: /path/to/knomit.db → "knomit"
	rh.name = strings.TrimSuffix(filepath.Base(path), ".db")
	si := &searchIndex{rh: rh}

	// facts_vec is code-managed (migration 000009 drops the old static
	// FLOAT[768] table). Recreate it at the default dimension now so the
	// facts_after_delete trigger and upsert/query paths work even before an
	// embedder is configured (or when embeddings are disabled entirely). Once
	// an embedder is set, Rebuild's ensureFactsVec recreates it at the real
	// model dim and the model-change self-heal re-embeds the corpus.
	if err := si.ensureFactsVecDefault(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: ensure facts_vec: %w", err)
	}

	rh.onDrop = si.GC
	rh.im = si // notifyCommit delegates to im.Sync after every commit.
	fi := &factIndex{rh: rh}
	ri := &remoteIndex{rh: rh}
	canonPath := canonicalizePath(path)

	// Open the ephemeral session DB (separate file, stock driver). This is
	// required, not best-effort: the session/work-queue tables no longer exist
	// in the main DB, so a Service without it could not page queries or run the
	// work-stealing pipeline. A setup failure therefore fails Open rather than
	// silently degrading.
	sessionDB, sessionPath, err := openSessionDB(path)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: session DB: %w", err)
	}

	return &Service{
		rh:            rh,
		fi:            fi,
		si:            si,
		pi:            &pipelineIndex{rh: rh, sessionDB: sessionDB},
		ti:            &toolIndex{db: sessionDB},
		ri:            ri,
		dbPath:        canonPath,
		sessionDB:     sessionDB,
		sessionDBPath: sessionPath,
	}, nil
}

// sessionDBPathFor derives the ephemeral session DB path from the main DB path.
// For a file DB it is a sibling "<name>.sessions.db"; for the :memory: fallback
// (tests, DB-only mode) it is a unique temp file so multiple in-memory Services
// don't collide on one session file.
func sessionDBPathFor(mainPath string) string {
	if mainPath == ":memory:" || mainPath == "" {
		return filepath.Join(os.TempDir(), "knomit-sessions-"+uuid.New().String()+".db")
	}
	return strings.TrimSuffix(mainPath, ".db") + ".sessions.db"
}

// openSessionDB removes any leftover session files (so the DB starts empty) and
// opens a fresh one with the stock sqlite3 driver, then applies the schema.
// Returns the opened DB and the resolved path.
func openSessionDB(mainPath string) (*sql.DB, string, error) {
	sessionPath := sessionDBPathFor(mainPath)
	// Start empty: a session cursor from a previous run is dead anyway.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(sessionPath + suffix)
	}

	// Stock "sqlite3" driver — NO vec/GraphQLite extensions, so no need for
	// _txlock=immediate or the warm/pinned pool the main DB requires.
	dsn := sessionPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1&_synchronous=OFF"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, "", fmt.Errorf("open: %w", err)
	}
	if err := initSessionSchema(db); err != nil {
		db.Close()
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(sessionPath + suffix)
		}
		return nil, "", err
	}
	return db, sessionPath, nil
}

// initSessionSchema execs the embedded session schema wholesale against the
// fresh DB. No versioning — the DB is recreated empty each start.
func initSessionSchema(db *sql.DB) error {
	if _, err := db.Exec(sessionSchema); err != nil {
		return fmt.Errorf("init session schema: %w", err)
	}
	return nil
}

// ReapIdleSessions deletes session rows whose last_used_at is older than the
// per-cluster TTL, cascading to their queue/work-item children. Active sessions
// bump last_used_at on every page/work-item access and are never reaped, so this
// is safe under concurrent paging. A non-positive TTL skips that cluster.
// Returns the total number of session rows deleted across both clusters.
func (s *Service) ReapIdleSessions(ctx context.Context, toolTTL, pipelineTTL time.Duration) (int, error) {
	if s.sessionDB == nil {
		return 0, nil
	}
	var total int
	now := time.Now().UTC()
	reap := func(table string, ttl time.Duration) error {
		if ttl <= 0 {
			return nil
		}
		cutoff := now.Add(-ttl).Format(time.RFC3339)
		res, err := s.sessionDB.ExecContext(ctx,
			"DELETE FROM "+table+" WHERE last_used_at < ?", cutoff)
		if err != nil {
			return fmt.Errorf("reap %s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
		return nil
	}
	if err := reap("tool_sessions", toolTTL); err != nil {
		return total, err
	}
	if err := reap("pipeline_sessions", pipelineTTL); err != nil {
		return total, err
	}
	return total, nil
}

// warmPool forces n distinct physical connections open and returns them to the
// idle pool. Grabbing all n at once (rather than ping-in-a-loop, which would
// reuse one connection) guarantees the driver opens n separate handles — each
// running the ConnectHook + GraphQLite extension load exactly once, now, while
// no write lock is held. After this, the pool holds n warm connections that are
// reused without re-initialization (see SetMaxIdleConns rationale in Open).
func warmPool(db *sql.DB, n int) error {
	conns := make([]*sql.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			c.Close() // returns to the idle pool; does NOT close the underlying connection
		}
	}()
	for range n {
		c, err := db.Conn(context.Background())
		if err != nil {
			return err
		}
		conns = append(conns, c)
	}
	return nil
}

// SetCrypt sets the encryption provider for credential storage.
func (s *Service) SetCrypt(c *Crypt) { s.ri.crypt = c }

// Remote returns the RemoteIndex for git remote configuration and sync.
func (s *Service) Remote() RemoteIndex { return s.ri }

// Facts returns the FactIndex for git-backed fact operations.
func (s *Service) Facts() FactIndex { return s.fi }

// Search returns the SearchIndex for full-text and vector search.
func (s *Service) Search() SearchIndex { return s.si }

// IndexManager returns the IndexManager for search index lifecycle operations.
func (s *Service) IndexManager() IndexManager { return s.si }

// SetEmbedder configures the embedder used by the search index for vector operations.
func (s *Service) SetEmbedder(e BatchEmbedder) { s.rh.setEmbedder(e) }

// Pipeline returns the PipelineIndex for pipeline session management.
func (s *Service) Pipeline() PipelineIndex { return s.pi }

// ToolSession returns the ToolSessionIndex for tool session persistence.
func (s *Service) ToolSession() ToolSessionIndex { return s.ti }

// Branches returns the branch index.
func (s *Service) Branches() BranchIndex { return s.rh }

// ClusterCache returns the cluster-cache storage layer (SQL CRUD only;
// decision logic lives in internal/clustercache).
func (s *Service) ClusterCache() ClusterCacheStore { return &clusterCacheStore{rh: s.rh} }


// Checkpoint flushes the WAL to the main database file so the .db file is
// self-contained (e.g. before file-level copy). This is a no-op if WAL mode
// is not enabled.
func (s *Service) Checkpoint() error {
	_, err := s.rh.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// Close closes the underlying database connections and removes the ephemeral
// session DB file (it holds only disposable runtime state).
func (s *Service) Close() error {
	if s.sessionDB != nil {
		s.sessionDB.Close()
		s.sessionDB = nil
	}
	if s.sessionDBPath != "" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(s.sessionDBPath + suffix)
		}
	}
	return s.rh.db.Close()
}

// SetSigner sets the SSH signer used for commit signing.
func (s *Service) SetSigner(signer ssh.Signer) {
	s.rh.signer = signer
}

// SetOnCommit registers a callback invoked after every commit (after internal
// bookkeeping). This allows callers to observe commits for side-effects such
// as SSE broadcasting. The callback receives the branch name and commit hash.
func (s *Service) SetOnCommit(fn func(branch, hash string)) {
	s.rh.onCommit = fn
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

// canonicalizePath resolves symlinks so the path matches what SQLite returns
// via PRAGMA database_list (e.g. on macOS /var is a symlink to /private/var).
// Falls back to the original path if resolution fails or path is a special
// value like ":memory:".
func canonicalizePath(path string) string {
	if path == "" || path == ":memory:" {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}
