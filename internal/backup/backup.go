// Package backup owns knomit's continuous replication to object storage. It is
// the ONLY package that imports litestream — everything else talks to Manager.
//
// # Two SQLite libraries share these files, and they do NOT see each other's locks
//
// litestream v0.5 uses modernc.org/sqlite for its own connections while knomit
// uses mattn/go-sqlite3 for its databases; knomit cannot switch to modernc
// because sqlite-vec has no modernc build. So two independent SQLite builds open
// the same files from the same process — and that specific combination defeats
// SQLite's locking. This is NOT the ordinary multi-connection case.
//
// Why: POSIX advisory record locks are per (process, inode), not per file
// descriptor, so two descriptors held by ONE process never conflict. SQLite's
// workaround is a private per-process inode table that mediates locks between
// its own connections before they ever reach the kernel — but that table belongs
// to one SQLite BUILD. Two builds in one process each keep their own, so neither
// sees the other's locks and the kernel will not arbitrate.
//
// Verified, not theoretical. With litestream tracking a database and holding a
// live connection, closing knomit's connection DELETES the -wal and removes the
// -shm out from under it — 3 times out of 3. SQLite only removes a WAL after
// taking an EXCLUSIVE lock, which litestream's reader should have made
// impossible.
//
// So mutual exclusion here is not enforced by the filesystem. The ONLY thing
// keeping the two apart is that knomit explicitly UNTRACKS a database before it
// closes, moves or replaces the file: Untrack closes litestream's connection
// first and synchronously, so knomit's own close is then genuinely the last one.
// Pause, repos.Manager.Archive/Restore, and the app tests' stopInstance helper
// all follow that pattern, and each says so at its call site.
//
// Therefore: any new code path that closes, renames, deletes or overwrites a
// database file while it is still TRACKED is unsafe. Observed consequences are
// not subtle — a final replica sync failing with "open <db>-wal: no such file or
// directory", which aborts whatever operation triggered it, and (under load,
// with a reader walking pages through cgo SQLite while litestream works the same
// file) SIGBUS killing the process outright. Add the untrack, or leave the file
// alone.
//
// This is under review as a design question — generalising the untrack-first
// pattern, moving litestream out of process, and accepting a narrower documented
// risk are all on the table — so treat the rule above as the current contract
// rather than as the settled design.
//
// The blank imports below register litestream's "file", "s3", and "gs"
// replica-client URL schemes (see litestream.RegisterReplicaClientFactory): a
// backend package must be imported for litestream.NewReplicaClientFromURL to
// resolve its scheme. "file" backs the local-disk test path; "s3" and "gs"
// are the documented production targets (s3://... or gs://..., including
// S3-compatible endpoints like Fly's Tigris — see the backup design doc).
// litestream also ships abs (Azure Blob), sftp, webdav, nats, and oss
// backends; none of those are part of the documented backup.url surface, so
// they are deliberately NOT imported here. Add a blank import for one only
// if it becomes a documented target — otherwise its URL scheme fails at
// Open()'s probe with "unsupported replica URL scheme", which is the correct
// behavior for an undocumented/unsupported target.
package backup

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/benbjohnson/litestream"
	_ "github.com/benbjohnson/litestream/file"
	_ "github.com/benbjohnson/litestream/gs"
	_ "github.com/benbjohnson/litestream/s3"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
)

// archivePrefix is the logical-name (and replica-path) namespace archived repo
// databases live under. It is a namespace no live repo can ever enter: repo
// names are [a-z0-9_-]+ (repos.isValidRepoName), so none of them can contain a
// slash, and the id after the prefix is a ksuid minted at archive time.
const archivePrefix = "archive/"

// archiveSnapshotRetention is the retention window the archive store advertises.
// It is "forever" expressed as a number, and it is belt to RetentionEnabled's
// braces: with deletion switched off the window is not consulted for remote
// files at all, but litestream still prunes the LOCAL LTX cache of anything the
// window calls expired, and there is no reason to churn that for a database
// which by definition never changes again.
const archiveSnapshotRetention = 100 * 365 * 24 * time.Hour

