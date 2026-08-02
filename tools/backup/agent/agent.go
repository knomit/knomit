// Package agent is the litestream half of knomit's backup: the code that
// runs INSIDE the knomit-backup child process. It is the only package in this
// repository that imports litestream, and nothing knomit links imports it.
//
// # Why it is a separate process
//
// litestream v0.5 opens its databases with modernc.org/sqlite; knomit opens
// the same files with the cgo mattn/go-sqlite3 build and cannot switch, because
// sqlite-vec has no modernc build. Two SQLite BUILDS in ONE process do not see
// each other's locks: POSIX advisory record locks do not conflict between
// descriptors held by the same process, and SQLite's compensating per-process
// inode table is private to a single build. That was demonstrated, not
// theorised — knomit's close deleted litestream's -wal while litestream held a
// read lock with PERSIST_WAL, and removed the -shm while litestream had it
// mapped, 3 runs out of 3.
//
// Across PROCESSES the same POSIX locks work exactly as SQLite intends, so
// moving litestream out is not a mitigation of that hazard, it is its removal.
// Nothing here may ever be linked back into knomit.
//
// # Blank imports
//
// They register litestream's replica-client URL schemes (see
// litestream.RegisterReplicaClientFactory): a backend package must be imported
// for litestream.NewReplicaClientFromURL to resolve its scheme. "file" backs
// the local-disk test path; "s3" and "gs" are the documented production targets
// (including S3-compatible endpoints such as Fly's Tigris). litestream also
// ships abs, sftp, webdav, nats and oss backends; none are part of the
// documented backup.url surface, so they are deliberately NOT imported — an
// undocumented scheme then fails at open's probe with "unsupported replica URL
// scheme", which is the correct answer for an unsupported target.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sync"
	"time"

	"github.com/benbjohnson/litestream"
	_ "github.com/benbjohnson/litestream/file"
	_ "github.com/benbjohnson/litestream/gs"
	_ "github.com/benbjohnson/litestream/s3"

	"knomit/internal/backup/proto"
)

// archiveSnapshotRetention is the retention window the archive store
// advertises. It is "forever" expressed as a number, and it is belt to
// RetentionEnabled's braces: with deletion switched off the window is not
// consulted for remote files at all, but litestream still prunes the LOCAL LTX
// cache of anything the window calls expired (db.go's sweep of expired local
// files sits OUTSIDE the RetentionEnabled guard), and there is no reason to
// churn that for a database which by definition never changes again.
const archiveSnapshotRetention = 100 * 365 * 24 * time.Hour

// Agent owns litestream's stores and the set of tracked databases.
//
// # Locking
//
// opMu serialises the MUTATIONS of the tracked set (track, untrack,
// reset_local_state) against each other, so a name cannot be registered and
// unregistered concurrently. It is deliberately not mu: unregistering blocks
// on litestream — a database close performs a final replica sync WITH RETRY,
// bounded by ShutdownSyncTimeout (30s by default) — and holding mu across that
// would stall every status request for the duration of an object-store hiccup.
//
// mu guards the fields below and is held only for map and flag access, never
// across a litestream call. Order is opMu then mu, never the reverse.
type Agent struct {
	opMu sync.Mutex

	mu  sync.RWMutex
	cfg proto.Config
	dbs map[string]tracked
	// store replicates LIVE databases under the configured retention.
	store *litestream.Store
	// archiveStore replicates ARCHIVED databases with retention disabled.
	//
	// It is a second store rather than a per-database setting because v0.5.15
	// has no per-database one that survives registration: Store.RegisterDB
	// overwrites db.RetentionEnabled from the store's own field immediately
	// before db.Open() copies it into the compactor, Store.SetRetentionEnabled
	// is store-wide, Store.EnforceSnapshotRetention reads the STORE's
	// SnapshotRetention for every database it sweeps, and litestream.Replica
	// has no retention field at all. The store a database is registered with is
	// therefore the only place the setting can be made to stick.
	archiveStore *litestream.Store
	opened       bool
	closed       bool

	// logger is where the agent's own diagnostics go. It must never be stdout:
	// stdout carries protocol traffic only.
	logger *slog.Logger
}

