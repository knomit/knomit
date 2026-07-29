// Package backup owns knomit's continuous replication to object storage. It is
// the ONLY package that imports litestream — everything else talks to Manager.
//
// Note that litestream v0.5 uses modernc.org/sqlite for its own connections
// while knomit uses mattn/go-sqlite3 for its databases. Both open the same
// files; they coordinate through POSIX locks, which is SQLite's ordinary
// multi-connection case. knomit cannot switch to modernc because sqlite-vec has
// no modernc build.
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
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/benbjohnson/litestream"
	_ "github.com/benbjohnson/litestream/file"
	_ "github.com/benbjohnson/litestream/gs"
	_ "github.com/benbjohnson/litestream/s3"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
)

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
	mu     sync.RWMutex
	dbs    map[string]*litestream.DB
	store  *litestream.Store
	closed bool
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

	m.store = litestream.NewStore(nil, litestream.CompactionLevels{
		{Level: 0},
		{Level: 1, Interval: 10 * time.Second},
	})
	m.store.SnapshotInterval = cfg.SnapshotInterval
	m.store.SnapshotRetention = cfg.SnapshotRetention
	m.store.L0Retention = cfg.L0Retention

	if err := m.store.Open(context.Background()); err != nil {
		return nil, fmt.Errorf("backup.Open: store: %w", err)
	}
	log.Info().Str("url", cfg.URL).Str("instance", cfg.Instance).Msg("backup replication enabled")
	return m, nil
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
// Tracking an already-tracked name is a no-op.
func (m *Manager) Track(name, dbPath string) error {
	if m == nil {
		return nil
	}
	// opMu, not mu: the store call below must not block Status(). See the field
	// comment on Manager.opMu.
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.RLock()
	_, tracked := m.dbs[name]
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return fmt.Errorf("backup.Track: manager is closed")
	}
	if tracked {
		return nil
	}

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
	if err := m.store.RegisterDB(db); err != nil {
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
		if err := m.store.UnregisterDB(context.Background(), db.Path()); err != nil {
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

	if err := m.store.UnregisterDB(context.Background(), db.Path()); err != nil {
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
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	return m.store.Close(ctx)
}

// relFor maps a logical database name to its path under the instance prefix.
// "control" is special-cased; archived repos live under archive/ with retention
// disabled (see Archive integration).
func (m *Manager) relFor(name string) string {
	if name == "control" {
		return "control.db"
	}
	return path.Join("repos", name+".db")
}
