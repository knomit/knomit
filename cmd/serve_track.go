package cmd

import (
	"path/filepath"

	"github.com/rs/zerolog/log"

	"knomit/internal/backup"
)

// replicationTracker is the slice of *backup.Manager the boot-time track loop
// uses. It exists so the loop can be driven by a test double: the real manager
// spawns a child process and talks to an object store, and a wiring test should
// need neither.
type replicationTracker interface {
	Track(name, dbPath string) error
	TrackArchived(archiveID, dbPath string) error
}

// replicationTargets is the slice of *repos.Manager the loop reads. Same
// reason: the set of databases to replicate is the only thing the loop wants
// from the manager, and asking for exactly that is what makes it testable
// without opening any.
type replicationTargets interface {
	OpenDBPaths() map[string]string
	ArchivedDBPaths() (map[string]string, error)
}

// replicationTrackerFor converts the boot result's backup manager into the
// interface the loop takes, mapping "no replication" to a genuinely nil
// interface.
//
// It is a separate function because a typed nil POINTER stored in an interface
// makes that interface non-nil — the same trap app.New documents where it wires
// repos.BackupTracker. Doing the conversion here keeps the check out of serve.go,
// so the call site below has no condition of its own to short-circuit.
func replicationTrackerFor(bm *backup.Manager) replicationTracker {
	if bm == nil {
		return nil
	}
	return bm
}

// trackForReplication registers every database this server just opened with the
// replica: control.db, every live repo, and every archived database still on the
// volume. A nil tracker means replication is off, which is not an error.
//
// This is the shipped server's only replication wiring, and until it was
// extracted nothing tested it at all: a reviewer disabled it outright and the
// whole suite — cmd, app, and the end-to-end recovery story test — still passed,
// because every other test either mirrors this loop rather than calling it or
// never reaches it.
//
// It must run AFTER repos.Manager.Start, which is what reconciles the registry
// against the disk. Before that there is no truthful answer to "which databases
// are live", and replicating the wrong answer is worse than replicating none.
//
// # Nothing here fails the boot
//
// It returns no error, and that is the point rather than an omission. By the
// time this runs the server has opened every database successfully and is ready
// to serve; the only thing still outstanding is whether a CACHE gets written
// for the benefit of the NEXT boot. A tracked database that fails to register
// costs future boot time, never data — git is the source of truth and every
// database here is rebuildable from it — so aborting a working server over an
// object-store hiccup or an agent that is a few seconds late is exactly the
// "cache miss becomes an outage" this feature must never cause.
//
// The failures are loud instead, and they do not stop at the log: a database
// knomit believes it is replicating that the agent has never heard of is
// precisely the case backup.Manager.Status reconciles and reports with an
// error, so it also reaches /runtime/status and the knomit_backup_error series.
//
// replicateControl is false when this boot could not read control.db back from
// the replica. Its registry is the ONE piece of state git cannot rebuild, so a
// boot that failed to read it must not write its empty replacement over it.
// See app.BootResult.ReplicateControl.
func trackForReplication(t replicationTracker, m replicationTargets, home string, replicateControl bool) {
	if t == nil {
		return // replication disabled: nothing opened here is replicated
	}
	if replicateControl {
		if err := t.Track("control", filepath.Join(home, "control.db")); err != nil {
			log.Error().Err(err).Msg("backup: control.db could not be registered for replication; " +
				"the server is running and NOT backing up its repo registry")
		}
	} else {
		log.Warn().Msg("backup: control.db is deliberately NOT being replicated this boot because its " +
			"restore failed; the registry already in the replica is being preserved rather than overwritten")
	}
	for name, dbPath := range m.OpenDBPaths() {
		if err := t.Track(name, dbPath); err != nil {
			log.Error().Err(err).Str("repo", name).Str("db", dbPath).
				Msg("backup: repo could not be registered for replication; the server is running and " +
					"this repo is NOT being backed up until it is restarted")
		}
	}
	// Archived databases too, under the retention-disabled archive namespace.
	// Without this an archive is replicated only for the lifetime of the process
	// that created it: after a restart nothing tracks it, so Purge's untrack is
	// a permanent no-op. Archives whose database is NOT on this volume are
	// skipped here and fetched from the replica on unarchive instead.
	archived, err := m.ArchivedDBPaths()
	if err != nil {
		log.Error().Err(err).Msg("backup: could not list archived repos; none of them are being replicated " +
			"by this process, so a purge will not reclaim their replica objects until it is restarted")
		return
	}
	for id, dbPath := range archived {
		if err := t.TrackArchived(id, dbPath); err != nil {
			log.Error().Err(err).Str("id", id).Str("db", dbPath).
				Msg("backup: archived database could not be registered for replication")
		}
	}
}