// ArchiveName maps an archive id to the logical database name its replica lives
// under. Callers outside this package go through TrackArchived/UntrackArchived
// and never build the name themselves — the prefix is this package's business.
func ArchiveName(archiveID string) string { return archivePrefix + archiveID }

// isArchiveName reports whether a logical name belongs to an archived database.
func isArchiveName(name string) bool { return strings.HasPrefix(name, archivePrefix) }

// ErrTrackedElsewhere is returned by Track when a name is already tracked
// against a DIFFERENT file.
//
// Silently accepting that (the obvious "already tracked, nothing to do") is a
// data-loss bug, not a harmless no-op: litestream.DB.init pins a file descriptor
// with a single os.Open(db.path) guarded by `if db.db != nil { return nil }`, so
// the tracked database keeps replicating the INODE it opened. If a repo is
// archived (its .db renamed away) and a new repo then claims the freed name, a
// swallowed Track leaves the new database replicated by nobody — no snapshot, no
// error, and Status still reporting the name as in sync.
var ErrTrackedElsewhere = errors.New("name is already tracked against a different file")

// DBStatus is one tracked database's replication state.
type DBStatus struct {
	Name       string
	LocalTXID  uint64
	RemoteTXID uint64
	InSync     bool
	LastError  string
}

// Manager owns the litestream store and the set of tracked databases.
type Manager struct {
	cfg  config.BackupConfig
	home string

	// opMu serialises Track and Untrack against each other so a name cannot be
	// registered and unregistered concurrently. It is deliberately NOT mu:
	// registering and (especially) unregistering block on litestream — a
	// database close performs a final replica sync WITH RETRY, bounded by
	// ShutdownSyncTimeout (30s by default). Holding mu across that would stall
	// every Status() call for the duration of an object-store hiccup, which is
	// the exact failure mode Status was already restructured to avoid.
	opMu sync.Mutex

	// mu guards the fields below and is held only for map/flag access — never
	// across a litestream call.
	mu  sync.RWMutex
	dbs map[string]*litestream.DB
	// store replicates LIVE databases under the configured retention.
	store *litestream.Store
	// archiveStore replicates ARCHIVED databases with retention disabled.
	//
	// It is a second store rather than a per-database setting because v0.5.15
	// has no per-database one that survives registration: Store.RegisterDB
	// overwrites db.RetentionEnabled from the store's own field immediately
	// before db.Open() copies it into the compactor, Store.SetRetentionEnabled
	// is store-wide, Store.EnforceSnapshotRetention reads the STORE's
	// SnapshotRetention for every database it sweeps, and litestream.Replica has
	// no retention field at all. The store a database is registered with is
	// therefore the only place the setting can be made to stick.
	//
	// Both stores are opened by Open and closed by Close; storeFor routes by
	// name. The extra cost is one set of compaction-monitor goroutines.
	archiveStore *litestream.Store
	closed       bool
}

// Open builds a Manager for the given config. It returns (nil, nil) when backup
// is disabled, so every caller can treat a nil *Manager as "do nothing".
//
// Open PROBES the replica target before returning. Credential and reachability
// failures must surface at boot, not silently at the first sync attempt.
func Open(cfg config.BackupConfig, home string) (*Manager, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("backup.Open: url is required when backup is enabled")
	}
	m := &Manager{cfg: cfg, home: home, dbs: map[string]*litestream.DB{}}

	if err := m.probe(context.Background()); err != nil {
		return nil, fmt.Errorf("backup.Open: replica target unreachable (%s): %w", cfg.URL, err)
	}

	m.store = newStore(cfg)
	if err := m.store.Open(context.Background()); err != nil {
		return nil, fmt.Errorf("backup.Open: store: %w", err)
	}

	// The archive store's retention settings are the whole reason it exists; see
	// the field comment on Manager.archiveStore.
	m.archiveStore = newStore(cfg)
	m.archiveStore.RetentionEnabled = false
	m.archiveStore.SnapshotRetention = archiveSnapshotRetention
	// Zero disables Store.monitorL0Retention outright (it only starts when both
	// L0Retention and its check interval are positive) and makes
	// DB.EnforceL0Retention return early, so no sweep can even reach the
	// RetentionEnabled check for an archived database.
	m.archiveStore.L0Retention = 0
	if err := m.archiveStore.Open(context.Background()); err != nil {
		// The live store is already running and nothing else will reclaim it —
		// Open returns nil, so the caller has no Manager to Close.
		_ = m.store.Close(context.Background())
		return nil, fmt.Errorf("backup.Open: archive store: %w", err)
	}

	log.Info().Str("url", cfg.URL).Str("instance", cfg.Instance).Msg("backup replication enabled")
	return m, nil
}