// tracked is one registered database plus the fact that decides which store
// owns it. The flag is remembered rather than re-derived, so untrack cannot
// disagree with track about where a database was registered — routing it to the
// wrong store would make UnregisterDB a silent no-op and leave the database
// replicating after the caller was told it had stopped.
type tracked struct {
	db       *litestream.DB
	archived bool
}

// New returns an Agent that logs to logger. The stores do not exist until
// Open is called.
func New(logger *slog.Logger) *Agent {
	if logger == nil {
		logger = slog.Default()
	}
	return &Agent{dbs: map[string]tracked{}, logger: logger}
}

// coded wraps an error with the protocol code the client should see. Handlers
// return it when the caller must be able to branch on the failure's identity
// rather than read its message.
type coded struct {
	code string
	err  error
}

func (c *coded) Error() string { return c.err.Error() }
func (c *coded) Unwrap() error { return c.err }

func withCode(code string, err error) error { return &coded{code: code, err: err} }

// codeOf returns the protocol code for an error, defaulting to CodeInternal.
func codeOf(err error) string {
	var c *coded
	if errors.As(err, &c) {
		return c.code
	}
	return proto.CodeInternal
}

// Open builds both stores and PROBES the replica target.
//
// The probe is a REAL round-trip rather than a URL parse: the whole point is
// that bad credentials or an unreachable bucket fail knomit's boot instead of
// surfacing later as a silent replication stall.
func (a *Agent) Open(ctx context.Context, cfg proto.Config) error {
	a.opMu.Lock()
	defer a.opMu.Unlock()

	a.mu.RLock()
	opened, closed := a.opened, a.closed
	a.mu.RUnlock()
	if closed {
		return fmt.Errorf("open: agent is closed")
	}
	if opened {
		return fmt.Errorf("open: agent is already open")
	}

	if cfg.URL == "" {
		return fmt.Errorf("open: url is required")
	}

	if err := probe(ctx, prefix(cfg, ".")); err != nil {
		return fmt.Errorf("replica target unreachable (%s): %w", cfg.URL, err)
	}

	store := newStore(cfg)
	if err := store.Open(ctx); err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	// The archive store's retention settings are the whole reason it exists;
	// see the field comment on Agent.archiveStore.
	archiveStore := newStore(cfg)
	archiveStore.RetentionEnabled = false
	archiveStore.SnapshotRetention = archiveSnapshotRetention
	// Zero disables Store.monitorL0Retention outright (it only starts when both
	// L0Retention and its check interval are positive) and makes
	// DB.EnforceL0Retention return early, so no sweep can even reach the
	// RetentionEnabled check for an archived database.
	archiveStore.L0Retention = 0
	if err := archiveStore.Open(ctx); err != nil {
		// The live store is already running and nothing else will reclaim it:
		// open failed, so the client has no agent state to close.
		_ = store.Close(ctx)
		return fmt.Errorf("open archive store: %w", err)
	}

	a.mu.Lock()
	a.cfg, a.store, a.archiveStore, a.opened = cfg, store, archiveStore, true
	a.mu.Unlock()

	a.logger.Info("backup agent open", "url", cfg.URL, "instance", cfg.Instance)
	return nil
}

// newStore builds a litestream store with knomit's compaction levels and the
// configured intervals. Callers adjust retention afterwards.
func newStore(cfg proto.Config) *litestream.Store {
	s := litestream.NewStore(nil, litestream.CompactionLevels{
		{Level: 0},
		{Level: 1, Interval: 10 * time.Second},
	})
	s.SnapshotInterval = cfg.SnapshotInterval
	s.SnapshotRetention = cfg.SnapshotRetention
	s.L0Retention = cfg.L0Retention
	return s
}

// prefix builds the replica URL for one relative path under the instance.
func prefix(cfg proto.Config, rel string) string {
	return cfg.URL + "/" + path.Join(cfg.Instance, rel)
}

