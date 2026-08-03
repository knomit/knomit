// Package backup owns knomit's continuous replication to object storage. It is
// a CLIENT: the replication itself runs in a separate process, the knomit-backup
// agent (tools/backup, wrapping tools/backup/agent). Nothing knomit links
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
	"sync/atomic"

	"github.com/rs/zerolog/log"

	"knomit/internal/backup/proto"
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

// DBStatus is one database's replication state.
//
// LastSyncUnix is litestream's own record of when this database last completed
// a replica sync, carried through from the agent. Zero means NEVER SYNCED and
// must not be rendered as a timestamp — see proto.DBStatus.
//
// Paused means a store swap deliberately untracked the database and has not
// resumed it yet. It is neither healthy nor an error, and it is reported
// precisely because the alternative is worse: a paused database is not in the
// tracked set, so without this it would vanish from Status entirely and a pause
// that never resumed would read as an all-clear.
type DBStatus struct {
	Name         string
	LocalTXID    uint64
	RemoteTXID   uint64
	InSync       bool
	LastSyncUnix int64
	Paused       bool
	LastError    string
}

// dbEntry is what knomit believes about one replicated database. It is the
// SOURCE OF TRUTH for re-establishment: the agent holds its tracked set in
// memory, so after a crash this map is the only record of what must start
// replicating again.
type dbEntry struct {
	path     string
	archived bool
	// pending is true while the Track that recorded this entry is still in
	// flight. The entry is written BEFORE the agent call — see Track for why —
	// so between those two moments knomit legitimately believes in a database
	// the agent has never heard of. Status must not report that as a database
	// that has stopped being backed up.
	pending bool
	// seq stamps every write to this name, so Status can tell an entry that has
	// not moved since it asked from one that has. A pending flag alone is not
	// enough: a Pause resume can untrack and fully re-track a name inside one
	// Status round trip, leaving a settled entry the agent's older reply could
	// not have known about.
	seq uint64
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

	// nextSeq stamps dbEntry writes. Monotonic, never reused, and only ever
	// compared for equality.
	nextSeq atomic.Uint64

	mu  sync.RWMutex
	dbs map[string]dbEntry
	// paused holds the names Pause has untracked and resume has not yet put
	// back. It is guarded by mu. Pause adds a name only after Untrack has
	// removed it from dbs, and resume clears it only after Track has put it
	// back — so the two maps overlap for exactly the width of that last step,
	// which pausedStatus resolves in favour of the live entry.
	//
	// Its only consumer is Status. Without it a paused database is in neither
	// map and appears nowhere, so an operator watching the backup surface during
	// a store swap — or after one that failed to resume — sees a clean bill of
	// health for a repo nothing is replicating.
	paused map[string]struct{}
	closed bool
}