// newStore builds a litestream store with knomit's compaction levels and the
// configured intervals. Callers adjust retention afterwards.
func newStore(cfg config.BackupConfig) *litestream.Store {
	s := litestream.NewStore(nil, litestream.CompactionLevels{
		{Level: 0},
		{Level: 1, Interval: 10 * time.Second},
	})
	s.SnapshotInterval = cfg.SnapshotInterval
	s.SnapshotRetention = cfg.SnapshotRetention
	s.L0Retention = cfg.L0Retention
	return s
}

// storeFor returns the store a logical name's database belongs to. It is a pure
// function of the name, so Track and Untrack cannot disagree about which store
// holds a database.
func (m *Manager) storeFor(name string) *litestream.Store {
	if isArchiveName(name) {
		return m.archiveStore
	}
	return m.store
}

// probe verifies the replica target is reachable and credentials work.
//
// It performs a REAL round-trip to the object store rather than merely parsing
// the URL: the whole point is that bad credentials or an unreachable bucket
// fail the boot instead of surfacing later as a silent replication stall.
//
// litestream.ReplicaClient.LTXFiles returns a lazily-paginated iterator (the
// underlying AWS SDK paginator does not issue a request until the first
// Next()), so parsing the URL and constructing the client is not enough — we
// must drive the iterator at least once to force that first network call. One
// Next() is sufficient: it either returns an item (proving the round-trip
// succeeded) or sets the iterator's error (surfaced via Close()), and an empty
// result is success — a first boot has nothing there yet.
func (m *Manager) probe(ctx context.Context) error {
	client, err := litestream.NewReplicaClientFromURL(m.prefix("."))
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

// prefix builds the replica URL for one logical database.
func (m *Manager) prefix(rel string) string {
	return m.cfg.URL + "/" + path.Join(m.cfg.Instance, rel)
}

// Track begins replicating the database at dbPath under the given logical name.
// Re-tracking a name against the SAME file is a no-op; against a different file
// it is ErrTrackedElsewhere — see that variable for why silence would be a bug.
func (m *Manager) Track(name, dbPath string) error {
	if m == nil {
		return nil
	}
	// opMu, not mu: the store call below must not block Status(). See the field
	// comment on Manager.opMu.
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.RLock()
	existing, tracked := m.dbs[name]
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return fmt.Errorf("backup.Track: manager is closed")
	}
	if tracked {
		if existing.Path() != dbPath {
			return fmt.Errorf("backup.Track %q: %w (replicating %s, asked for %s); "+
				"the caller's database would be backed up by nothing",
				name, ErrTrackedElsewhere, existing.Path(), dbPath)
		}
		return nil
	}

	store := m.storeFor(name)
	db := litestream.NewDB(dbPath)
	db.MonitorInterval = m.cfg.MonitorInterval

	client, err := litestream.NewReplicaClientFromURL(m.prefix(m.relFor(name)))
	if err != nil {
		return fmt.Errorf("backup.Track %q: replica client: %w", name, err)
	}
	db.Replica = litestream.NewReplicaWithClient(db, client)

	// litestream.Store has no Add/Remove pair in v0.5.15; RegisterDB/UnregisterDB
	// fill that role (RegisterDB also calls db.Open(), so we must not call it
	// ourselves first). The store's registered-DB set is dynamic — RegisterDB
	// appends to it and starts monitoring immediately, so we don't need to
	// rebuild the store's DB slice on every Track/Untrack.
	if err := store.RegisterDB(db); err != nil {
		return fmt.Errorf("backup.Track %q: register with store: %w", name, err)
	}

	// Re-check closed under the WRITE lock, and hand the database back if Close
	// won the race. The check at the top of this function is not enough: mu is
	// not held across the registration above, so Close can run in between — and
	// nothing downstream would ever clean this database up. litestream's
	// Store.RegisterDB has no closed flag (it opens and appends to an
	// already-closed store), and DB's context comes from context.Background(),
	// so store.Close's cancel never reaches the monitor goroutine this just
	// started. Without the recheck the monitor and its SQLite read lock outlive
	// shutdown, replicating on behalf of a process that believes it stopped.
	//
	// The alternative — having Close take opMu — would close the same hole, but
	// it would put shutdown behind an in-flight Untrack, whose final replica
	// sync retries for up to ShutdownSyncTimeout. Shutdown should not wait 30s
	// on an unreachable object store, so the recheck is preferred.
	m.mu.Lock()
	closed = m.closed
	if !closed {
		m.dbs[name] = db
	}
	m.mu.Unlock()
	if closed {
		// Outside mu, as ever: this closes the database and its final sync can
		// block on the replica.
		if err := store.UnregisterDB(context.Background(), db.Path()); err != nil {
			return fmt.Errorf("backup.Track %q: manager closed mid-registration, and unregistering it failed: %w", name, err)
		}
		return fmt.Errorf("backup.Track: manager is closed")
	}

	log.Info().Str("db", name).Str("path", dbPath).Msg("backup: tracking database")
	return nil
}