// prefixFor is prefix against the agent's live config.
func (a *Agent) prefixFor(rel string) string {
	a.mu.RLock()
	cfg := a.cfg
	a.mu.RUnlock()
	return prefix(cfg, rel)
}

// probe verifies the replica target is reachable and credentials work.
//
// litestream.ReplicaClient.LTXFiles returns a lazily-paginated iterator (the
// underlying AWS SDK paginator does not issue a request until the first
// Next()), so parsing the URL and constructing the client is not enough — the
// iterator must be driven at least once to force that first network call. One
// Next() is sufficient: it either returns an item (proving the round-trip
// succeeded) or sets the iterator's error (surfaced via Close()), and an empty
// result is success — a first boot has nothing there yet.
func probe(ctx context.Context, url string) error {
	client, err := litestream.NewReplicaClientFromURL(url)
	if err != nil {
		return fmt.Errorf("parse replica url: %w", err)
	}
	if err := client.Init(ctx); err != nil {
		return fmt.Errorf("init replica client: %w", err)
	}
	itr, err := client.LTXFiles(ctx, 0, 0, false)
	if err != nil {
		return fmt.Errorf("list ltx files: %w", err)
	}
	itr.Next() // force the round-trip; discard the item, if any.
	if err := itr.Close(); err != nil {
		return fmt.Errorf("list ltx files: %w", err)
	}
	return nil
}

// requireOpen reports the not-open condition as a coded error, so the client
// can tell "the agent restarted and has not been reconfigured yet" from a
// genuine failure and retry rather than propagate.
func (a *Agent) requireOpen() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.closed {
		return withCode(proto.CodeNotOpen, fmt.Errorf("agent is closed"))
	}
	if !a.opened {
		return withCode(proto.CodeNotOpen, fmt.Errorf("agent has not been opened"))
	}
	return nil
}

// storeFor returns the store a database belongs to. Archived databases live in
// the retention-disabled store; the caller says which, because it is a property
// of the database's role and not of its path.
func (a *Agent) storeFor(archived bool) *litestream.Store {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if archived {
		return a.archiveStore
	}
	return a.store
}

