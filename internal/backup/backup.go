// Package backup owns knomit's continuous replication to object storage. It is
// a CLIENT: the replication itself runs in a separate process, the knomit-backup
// agent (tools/backup, wrapping internal/backupagent). Nothing knomit links
// imports litestream — verify with
//
//	go list -deps . | grep -E 'litestream|modernc'
//
// which must print nothing.
//
// # Why a separate process
//
// litestream v0.5 drives its own SQLite connections through
// modernc.org/sqlite; knomit drives its databases through the cgo
// mattn/go-sqlite3 build and cannot switch, because sqlite-vec has no modernc
// build. Two SQLite BUILDS inside ONE process do not see each other's locks:
// POSIX advisory record locks do not conflict between file descriptors held by
// the same process, and SQLite's compensating per-process inode table — which
// mediates locks between its own connections before they reach the kernel —
// belongs to one BUILD. Two builds each keep their own, so neither sees the
// other's locks and the kernel will not arbitrate.
//
// That was demonstrated, not theorised. With litestream tracking a database and
// holding a live connection, closing knomit's connection DELETED the -wal and
// removed the -shm out from under it, 3 runs out of 3 — even though SQLite only
// removes a WAL after taking an EXCLUSIVE lock, which litestream's reader should
// have made impossible. One fact explained three symptoms chased separately: an
// intermittent archive failure, "invalid wal header magic: 0" on shutdown, and a
// silently lost write tail.
//
// Across PROCESSES those same locks work exactly as SQLite intends. So this is
// not a mitigation of that hazard, it is its removal: the previous rule
// ("untrack before you close, move or replace a database file") is no longer
// load-bearing for CORRECTNESS, because the kernel now enforces the exclusion
// the same-process case could not. Untracking first is still the right thing to
// do before REPLACING a file — see Pause — but for a different reason: a swapped
// file is a new identity, and the LTX chain must not be continued across it.
//
// # Nil is "backup disabled"
//
// Open returns (nil, nil) when replication is switched off, and every exported
// method here is nil-receiver-safe. Callers therefore need no scattered nil
// checks.
package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"

	"knomit/internal/backupproto"
	"knomit/internal/config"
)

// agentBinary is the name of the child process knomit spawns.
const agentBinary = "knomit-backup"

// archivePrefix is the logical-name (and replica-path) namespace archived repo
// databases live under. It is a namespace no live repo can ever enter: repo
// names are [a-z0-9_-]+ (repos.isValidRepoName), so none of them can contain a
// slash, and the id after the prefix is a ksuid minted at archive time.
const archivePrefix = "archive/"

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

// dbEntry is what knomit believes about one replicated database. It is the
// SOURCE OF TRUTH for re-establishment: the agent holds its tracked set in
// memory, so after a crash this map is the only record of what must start
// replicating again.
type dbEntry struct {
	path     string
	archived bool
}

// Manager is knomit's handle on the replication agent.
//
// # Locking
//
// opMu serialises the MUTATIONS of the tracked set — Track, Untrack, and the
// reset-and-retrack inside Pause — against each other, so a name cannot be
// registered and unregistered concurrently. It is deliberately not mu:
// untracking blocks on the agent, whose reply waits on a final replica sync
// with retry, and holding mu across that would stall every Status call for the
// duration of an object-store hiccup.
//
// mu guards the fields below and is held only for map and flag access. It is
// never held across a protocol round trip, and never across the pipe write
// inside one — a pipe write is still a blocking call, which is the part of the
// old in-process discipline that survives here unchanged.
type Manager struct {
	cfg  config.BackupConfig
	home string
	cl   *client

	opMu sync.Mutex

	mu     sync.RWMutex
	dbs    map[string]dbEntry
	closed bool
}