// Untrack permanently stops replicating a database (archive, purge).
//
// The database is dropped from the tracked set BEFORE the store call, because
// UnregisterDB removes it from the store and closes it in that order: once it
// returns — error or not — the database is closed and no longer replicating, so
// leaving it in the map on failure would only make Status report a corpse.
// Callers that need replication back (Pause) must re-Track, not retry Untrack.
func (m *Manager) Untrack(name string) error {
	if m == nil {
		return nil
	}
	// opMu, not mu: UnregisterDB closes the database, whose final replica sync
	// RETRIES for up to ShutdownSyncTimeout. See the field comment on
	// Manager.opMu.
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	db, ok := m.dbs[name]
	delete(m.dbs, name)
	m.mu.Unlock()
	if !ok {
		return nil
	}

	if err := m.storeFor(name).UnregisterDB(context.Background(), db.Path()); err != nil {
		return fmt.Errorf("backup.Untrack %q: %w", name, err)
	}
	log.Info().Str("db", name).Msg("backup: stopped tracking database")
	return nil
}

// Status reports replication state for every tracked database.
//
// db.SyncStatus performs a REMOTE round-trip per call — it drains the entire
// level-0 LTX file listing from the replica to find the remote position (see
// litestream's Replica.calcPos / MaxLTXFileInfo), unbounded by bucket size and
// with no caching. Status() therefore snapshots the tracked-DB set under the
// manager lock and releases it BEFORE making any of those calls: holding the
// lock across a blocking network call would stall a concurrent Track/Untrack
// (which need the exclusive Lock) for as long as any Status() round-trip is
// in flight.
//
// Status() does not itself cache or rate-limit the underlying LIST cost — it
// always reflects live-as-of-now state, at the cost of one remote LIST per
// tracked database per call. Callers that poll this on a schedule (e.g. a
// /metrics or /runtime/status handler) own that trade-off: either accept the
// LIST cost at their scrape interval, or add their own TTL cache in front of
// Status() if the interval is tight enough to matter. This method
// deliberately stays simple rather than growing a caching layer.
func (m *Manager) Status(ctx context.Context) []DBStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	snapshot := make(map[string]*litestream.DB, len(m.dbs))
	for name, db := range m.dbs {
		snapshot[name] = db
	}
	m.mu.RUnlock()

	out := make([]DBStatus, 0, len(snapshot))
	for name, db := range snapshot {
		st := DBStatus{Name: name}
		sync, err := db.SyncStatus(ctx)
		if err != nil {
			st.LastError = err.Error()
		} else {
			st.LocalTXID = uint64(sync.LocalTXID)
			st.RemoteTXID = uint64(sync.RemoteTXID)
			st.InSync = sync.InSync
		}
		out = append(out, st)
	}
	return out
}