// Track begins replicating the database at p.Path under p.Name.
//
// Re-tracking a name against the SAME file is a no-op — which is what makes the
// method safe to replay after an agent restart. Against a DIFFERENT file it is
// CodeTrackedElsewhere, never a silent success: litestream.DB.init pins a file
// descriptor with a single os.Open guarded by `if db.db != nil { return nil }`,
// so a tracked database keeps replicating the INODE it opened. If a repo is
// archived (its .db renamed away) and a new repo then claims the freed name, a
// swallowed track would leave the new database replicated by nobody — no
// snapshot, no error, and status still reporting the name as in sync.
func (a *Agent) Track(ctx context.Context, p proto.TrackParams) error {
	if err := a.requireOpen(); err != nil {
		return err
	}
	if p.Name == "" || p.Path == "" || p.Rel == "" {
		return withCode(proto.CodeBadRequest, fmt.Errorf("track: name, path and rel are required"))
	}

	// opMu, not mu: the store call below must not block status.
	a.opMu.Lock()
	defer a.opMu.Unlock()

	a.mu.RLock()
	existing, isTracked := a.dbs[p.Name]
	closed := a.closed
	a.mu.RUnlock()
	if closed {
		return withCode(proto.CodeNotOpen, fmt.Errorf("agent is closed"))
	}
	if isTracked {
		if existing.db.Path() != p.Path {
			return withCode(proto.CodeTrackedElsewhere, fmt.Errorf(
				"%q is already tracked against a different file (replicating %s, asked for %s); "+
					"the caller's database would be backed up by nothing",
				p.Name, existing.db.Path(), p.Path))
		}
		return nil
	}

	store := a.storeFor(p.Archived)
	db := litestream.NewDB(p.Path)
	a.mu.RLock()
	db.MonitorInterval = a.cfg.MonitorInterval
	a.mu.RUnlock()

	client, err := litestream.NewReplicaClientFromURL(a.prefixFor(p.Rel))
	if err != nil {
		return fmt.Errorf("track %q: replica client: %w", p.Name, err)
	}
	db.Replica = litestream.NewReplicaWithClient(db, client)

	// litestream.Store has no Add/Remove pair in v0.5.15; RegisterDB/UnregisterDB
	// fill that role (RegisterDB also calls db.Open(), so we must not call it
	// ourselves first). The store's registered-DB set is dynamic — RegisterDB
	// appends to it and starts monitoring immediately.
	if err := store.RegisterDB(db); err != nil {
		return fmt.Errorf("track %q: register with store: %w", p.Name, err)
	}

	// Re-check closed under the WRITE lock, and hand the database back if Close
	// won the race. The check at the top is not enough: mu is not held across
	// the registration above, so Close can run in between — and nothing
	// downstream would ever clean this database up. litestream's
	// Store.RegisterDB has no closed flag (it opens and appends to an
	// already-closed store), and DB's context comes from context.Background(),
	// so store.Close's cancel never reaches the monitor goroutine this just
	// started. Without the recheck the monitor and its SQLite read lock outlive
	// shutdown, replicating on behalf of a process that believes it stopped.
	a.mu.Lock()
	closed = a.closed
	if !closed {
		a.dbs[p.Name] = tracked{db: db, archived: p.Archived}
	}
	a.mu.Unlock()
	if closed {
		// Outside mu, as ever: this closes the database and its final sync can
		// block on the replica.
		if err := store.UnregisterDB(context.Background(), db.Path()); err != nil {
			return fmt.Errorf("track %q: agent closed mid-registration, and unregistering it failed: %w", p.Name, err)
		}
		return withCode(proto.CodeNotOpen, fmt.Errorf("agent is closed"))
	}

	a.logger.Info("tracking database", "db", p.Name, "path", p.Path, "archived", p.Archived)
	return nil
}

// Untrack permanently stops replicating a database (archive, purge).
//
// The database is dropped from the tracked set BEFORE the store call, because
// UnregisterDB removes it from the store and closes it in that order: once it
// returns — error or not — the database is closed and no longer replicating, so
// leaving it in the map on failure would only make status report a corpse.
// Callers that need replication back must re-track, not retry untrack.
func (a *Agent) Untrack(name string) error {
	if err := a.requireOpen(); err != nil {
		return err
	}

	// opMu, not mu: UnregisterDB closes the database, whose final replica sync
	// RETRIES for up to ShutdownSyncTimeout.
	a.opMu.Lock()
	defer a.opMu.Unlock()

	a.mu.Lock()
	t, ok := a.dbs[name]
	delete(a.dbs, name)
	a.mu.Unlock()
	if !ok {
		return nil
	}

	if err := a.storeFor(t.archived).UnregisterDB(context.Background(), t.db.Path()); err != nil {
		return fmt.Errorf("untrack %q: %w", name, err)
	}
	a.logger.Info("stopped tracking database", "db", name)
	return nil
}

// Status reports replication state for every tracked database.
//
// db.SyncStatus performs a REMOTE round-trip per call — it drains the entire
// level-0 LTX file listing from the replica to find the remote position,
// unbounded by bucket size and with no caching. Status therefore snapshots the
// tracked set under mu and releases it BEFORE making any of those calls:
// holding the lock across a blocking network call would stall a concurrent
// track or untrack for as long as any round-trip is in flight.
func (a *Agent) Status(ctx context.Context) ([]proto.DBStatus, error) {
	if err := a.requireOpen(); err != nil {
		return nil, err
	}
	a.mu.RLock()
	snapshot := make(map[string]tracked, len(a.dbs))
	for name, t := range a.dbs {
		snapshot[name] = t
	}
	a.mu.RUnlock()

	out := make([]proto.DBStatus, 0, len(snapshot))
	for name, t := range snapshot {
		st := proto.DBStatus{Name: name}
		// litestream's own record of the last completed replica sync. Read from
		// the DB rather than derived here, and left at zero when it has never
		// synced — "never" is a state the consumer renders by omission, and any
		// stand-in value would claim a sync that did not happen.
		if at := t.db.LastSuccessfulSyncAt(); !at.IsZero() {
			st.LastSyncUnix = at.Unix()
		}
		sync, err := t.db.SyncStatus(ctx)
		if err != nil {
			st.LastError = err.Error()
		} else {
			st.LocalTXID = uint64(sync.LocalTXID)
			st.RemoteTXID = uint64(sync.RemoteTXID)
			st.InSync = sync.InSync
		}
		out = append(out, st)
	}
	return out, nil
}

