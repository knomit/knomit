package store

import (
	"context"
	"database/sql"
	"fmt"
	"knomit/internal/engine/store/internal/sqlite"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/crypto/ssh"

	"knomit/internal/engine/store/facts"
	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/internal/vcs"
	"knomit/internal/engine/store/merge"
	"knomit/internal/engine/store/migrate"
	"knomit/internal/engine/store/model"
	"knomit/internal/engine/store/remotesync"
	"knomit/internal/engine/store/sessions"
	"knomit/internal/engine/store/vocab"
)

// Service is the single entry point for all database and git access. It opens one
// SQLite file with the sqlite-vec extension, runs the embedded
// schema, and provides both a go-git Storer and typed index accessors.
//
// Git-backed fact operations are delegated to fi (*facts.Index), which is
// populated by the repo constructors (OpenRepo, InitRepo, etc.) and is nil/zero
// when Service is used in DB-only mode via Open().
type Service struct {
	rh *repoHandler
	// fr, mr and rr are the facts, merge and remotesync children over the same
	// *vcs.Repo as rh, held as NAMED fields: embedding them in repoHandler
	// promoted every child method onto rh and hid which child a call reached.
	fr          *facts.Repo
	mr          *merge.Repo
	rr          *remotesync.Repo
	handlerOnce sync.Once
	handler     http.Handler
	fi          *facts.Index
	si          *index.Index
	// The four query sub-services carved out of the search index (P1.3). The index.Index
	// (si) keeps IndexManager + the write/rebuild path; these own the read
	// surface. search composes all four for the Search() composite.
	fq *index.FactQuery
	gq *index.GraphStore
	hq *index.HistoryQuery
	mq *index.MethodologyMatcher
	// ax is the abstraction axis + restatement shortlist. Kept OFF the search
	// facade on purpose: it is review-pipeline state, and composing it would
	// hand it to every mcp/web consumer of Search().
	ax *vocab.AbstractionIndex
	// mo is alias resolution: motif spelling -> canonical id. Derived state
	// (MN3), rebuildable from fact_motifs alone.
	mo     *vocab.MotifIndex
	search *index.Facade
	pi     *sessions.PipelineIndex
	ti     *sessions.ToolIndex
	ri     *remotesync.Index
	dbPath string

	// sessionDB is a separate, ephemeral SQLite file holding all in-flight
	// session/work-queue state (tool paging cursors + pipeline work-stealing).
	// It uses the stock sqlite3 driver (no vec extension, so none of
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
	sqlite.Register() // one-time sqlite-vec + knomit driver registration

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
		// mid-statement partway through a read-then-write transaction. SQLite is
		// single-writer regardless; this only decides where a loser blocks.
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1&_txlock=immediate"
	}
	db, err := sql.Open("sqlite3_knomit", dsn)
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}

	if memory {
		db.SetMaxOpenConns(1)
	} else {
		// Bound the pool and keep connections long-lived. The ConnectHook now
		// only registers SQL functions and sets pragmas — neither writes to the
		// database — so a lazy open can no longer block on a held write lock.
		// (It used to load the GraphQLite extension, which wrote to its EAV
		// schema on init; that made a lazy open under _txlock=immediate a
		// potential self-deadlock and forced an eagerly pre-warmed pool.)
		const poolSize = 4
		db.SetMaxOpenConns(poolSize)
		db.SetMaxIdleConns(poolSize)
		db.SetConnMaxLifetime(0) // never expire — keep connections warm for the process lifetime
		db.SetConnMaxIdleTime(0) // never close idle connections

		// Per-connection performance pragmas are applied in the ConnectHook
		// (internal/sqlite/vec.go) so every pooled connection is configured, not just the first.
	}

	// Update query planner statistics (one-time hint, not per-connection).
	db.Exec("PRAGMA optimize")

	if err := migrate.All(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: %w", err)
	}

	gits := storegit.NewStorer(db)
	rh := newRepoHandler(db, gits)
	// Derive repo name from dbPath: /path/to/core.db → "core"
	rh.SetName(strings.TrimSuffix(filepath.Base(path), ".db"))
	si := index.New(rh.Repo)

	// facts_vec is code-managed (migration 000009 drops the old static
	// FLOAT[768] table). Recreate it at the default dimension now so the
	// facts_after_delete trigger and upsert/query paths work even before an
	// embedder is configured (or when embeddings are disabled entirely). Once
	// an embedder is set, Rebuild's ensureFactsVec recreates it at the real
	// model dim and the model-change self-heal re-embeds the corpus.
	if err := si.EnsureFactsVecDefault(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: ensure facts_vec: %w", err)
	}

	rh.SetOnDrop(si.GC)
	rh.SetIndexSync(si.Sync) // NotifyCommit syncs the index after every commit.
	fr := facts.NewRepo(rh.Repo)
	mr := merge.NewRepo(rh.Repo)
	rr := remotesync.NewRepo(mr, rh.Repo)
	fi := facts.NewIndex(fr)
	ri := remotesync.NewIndex(rr)
	fq := index.NewFactQuery(rh.Repo)
	gq := index.NewGraphStore(rh.Repo)
	hq := index.NewHistoryQuery(rh.Repo)
	mq := index.NewMethodologyMatcher(rh.Repo)
	ax := vocab.NewAbstractionIndex(rh.Repo)
	mo := vocab.NewMotifIndex(rh.Repo)
	search := index.NewFacade(fq, gq, hq, mq)
	canonPath := canonicalizePath(path)

	// Open the ephemeral session DB (separate file, stock driver). This is
	// required, not best-effort: the session/work-queue tables no longer exist
	// in the main DB, so a Service without it could not page queries or run the
	// work-stealing pipeline. A setup failure therefore fails Open rather than
	// silently degrading.
	sessionDB, sessionPath, err := sessions.Open(path)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: session DB: %w", err)
	}

	s := &Service{
		rh:            rh,
		fr:            fr,
		mr:            mr,
		rr:            rr,
		fi:            fi,
		si:            si,
		fq:            fq,
		gq:            gq,
		hq:            hq,
		mq:            mq,
		ax:            ax,
		mo:            mo,
		search:        search,
		pi:            sessions.NewPipelineIndex(rh.DB(), sessionDB),
		ti:            sessions.NewToolIndex(sessionDB),
		ri:            ri,
		dbPath:        canonPath,
		sessionDB:     sessionDB,
		sessionDBPath: sessionPath,
	}
	rh.SetConsensusBranchFunc(ri.UpstreamBranch)
	return s, nil
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
	return sessions.ReapIdle(ctx, s.sessionDB, toolTTL, pipelineTTL)
}