// Close stops replication for every tracked database.
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()

	// Outside mu, and CONCURRENTLY. Each store's close performs a final replica
	// sync per database with retry, bounded by ShutdownSyncTimeout (30s by
	// default). Holding mu across that would stall every Status() and Track()
	// call for the duration of an object-store hiccup — the failure mode the
	// opMu/mu split exists to prevent — and closing the two in sequence would
	// double the worst case to a minute for no benefit, since the stores share
	// nothing.
	//
	// Both, always: the archive store owns real databases too, and one left open
	// outlives the process's belief that it has stopped replicating.
	var wg sync.WaitGroup
	var archiveErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		archiveErr = m.archiveStore.Close(ctx)
	}()
	err := m.store.Close(ctx)
	wg.Wait()
	if err != nil {
		return err
	}
	return archiveErr
}

// relFor maps a logical database name to its path under the instance prefix.
// "control" is special-cased; archived repos already carry their archive/
// namespace in the name, so they become a sibling of repos/ rather than living
// inside it — the archive id is globally unique, so nesting would buy nothing.
func (m *Manager) relFor(name string) string {
	if name == "control" {
		return "control.db"
	}
	if isArchiveName(name) {
		return name + ".db"
	}
	return path.Join("repos", name+".db")
}

// TrackArchived replicates an archived repo's database under the archive prefix,
// where retention is DISABLED.
//
// An archived database stops changing, so under the ordinary snapshot retention
// its snapshots would simply expire — turning "archive" (a documented,
// recoverable state) into "delete" on a delay. Archived databases are idle, so
// keeping them costs one snapshot each and no ongoing traffic.
func (m *Manager) TrackArchived(archiveID, dbPath string) error {
	if m == nil {
		return nil
	}
	return m.Track(ArchiveName(archiveID), dbPath)
}

// UntrackArchived stops replicating an archived repo's database. It is the
// counterpart of TrackArchived, used when the archive is purged or restored.
func (m *Manager) UntrackArchived(archiveID string) error {
	if m == nil {
		return nil
	}
	return m.Untrack(ArchiveName(archiveID))
}

// DeleteArchivedReplica permanently removes an archived database's objects from
// the replica. Callers must Untrack it first — deleting the objects out from
// under a live replica only invites it to write them straight back.
//
// This is the one place knomit deletes replica data, and under the archive
// prefix it has to be: the archive store runs with retention disabled and a
// century-long window precisely so nothing ages out, which means nothing ever
// reclaims these objects either. A live repo's prefix self-heals as retention
// prunes its chain; an archived one cannot, by construction. Without this,
// "purge" would mean "delete locally, keep forever in the bucket" — a broken
// promise to anyone purging to make data go away, and unbounded storage growth.
//
// Scope is exactly one archived database: the client is built from that
// archive's own prefix, and DeleteAll is prefix-scoped in all three backends
// this package registers — file does RemoveAll on its own directory, s3
// paginates ListObjectsV2 under Path+"/" and batch-deletes only those keys, and
// gs iterates Objects with Prefix Path+"/" and deletes only those. Pinned by
// TestDeleteArchivedReplicaRemovesOnlyThatArchive against the file backend.
func (m *Manager) DeleteArchivedReplica(archiveID string) error {
	if m == nil {
		return nil
	}
	ctx := context.Background()
	url := m.prefix(m.relFor(ArchiveName(archiveID)))
	client, err := litestream.NewReplicaClientFromURL(url)
	if err != nil {
		return fmt.Errorf("backup.DeleteArchivedReplica %q: replica client: %w", archiveID, err)
	}
	if err := client.Init(ctx); err != nil {
		return fmt.Errorf("backup.DeleteArchivedReplica %q: init replica client: %w", archiveID, err)
	}
	if err := client.DeleteAll(ctx); err != nil {
		return fmt.Errorf("backup.DeleteArchivedReplica %q: %w", archiveID, err)
	}
	log.Info().Str("id", archiveID).Str("url", url).Msg("backup: deleted an archived database's replica objects")
	return nil
}