// Open builds a Manager for the given config. It returns (nil, nil) when backup
// is disabled, so every caller can treat a nil *Manager as "do nothing".
//
// Open locates and STARTS the agent, and the agent PROBES the replica target
// before Open returns, so a missing binary, an agent that will not start, and
// credential or reachability failures are all reported HERE rather than showing
// up later as a replica that silently never fills.
//
// The error does not stop the server, though. app.Bootstrap logs it and boots
// without replication, because the replica is a warm-start cache: every
// database it holds is rebuildable from git, so an unreachable bucket costs
// boot time and not data, and refusing to start would turn a cache miss into an
// outage. What the error DOES buy is that the condition is loud and attributed
// at the moment it happens — nobody has to infer it from an empty bucket.
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
	m := &Manager{cfg: cfg, home: home, dbs: map[string]dbEntry{}, paused: map[string]struct{}{}}
	m.cl = newClient(bin, env)
	m.cl.restoreBudget = cfg.RestoreTimeout
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
// Not finding it is an error with every candidate named, so the fix is obvious
// from the message alone. Like every other Open failure it is reported and not
// fatal — knomit starts without replication rather than refusing to run — so
// the message is the whole of what an operator gets, and it has to be enough.
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
		"the %s agent was not found (searched: %s); backup is enabled, so knomit will start but will "+
			"replicate NOTHING until this is fixed — install the agent beside knomit or set "+
			"backup.agent_path (KNOMIT_BACKUP_AGENT)",
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
	if err := m.cl.callOn(ctx, cn, proto.MethodOpen, proto.OpenParams{
		Config: proto.Config{
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
		if err := m.cl.callOn(ctx, cn, proto.MethodTrack, m.trackParams(name, e), nil); err != nil {
			log.Error().Err(err).Str("db", name).Str("path", e.path).
				Msg("backup: a tracked database could not be re-established after an agent restart; it is NOT being replicated")
		}
	}
	return nil
}

// trackParams builds the wire form of one tracked database. relFor is applied
// here because the mapping from a logical name to a replica path is knomit's
// naming policy, not the agent's.
func (m *Manager) trackParams(name string, e dbEntry) proto.TrackParams {
	return proto.TrackParams{
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

	entry := dbEntry{path: dbPath, archived: isArchiveName(name), pending: true, seq: m.nextSeq.Add(1)}

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
	//
	// It is recorded PENDING, because for the duration of the call knomit
	// believes in a database the agent has genuinely not been told about yet,
	// and Status must not mistake that for one that has stopped replicating.
	m.dbs[name] = entry
	m.mu.Unlock()

	if err := m.cl.call(context.Background(), proto.MethodTrack, m.trackParams(name, entry), nil); err != nil {
		m.mu.Lock()
		// Only drop the entry if it is still the one this call added: a
		// concurrent Untrack may already have removed it, and a later Track may
		// have replaced it.
		if cur, ok := m.dbs[name]; ok && cur == entry {
			delete(m.dbs, name)
		}
		m.mu.Unlock()
		// Best-effort compensation: the agent may have accepted the track and
		// then failed on the reply. Not for ErrTrackedElsewhere — there the
		// agent is replicating something else under this name, and untracking
		// it would stop a database that is not ours to stop.
		//
		// Note this runs while opMu is STILL HELD, so a Track that fails on a
		// budget and then compensates blocks other mutations for up to two
		// budgets, not one. That is deliberate: leaving the agent replicating a
		// database knomit has just forgotten is worse than the extra wait, and
		// the wait is bounded either way.
		if !errors.Is(err, ErrTrackedElsewhere) {
			_ = m.cl.call(context.Background(), proto.MethodUntrack, proto.UntrackParams{Name: name}, nil)
		}
		return fmt.Errorf("backup.Track %q: %w", name, err)
	}

	// Settled: the agent has it. A fresh seq, so a Status round trip that
	// started before this moment cannot mistake the entry for one the agent
	// should already have known about.
	m.mu.Lock()
	if cur, ok := m.dbs[name]; ok && cur == entry {
		m.dbs[name] = dbEntry{path: dbPath, archived: entry.archived, seq: m.nextSeq.Add(1)}
	}
	m.mu.Unlock()

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
	// A paused name is dropped here too, and this is not housekeeping. Pause is
	// TEMPORARY and Untrack is PERMANENT, and the combination is reachable:
	// repos.SwapStore resumes from a defer, a failed resume deliberately keeps
	// the mark, and archiving or purging that repo then untracks it. Leaving the
	// mark would make Status report a paused database that no longer exists, with
	// nothing left in the system that could ever clear it.
	//
	// The early return below still applies: the agent untracked this database
	// when it was paused, so there is nothing left to ask it to stop.
	delete(m.paused, name)
	m.mu.Unlock()
	if !ok {
		return nil
	}

	if err := m.cl.call(context.Background(), proto.MethodUntrack, proto.UntrackParams{Name: name}, nil); err != nil {
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
//
// The "the agent does not know it" alarm must be TRUE when it fires, or it
// trains operators to ignore it — which would destroy the only thing this
// reconciliation buys. Two ordinary races would otherwise raise it falsely:
//
//   - Track records its entry before calling the agent, so any Track that
//     begins while this round trip is in flight is in knomit's map and
//     legitimately absent from the reply. The pending flag excludes those.
//   - A Pause resume can untrack and fully re-track a name inside one round
//     trip, leaving a SETTLED entry the agent's older reply could not have
//     mentioned. The seq stamp excludes those: an entry is only reported
//     missing if it has not been written since before the request went out.
//
// Taking the snapshot before the call and comparing it after is what makes both
// checks meaningful; reordering alone would only trade the false positive for a
// symmetric false negative (a database untracked-then-lost would go unreported).
func (m *Manager) Status(ctx context.Context) []DBStatus {
	if m == nil {
		return nil
	}

	// Before the request goes out: what knomit believed, and how old that
	// belief is.
	m.mu.RLock()
	before := make(map[string]uint64, len(m.dbs))
	for name, e := range m.dbs {
		if !e.pending {
			before[name] = e.seq
		}
	}
	m.mu.RUnlock()

	var res proto.StatusResult
	callErr := m.cl.call(ctx, proto.MethodStatus, nil, &res)

	if callErr != nil {
		// The agent is unreachable, so knomit's own record is the only truth
		// available — including entries still pending, whose Track is failing
		// for the same reason.
		m.mu.RLock()
		names := make([]string, 0, len(m.dbs))
		for name := range m.dbs {
			names = append(names, name)
		}
		m.mu.RUnlock()
		out := make([]DBStatus, 0, len(names))
		for _, name := range names {
			out = append(out, DBStatus{Name: name, LastError: callErr.Error()})
		}
		return append(out, m.pausedStatus()...)
	}

	out := make([]DBStatus, 0, max(len(res.Databases), len(before)))
	for _, db := range res.Databases {
		delete(before, db.Name)
		out = append(out, DBStatus{
			Name:         db.Name,
			LocalTXID:    db.LocalTXID,
			RemoteTXID:   db.RemoteTXID,
			InSync:       db.InSync,
			LastSyncUnix: db.LastSyncUnix,
			LastError:    db.LastError,
		})
	}

	// Whatever is left was believed-in before the request and unmentioned in the
	// reply. Report it only if the belief has not changed since.
	m.mu.RLock()
	for name, seq := range before {
		cur, ok := m.dbs[name]
		if !ok || cur.pending || cur.seq != seq {
			continue
		}
		out = append(out, DBStatus{
			Name: name,
			LastError: "not registered with the replication agent: knomit believes this database is " +
				"being replicated and the agent does not, so it is NOT being backed up",
		})
	}
	m.mu.RUnlock()
	return append(out, m.pausedStatus()...)
}

// pausedStatus reports the databases Pause has taken out of the tracked set.
//
// They cannot come from the agent — it has genuinely untracked them — and they
// are not errors, so they are appended as their own state. A pause is normally
// a second or two of a store swap and is invisible in practice; the case this
// exists for is the one where resume never ran.
func (m *Manager) pausedStatus() []DBStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.paused) == 0 {
		return nil
	}
	out := make([]DBStatus, 0, len(m.paused))
	for name := range m.paused {
		// resume re-Tracks before it clears the paused mark — deliberately, so a
		// failed resume still reports the database rather than dropping it out
		// of both maps. That leaves a brief window where a name is in both, and
		// reporting it twice would be worse than either answer alone.
		if _, live := m.dbs[name]; live {
			continue
		}
		out = append(out, DBStatus{Name: name, Paused: true})
	}
	return out
}

// Close stops replication for every tracked database and shuts the agent down.
//
// It asks the agent to shut down cleanly and gives it a bounded grace period to
// acknowledge — the agent's final replica sync per database is what makes the
// backup current as of shutdown, and its error is what Close returns. It then
// closes the pipe regardless, and kills the agent if it has not exited within
// the grace period. Close never returns leaving an agent behind, and never
// waits indefinitely for one: an agent that stops answering still gets stopped.
//
// A shutdown the agent never acknowledged is reported as success, because it
// IS one — the process is gone. Only an error the agent actually sent back is
// returned.
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
	// Paused marks go with it. Nothing will resume them — the manager is done —
	// and unlike a tracked entry, which Status reports WITH an error saying the
	// manager is closed, a paused entry carries no error at all and would read
	// as "paused, resuming shortly" for the rest of the process's life.
	clear(m.paused)
	m.mu.Unlock()

	return m.cl.close(ctx)
}

// relFor maps a logical database name to its path under the instance prefix.
// "control" is special-cased; archived repos already carry their archive/
// namespace in the name, so they become a sibling of repos/ rather than living
// inside it — the archive id is globally unique, so nesting would buy nothing.
//
// Both special cases depend on no REPO ever arriving with such a name, because
// relFor answers on the logical name alone and would then hand a repo the
// control database's replica path. The archive prefix is safe by grammar (repo
// names admit no slash); "control" is safe only because repos.isValidRepoName
// reserves it. Anything added here must be reserved there in the same change.
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
	if err := m.cl.call(context.Background(), proto.MethodDeleteReplica,
		proto.DeleteReplicaParams{Rel: rel}, nil); err != nil {
		return fmt.Errorf("backup.DeleteArchivedReplica %q: %w", archiveID, err)
	}
	log.Info().Str("id", archiveID).Str("rel", rel).Msg("backup: deleted an archived database's replica objects")
	return nil
}