// ConfigureRemote wires the git remote named "origin" so go-git can fetch and
// push by name, with refspecs tracking both upstreamMain and agentBranch. The
// git config is a DERIVED CACHE of control.db, rewritten at open; it is never
// the source of truth. Idempotent.
//
// Errors, rather than panicking, when the repo is not initialised (rh.repo is
// nil until InitRepo/OpenRepo) — DB-only mode is plausible at the point Task 4
// wires this in at open time.
//
// An empty upstreamMain is refused (ErrNoConsensusBranch): this is the one
// public way in, so the fetch and reconcile paths below it never see an empty
// branch name and carry no default of their own.
func (s *Service) ConfigureRemote(url, upstreamMain, agentBranch string) error {
	if s.rh.Repository() == nil {
		return fmt.Errorf("ConfigureRemote: repository not initialised")
	}
	if upstreamMain == "" {
		return fmt.Errorf("ConfigureRemote: %w", model.ErrNoConsensusBranch)
	}
	return s.rh.ConfigureRemote(url, upstreamMain, agentBranch)
}

// SetNetworkTimeout bounds every remote git network operation (clone, fetch,
// push, ls-remote) performed by this Service. A non-zero value makes the store
// derive a deadline-bounded context at each go-git call site, so a stalled
// remote aborts instead of hanging forever. 0 disables the bound (legacy
// behavior). Wired from cfg.Git.NetworkTimeout by the repos builder at open
// time and re-applied on SwapStore reopen (store.Open does not restore it).
func (s *Service) SetNetworkTimeout(d time.Duration) { s.rh.SetNetTimeout(d) }