// isTrackedPath reports whether any registered database is replicating the file
// at path. It exists so the overwriting restore can refuse to write underneath
// a live replica.
func (a *Agent) isTrackedPath(path string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, t := range a.dbs {
		if t.db.Path() == path {
			return true
		}
	}
	return false
}

// ResetLocalState discards litestream's local LTX state for a database file,
// forcing a re-anchor against the replica the next time it is tracked.
//
// It takes a PATH rather than a name because it is called between an untrack
// and a re-track, when the name is by definition not tracked. litestream's
// local state lives entirely in paths derived from the database file, so a
// throwaway DB value is enough to address it.
func (a *Agent) ResetLocalState(ctx context.Context, dbPath string) error {
	if err := a.requireOpen(); err != nil {
		return err
	}
	if dbPath == "" {
		return withCode(proto.CodeBadRequest, fmt.Errorf("reset_local_state: path is required"))
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if err := litestream.NewDB(dbPath).ResetLocalState(ctx); err != nil {
		return fmt.Errorf("reset local state %s: %w", dbPath, err)
	}
	return nil
}

// DeleteReplica permanently removes every object under one replica prefix.
//
// Scope is exactly that prefix: the client is built from it, and DeleteAll is
// prefix-scoped in all three registered backends — file does RemoveAll on its
// own directory, s3 paginates ListObjectsV2 under Path+"/" and batch-deletes
// only those keys, and gs iterates Objects with Prefix Path+"/" and deletes
// only those.
func (a *Agent) DeleteReplica(ctx context.Context, rel string) error {
	if err := a.requireOpen(); err != nil {
		return err
	}
	if rel == "" {
		return withCode(proto.CodeBadRequest, fmt.Errorf("delete_replica: rel is required"))
	}
	url := a.prefixFor(rel)
	client, err := litestream.NewReplicaClientFromURL(url)
	if err != nil {
		return fmt.Errorf("delete replica %s: replica client: %w", rel, err)
	}
	if err := client.Init(ctx); err != nil {
		return fmt.Errorf("delete replica %s: init replica client: %w", rel, err)
	}
	if err := client.DeleteAll(ctx); err != nil {
		return fmt.Errorf("delete replica %s: %w", rel, err)
	}
	a.logger.Info("deleted a replica prefix", "rel", rel, "url", url)
	return nil
}

// Close stops replication for every tracked database. It is idempotent: both
// an explicit close request and stdin reaching EOF reach it.
func (a *Agent) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed || !a.opened {
		a.closed = true
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	store, archiveStore := a.store, a.archiveStore
	a.mu.Unlock()

	// Outside mu, and CONCURRENTLY. Each store's close performs a final replica
	// sync per database with retry, bounded by ShutdownSyncTimeout (30s by
	// default). Closing the two in sequence would double the worst case to a
	// minute for no benefit, since the stores share nothing.
	//
	// Both, always: the archive store owns real databases too, and one left
	// open outlives the process's belief that it has stopped replicating.
	var wg sync.WaitGroup
	var archiveErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		archiveErr = archiveStore.Close(ctx)
	}()
	err := store.Close(ctx)
	wg.Wait()
	if err != nil {
		return err
	}
	return archiveErr
}