// Open builds a Manager for the given config. It returns (nil, nil) when backup
// is disabled, so every caller can treat a nil *Manager as "do nothing".
//
// Open locates and STARTS the agent, and the agent PROBES the replica target
// before Open returns. A missing binary, an agent that will not start, and
// credential or reachability failures all fail the boot — none of them degrade
// to "backup silently disabled", because a knomit that comes up believing it is
// replicating when it is not is the exact outcome this feature exists to
// prevent.
func Open(cfg config.BackupConfig, home string) (*Manager, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("backup.Open: url is required when backup is enabled")
	}

	bin, err := locateAgent(cfg.AgentPath, home)
	if err != nil {
		return nil, fmt.Errorf("backup.Open: %w", err)
	}
	return openWithAgent(cfg, home, bin, nil)
}

// openWithAgent is Open with the child binary already chosen. Tests use it to
// run a scripted agent in place of the real one; nothing in production calls it
// with a non-nil env.
func openWithAgent(cfg config.BackupConfig, home, bin string, env []string) (*Manager, error) {
	m := &Manager{cfg: cfg, home: home, dbs: map[string]dbEntry{}}
	m.cl = newClient(bin, env)
	m.cl.establish = m.establish
	if err := m.cl.start(context.Background()); err != nil {
		return nil, fmt.Errorf("backup.Open: %w", err)
	}

	log.Info().Str("url", cfg.URL).Str("instance", cfg.Instance).Str("agent", bin).
		Int("agent_pid", m.cl.currentPID()).
		Msg("backup replication enabled")
	return m, nil
}

// locateAgent finds the knomit-backup binary, or explains where it looked.
//
// The order is deliberate: an explicit override wins; then the directory the
// running knomit came from, which is how every packaged layout works (the
// Makefile and the Dockerfile both put the two binaries side by side); then
// $KNOMIT_HOME/bin, for an operator who dropped it beside the data; then PATH.
//
// Not finding it is a hard failure with every candidate named, so the fix is
// obvious from the message alone. Same fail-at-boot principle as the replica
// probe.
func locateAgent(override, home string) (string, error) {
	var searched []string
	try := func(p string) (string, bool) {
		if p == "" {
			return "", false
		}
		searched = append(searched, p)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			return "", false
		}
		return p, true
	}

	if override != "" {
		if p, ok := try(override); ok {
			return p, nil
		}
		return "", fmt.Errorf("the configured backup agent %q is not an executable file", override)
	}

	if exe, err := os.Executable(); err == nil {
		if p, ok := try(filepath.Join(filepath.Dir(exe), agentBinary)); ok {
			return p, nil
		}
	}
	if home != "" {
		if p, ok := try(filepath.Join(home, "bin", agentBinary)); ok {
			return p, nil
		}
	}
	if p, err := exec.LookPath(agentBinary); err == nil {
		return p, nil
	}
	searched = append(searched, "$PATH")

	return "", fmt.Errorf(
		"the %s agent was not found (searched: %s); backup is enabled, and knomit will not start "+
			"pretending to replicate without it — install the agent beside knomit or set backup.agent_path "+
			"(KNOMIT_BACKUP_AGENT)",
		agentBinary, strings.Join(searched, ", "))
}

// establish brings a freshly spawned agent generation up to knomit's view of
// the world: configured, probed, and replicating every database knomit believes
// is being replicated.
//
// It runs on EVERY generation, not just the first. The agent keeps its tracked
// set in memory, so after a crash the only record of it is m.dbs — and a
// database that quietly stops replicating with nobody noticing is precisely the
// failure this feature exists to prevent.
//
// A failed open fails the generation: without stores there is nothing to track
// against, so the supervisor should back off and try again. A failed TRACK does
// not, because refusing the whole generation over one database would take the
// other repos down with it; it is logged at error level and the rest continue.
func (m *Manager) establish(ctx context.Context, cn *conn) error {
	if err := m.cl.callOn(ctx, cn, backupproto.MethodOpen, backupproto.OpenParams{
		Config: backupproto.Config{
			URL:               m.cfg.URL,
			Instance:          m.cfg.Instance,
			SnapshotInterval:  m.cfg.SnapshotInterval,
			SnapshotRetention: m.cfg.SnapshotRetention,
			L0Retention:       m.cfg.L0Retention,
			MonitorInterval:   m.cfg.MonitorInterval,
		},
	}, nil); err != nil {
		return err
	}

	m.mu.RLock()
	snapshot := make(map[string]dbEntry, len(m.dbs))
	for name, e := range m.dbs {
		snapshot[name] = e
	}
	m.mu.RUnlock()

	for name, e := range snapshot {
		if err := m.cl.callOn(ctx, cn, backupproto.MethodTrack, m.trackParams(name, e), nil); err != nil {
			log.Error().Err(err).Str("db", name).Str("path", e.path).
				Msg("backup: a tracked database could not be re-established after an agent restart; it is NOT being replicated")
		}
	}
	return nil
}