// SetOntologyRoot sets the directory facts live under — the only part of the
// tree the index admits (see IsFactPath). Wired from cfg.OntologyRoot by the
// repos builder at open time and re-applied on SwapStore reopen, exactly like
// SetNetworkTimeout; store.Open does not restore it, and an unset root falls
// back to the package default. Surrounding slashes are trimmed so "kb", "kb/"
// and "/kb" all name the same root.
//
// Changing the root does NOT re-scope an existing index: rows admitted under
// the previous root survive until the next Rebuild, which repopulates from the
// tree under the new one.
func (s *Service) SetOntologyRoot(root string) { s.rh.SetFactRoot(strings.Trim(root, "/")) }

// SetReadOnly marks the store as accepting no AUTHORED commits on any branch.
// Wired by the repos builder for subscriptions. The gate sits at the shared
// write seam (writeFileExact / deleteFile / batchWrite) so every door —
// facts, root files, batches — inherits it; the sync merges are deliberately
// outside it, since a subscription must keep following its upstream. One door
// goes below that seam: WriteSystemTree calls batchWriteLocked directly, so it
// carries its own copy of the gate (TestReadOnlyStore_RefusesAuthoredWrites…
// pins both).
//
// Like SetOntologyRoot, the flag does not survive store.Open and must be
// re-applied on SwapStore reopen: rewireStore rebuilds the Service from disk,
// and a swap that omits this leaves a subscription writable with no error and
// no log. It is set at build/swap time before the store is published and never
// mutated afterwards, which is why the bool needs no lock.
func (s *Service) SetReadOnly(ro bool) { s.rh.SetReadOnly(ro) }

// Remote returns the remotesync index for git remote configuration and sync,
// the origin and the consensus-branch record.
func (s *Service) Remote() *remotesync.Index { return s.ri }

// Facts is the fact index: git-backed fact reads and writes, and the one
// system-tree write door (WriteSystemTree, callable only from internal/repos;
// see test/archtest/store_write_internals_test.go).
func (s *Service) Facts() *facts.Index { return s.fi }

// FactsRepo is the facts child's reader of one commit's own tree: the system
// files, templates, skills and trigger-dispatcher rows, plus the signer
// fingerprint. It holds the vcs handle privately, so none of the git layer's
// methods is reachable through it.
func (s *Service) FactsRepo() *facts.Repo { return s.fr }

// Search returns the search composite (a facade over the four query
// sub-services). Prefer the narrow accessors below (FactQuery/GraphStore/
// HistoryQuery/Methodology) — depend on the smallest query surface a caller
// needs.
func (s *Service) Search() *index.Facade { return s.search }

// FactQuery returns the fact read/search/existence query sub-service.
func (s *Service) FactQuery() *index.FactQuery { return s.fq }

// GraphStore returns the DERIVED_FROM / SIMILAR_TO graph query sub-service.
func (s *Service) GraphStore() *index.GraphStore { return s.gq }

// HistoryQuery returns the commit-log / revision history query sub-service.
func (s *Service) HistoryQuery() *index.HistoryQuery { return s.hq }

// Methodology returns the methodology-matching query sub-service.
func (s *Service) Methodology() *index.MethodologyMatcher { return s.mq }

// Abstraction returns the title-embedding axis and the restatement shortlist
// built on it. REVIEW PIPELINE ONLY — no runtime path may consume it.
func (s *Service) Abstraction() *vocab.AbstractionIndex { return s.ax }

// Motifs is alias resolution over the corpus's motif vocabulary.
func (s *Service) Motifs() *vocab.MotifIndex { return s.mo }

// IndexManager returns the search index for its lifecycle operations (Sync,
// SyncLocked, Rebuild, SyncWatermark, NeedsRebuild).
func (s *Service) IndexManager() *index.Index { return s.si }

