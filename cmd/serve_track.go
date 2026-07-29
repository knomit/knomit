package cmd

import (
	"fmt"
	"path/filepath"

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
func trackForReplication(t replicationTracker, m replicationTargets, home string) error {
	if t == nil {
		return nil // replication disabled: nothing opened here is replicated
	}
	if err := t.Track("control", filepath.Join(home, "control.db")); err != nil {
		return fmt.Errorf("track control.db: %w", err)
	}
	for name, dbPath := range m.OpenDBPaths() {
		if err := t.Track(name, dbPath); err != nil {
			return fmt.Errorf("track %s: %w", name, err)
		}
	}
	// Archived databases too, under the retention-disabled archive namespace.
	// Without this an archive is replicated only for the lifetime of the process
	// that created it: after a restart nothing tracks it, so Purge's untrack is
	// a permanent no-op. Archives whose database is NOT on this volume are
	// skipped here and fetched from the replica on unarchive instead.
	archived, err := m.ArchivedDBPaths()
	if err != nil {
		return fmt.Errorf("list archived repos for replication: %w", err)
	}
	for id, dbPath := range archived {
		if err := t.TrackArchived(id, dbPath); err != nil {
			return fmt.Errorf("track archived %s: %w", id, err)
		}
	}
	return nil
}