// trackParams builds the wire form of one tracked database. relFor is applied
// here because the mapping from a logical name to a replica path is knomit's
// naming policy, not the agent's.
func (m *Manager) trackParams(name string, e dbEntry) backupproto.TrackParams {
	return backupproto.TrackParams{
		Name:     name,
		Path:     e.path,
		Rel:      m.relFor(name),
		Archived: e.archived,
	}
}

// Track begins replicating the database at dbPath under the given logical name.
// Re-tracking a name against the SAME file is a no-op; against a different file
// it is ErrTrackedElsewhere — see that variable for why silence would be a bug.
func (m *Manager) Track(name, dbPath string) error {
	if m == nil {
		return nil
	}
	// opMu, not mu: the agent round trip below must not block Status.
	m.opMu.Lock()
	defer m.opMu.Unlock()

	entry := dbEntry{path: dbPath, archived: isArchiveName(name)}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("backup.Track: manager is closed")
	}
	existing, tracked := m.dbs[name]
	if tracked {
		m.mu.Unlock()
		if existing.path != dbPath {
			return fmt.Errorf("backup.Track %q: %w (replicating %s, asked for %s); "+
				"the caller's database would be backed up by nothing",
				name, ErrTrackedElsewhere, existing.path, dbPath)
		}
		return nil
	}
	// Recorded BEFORE the call, deliberately. If the agent dies mid-Track the
	// supervisor re-establishes from this map, and a name missing from it would
	// be a database nobody replicates and nobody reports. The reverse ordering
	// has no such self-healing: the agent would be tracking a database knomit
	// has no record of only if the call succeeded and we then failed to record
	// it, which cannot happen here.
	m.dbs[name] = entry
	m.mu.Unlock()

	if err := m.cl.call(context.Background(), backupproto.MethodTrack, m.trackParams(name, entry), nil); err != nil {
		m.mu.Lock()
		// Only drop the entry if it is still the one this call added: a
		// concurrent Untrack may already have removed it.
		if cur, ok := m.dbs[name]; ok && cur == entry {
			delete(m.dbs, name)
		}
		m.mu.Unlock()
		// Best-effort compensation: the agent may have accepted the track and
		// then failed on the reply. Not for ErrTrackedElsewhere — there the
		// agent is replicating something else under this name, and untracking
		// it would stop a database that is not ours to stop.
		if !errors.Is(err, ErrTrackedElsewhere) {
			_ = m.cl.call(context.Background(), backupproto.MethodUntrack, backupproto.UntrackParams{Name: name}, nil)
		}
		return fmt.Errorf("backup.Track %q: %w", name, err)
	}

	log.Info().Str("db", name).Str("path", dbPath).Msg("backup: tracking database")
	return nil
}

// Untrack permanently stops replicating a database (archive, purge).
//
// The database is dropped from knomit's record BEFORE the agent call, because
// the agent unregisters and closes in that order: once its reply comes back —
// error or not — the database is closed and no longer replicating, so leaving
// it in the map on failure would only make Status report a corpse, and would
// make the supervisor re-establish it on the next restart. Callers that need
// replication back (Pause) must re-Track, not retry Untrack.
func (m *Manager) Untrack(name string) error {
	if m == nil {
		return nil
	}
	// opMu, not mu: the agent's reply waits on a final replica sync with retry.
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	_, ok := m.dbs[name]
	delete(m.dbs, name)
	m.mu.Unlock()
	if !ok {
		return nil
	}

	if err := m.cl.call(context.Background(), backupproto.MethodUntrack, backupproto.UntrackParams{Name: name}, nil); err != nil {
		return fmt.Errorf("backup.Untrack %q: %w", name, err)
	}
	log.Info().Str("db", name).Msg("backup: stopped tracking database")
	return nil
}

