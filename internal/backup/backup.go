// Package backup owns knomit's continuous replication to object storage. It is
// the ONLY package that imports litestream — everything else talks to Manager.
//
// Note that litestream v0.5 uses modernc.org/sqlite for its own connections
// while knomit uses mattn/go-sqlite3 for its databases. Both open the same
// files; they coordinate through POSIX locks, which is SQLite's ordinary
// multi-connection case. knomit cannot switch to modernc because sqlite-vec has
// no modernc build.
//
// The blank imports below register litestream's "file" and "s3" replica-client
// URL schemes (see litestream.RegisterReplicaClientFactory): a backend package
// must be imported for litestream.NewReplicaClientFromURL to resolve its
// scheme. "file" backs the local-disk test path; "s3" is the intended
// production target. Add further blank imports (gs, abs, sftp, webdav, ...) if
// knomit ever needs to replicate to those backends.
package backup

import (
	"context"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/benbjohnson/litestream"
	_ "github.com/benbjohnson/litestream/file"
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("backup.Track: manager is closed")
	}
	if _, ok := m.dbs[name]; ok {
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
	m.dbs[name] = db
	log.Info().Str("db", name).Str("path", dbPath).Msg("backup: tracking database")
	return nil
}

// Untrack permanently stops replicating a database (archive, purge).
func (m *Manager) Untrack(name string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	db, ok := m.dbs[name]
	if !ok {
		return nil
	}
	delete(m.dbs, name)
	if err := m.store.UnregisterDB(context.Background(), db.Path()); err != nil {
		return fmt.Errorf("backup.Untrack %q: %w", name, err)
	}
	log.Info().Str("db", name).Msg("backup: stopped tracking database")
	return nil
}

// Status reports replication state for every tracked database.
func (m *Manager) Status(ctx context.Context) []DBStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]DBStatus, 0, len(m.dbs))
	for name, db := range m.dbs {
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