// SetEmbedder configures the embedder used by the search index for vector operations.
func (s *Service) SetEmbedder(e model.BatchEmbedder) { s.rh.SetEmbedder(e) }

// Pipeline returns the pipeline session index (sessions, work items and the
// pipeline watermarks).
func (s *Service) Pipeline() *sessions.PipelineIndex { return s.pi }

// ToolSession returns the tool session index (paging cursors and queues).
func (s *Service) ToolSession() *sessions.ToolIndex { return s.ti }

// Merge is the merge child's handle: branch merges (MergeBranch), the pushed
// branches report (PushedBranches) and the experiment lifecycle (open, list,
// commit, sync, rollback, expire). It is the same *merge.Repo the store's own
// MergePushed, MergeConsensus and the reconcile paths use. Callers import
// knomit/internal/engine/store/merge for the names it speaks.
//
// The handle also exports the lock-assuming merge cores that remotesync calls
// (MergeIntoBranchLocked, MergeIntoBranchLockedOpts, MergeTreesWithStrategy,
// MarkExperimentsRewound, FactMergeResolutions): they run without the branch
// lock and without NotifyCommit ordering of their own, so no code outside
// internal/engine/store may call them (test/archtest TestStoreMergeCores_OnlyInsideStore).
// A merge that must happen from outside is MergeBranch, or MergePushed and
// MergeConsensus on the Service, which take the lock.
func (s *Service) Merge() *merge.Repo { return s.mr }

// RootCommit returns the hash of the repo's root commit — the `init: create
// knowledge base` commit every knomit repo is born with — reached by a
// first-parent walk from the given branch's head. The root is identical in
// every clone of a repo, which makes it the repo's stable identity
// (lenses RFC decision 11); all branches share it.
func (s *Service) RootCommit(ctx context.Context, branch string) (string, error) {
	return s.rh.RootCommit(ctx, branch)
}

// Checkpoint flushes the WAL to the main database file so the .db file is
// self-contained (e.g. before file-level copy). This is a no-op if WAL mode
// is not enabled.
func (s *Service) Checkpoint() error {
	_, err := s.rh.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)")
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
	return s.rh.DB().Close()
}

// SetSigner sets the SSH signer used for commit signing.
func (s *Service) SetSigner(signer ssh.Signer) {
	s.rh.SetSigner(signer)
}

// SetOnCommit registers a callback invoked after every commit (after internal
// bookkeeping). This allows callers to observe commits for side-effects such
// as SSE broadcasting. The callback receives the branch name and commit hash.
func (s *Service) SetOnCommit(fn func(branch, hash string)) {
	s.rh.SetOnCommit(fn)
}

// HasSharedHistory checks whether localBranch shares any commits with remoteBranch on the remote service.
// Uses a bounded walk (max 1000 commits) to avoid scanning huge histories.
func (s *Service) HasSharedHistory(ctx context.Context, localBranch string, remote *Service, remoteBranch string) (bool, error) {
	return vcs.HasSharedHistory(ctx, s.rh.Repo, localBranch, remote.rh.Repo, remoteBranch)
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

// HasObject reports whether this store already holds the object named by hash.
//
// A HIT is proof, a MISS is not. Commits are unique to a history, so a store
// that holds a remote's advertised tip is holding the same knowledge base —
// which is what makes this a cheap sufficient check for a duplicate
// subscription (internal/repos, layer 2). A store that does NOT hold it has
// established nothing: the local copy may simply be behind the remote, which
// is the ordinary case. Callers must never read a miss as "not a duplicate".
//
// An unparseable hash is a miss rather than an error: the value comes from a
// remote's advertisement, and the honest answer to "do we hold <garbage>" is
// no. No guard is needed for that — plumbing.NewHash zero-fills whatever it
// cannot decode, and no store holds the zero hash or a zero-padded prefix — so
// there is deliberately no validity check here to go stale or untested.
// TestHasObject pins the behaviour regardless of how it is reached.
//
// Local read only; never touches the network.
func (s *Service) HasObject(hash string) bool {
	return s.rh.Storer().HasEncodedObject(plumbing.NewHash(hash)) == nil
}