// Status reports replication state for every tracked database.
//
// The agent performs a REMOTE round-trip per database — it drains the entire
// level-0 LTX file listing from the replica to find the remote position,
// unbounded by bucket size and with no caching. Status does not itself cache or
// rate-limit that cost; it always reflects live-as-of-now state. Callers that
// poll it on a schedule own that trade-off and should add their own TTL cache
// if their interval is tight enough to matter.
//
// Status is RECONCILED against knomit's own record, never taken from the agent
// alone. Two cases make that necessary, and both are the same mistake:
//
//   - The agent is down. Reporting an empty list would say "nothing is being
//     replicated" in the one voice an operator reads as "all clear".
//   - The agent is up but does not know about a database knomit does — a track
//     that failed during re-establishment after a crash, which is logged and
//     then abandoned. Building the answer from the agent's reply alone would
//     omit that name entirely, so the database would stop replicating and
//     disappear from the one surface that could have said so.
//
// Both are reported as an entry with LastError set. A name the AGENT knows and
// knomit does not is reported too, unaltered: knomit cannot vouch for it, but
// hiding a replica nobody is meant to be running would be worse.
func (m *Manager) Status(ctx context.Context) []DBStatus {
	if m == nil {
		return nil
	}
	var res backupproto.StatusResult
	callErr := m.cl.call(ctx, backupproto.MethodStatus, nil, &res)

	m.mu.RLock()
	expected := make(map[string]struct{}, len(m.dbs))
	for name := range m.dbs {
		expected[name] = struct{}{}
	}
	m.mu.RUnlock()

	if callErr != nil {
		out := make([]DBStatus, 0, len(expected))
		for name := range expected {
			out = append(out, DBStatus{Name: name, LastError: callErr.Error()})
		}
		return out
	}

	out := make([]DBStatus, 0, max(len(res.Databases), len(expected)))
	for _, db := range res.Databases {
		delete(expected, db.Name)
		out = append(out, DBStatus{
			Name:       db.Name,
			LocalTXID:  db.LocalTXID,
			RemoteTXID: db.RemoteTXID,
			InSync:     db.InSync,
			LastError:  db.LastError,
		})
	}
	for name := range expected {
		out = append(out, DBStatus{
			Name: name,
			LastError: "not registered with the replication agent: knomit believes this database is " +
				"being replicated and the agent does not, so it is NOT being backed up",
		})
	}
	return out
}

// Close stops replication for every tracked database and shuts the agent down.
//
// It asks for a clean shutdown first — the agent's final replica sync per
// database is what makes the backup current as of shutdown — then closes the
// pipe and, if the agent has not exited within the grace period, kills it.
// Close never returns leaving an agent behind.
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

	return m.cl.close(ctx)
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
// Scope is exactly one archived database: the agent builds its replica client
// from that archive's own prefix, and DeleteAll is prefix-scoped in all three
// registered backends — file does RemoveAll on its own directory, s3 paginates
// ListObjectsV2 under Path+"/" and batch-deletes only those keys, and gs
// iterates Objects with Prefix Path+"/" and deletes only those. Pinned by
// TestDeleteArchivedReplicaRemovesOnlyThatArchive against the file backend.
func (m *Manager) DeleteArchivedReplica(archiveID string) error {
	if m == nil {
		return nil
	}
	rel := m.relFor(ArchiveName(archiveID))
	if err := m.cl.call(context.Background(), backupproto.MethodDeleteReplica,
		backupproto.DeleteReplicaParams{Rel: rel}, nil); err != nil {
		return fmt.Errorf("backup.DeleteArchivedReplica %q: %w", archiveID, err)
	}
	log.Info().Str("id", archiveID).Str("rel", rel).Msg("backup: deleted an archived database's replica objects")
	return nil
}
