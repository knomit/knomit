package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"knomit/internal/backup"
	"knomit/internal/config"
	"knomit/internal/repos"
)

// bootInstance runs the real boot sequence for one container and wires
// replication the way cmd/serve does: Bootstrap first (it rehydrates the volume
// from the replica), then the repo manager, then Track for control.db, every
// open repo, and every archived database still on disk.
//
// It builds repos.Manager directly rather than going through app.New, and the
// two omissions are deliberate.
//
//   - No embedder. app.New requires one and downloads an ONNX model into
//     <home>/models; with a fresh home per container that is tens of seconds of
//     network per test, for a round trip that never embeds anything.
//   - DisableBackgroundSync, so each repo's index heal runs inline. In
//     production it runs in a goroutine, and a heal reading a database through
//     cgo SQLite while litestream's own connection works on the same file can
//     take the process down with SIGBUS under load — reproducible, and reported
//     alongside this work as a defect in its own right. It is not something this
//     test should be asserting about, and a test that crashes the binary one run
//     in five is not a regression guard, so the heal is made synchronous here.
//
// Everything the archive round trip actually depends on is real: the replica,
// Bootstrap's restore, control.db and its registry, and the archive namespace.
func bootInstance(t *testing.T, cfg config.Config) (*repos.Manager, *backup.Manager) {
	t.Helper()
	ctx := context.Background()

	boot, err := Bootstrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if boot.Backup == nil {
		t.Fatal("fixture: backup must be enabled for a round-trip test")
	}

	m := repos.New(ctx, repos.Deps{
		Cfg:                   cfg,
		Signer:                boot.Signer,
		AgentBranch:           boot.AgentBranch,
		KeyPath:               keyPathFor(cfg),
		Backup:                boot.Backup,
		StrictMissing:         cfg.Backup.Enabled,
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		boot.Backup.Close(context.Background())
		t.Fatalf("Start: %v", err)
	}

	if err := boot.Backup.Track("control", filepath.Join(cfg.Home, "control.db")); err != nil {
		t.Fatalf("track control: %v", err)
	}
	for name, dbPath := range m.OpenDBPaths() {
		if err := boot.Backup.Track(name, dbPath); err != nil {
			t.Fatalf("track %s: %v", name, err)
		}
	}
	archived, err := m.ArchivedDBPaths()
	if err != nil {
		t.Fatalf("ArchivedDBPaths: %v", err)
	}
	for id, dbPath := range archived {
		if err := boot.Backup.TrackArchived(id, dbPath); err != nil {
			t.Fatalf("track archived %s: %v", id, err)
		}
	}
	return m, boot.Backup
}

// stopInstance tears a container down so the replica ends up holding everything
// it had. It untracks EVERY database before closing the manager, which is the
// opposite of the order cmd/serve uses.
//
// The reason is what this test needs to assert, not any doubt about the shipped
// order. Untrack closes one litestream database and performs its final sync
// SYNCHRONOUSLY, returning that database's own error — so a failure names the
// database. That is a stronger guarantee than polling Status for "InSync &&
// RemoteTXID > 0", which a stale position also satisfies: a poll can return
// before the row this test depends on (the archived registry row) has been
// uploaded at all.
//
// Production does the reverse — cmd/serve's deferred boot.Backup.Close runs
// after a.Close, so knomit's SQLite handles close first and the agent's final
// sync runs last — and that order is FINE. An earlier version of this comment
// said it was not: that knomit's close checkpoints and removes each -wal while
// litestream still monitors it, so the final sync fails with "invalid wal header
// magic: 0" and retries until ShutdownSyncTimeout. That was true while
// litestream ran inside the knomit process, where POSIX advisory locks do not
// conflict between two descriptors held by the same process, so knomit's close
// could take the exclusive lock the checkpoint-and-delete needs. Moving
// litestream into the knomit-backup child process removed the mechanism: the
// kernel now arbitrates between processes, knomit's close cannot take that lock,
// the -wal survives, and the agent's final sync reads it.
//
// The shipped order is covered by
// TestRecovery_ProductionShutdownOrderFlushesTheLastWrite in test/storytests,
// which tears down in it and recovers the last write from the replica.
func stopInstance(t *testing.T, m *repos.Manager, bm *backup.Manager) {
	t.Helper()
	ctx := context.Background()
	for _, st := range bm.Status(ctx) {
		if err := bm.Untrack(st.Name); err != nil {
			t.Fatalf("final flush of %q: %v", st.Name, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("close repo manager: %v", err)
	}
	if err := bm.Close(ctx); err != nil {
		t.Fatalf("close backup: %v", err)
	}
}

// replaceContainer returns the config the next container comes up with: a brand
// new EMPTY volume, the same replica bucket, the same injected identity.
//
// A fresh directory rather than wiping the old one in place. Deleting a home out
// from under a still-running process is not what a container replacement is, and
// it is actively hazardous — SQLite maps database pages, so removing or
// truncating those files beneath a live mapping raises SIGBUS and takes the test
// binary down. A new directory reproduces the real scenario exactly (empty
// volume, populated bucket) with none of that.
//
// The SSH key is carried across because in production it is a mounted secret,
// not volume state. The agent branch is derived from it, so a fresh key would
// fork the repo's identity and leave the content assertion comparing nothing.
func replaceContainer(t *testing.T, old config.Config) config.Config {
	t.Helper()
	next := old
	next.Home = t.TempDir()
	for _, name := range []string{"id_ed25519", "id_ed25519.pub"} {
		b, err := os.ReadFile(filepath.Join(old.Home, name))
		if err != nil {
			t.Fatalf("read %s from the outgoing container: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(next.Home, name), b, 0o600); err != nil {
			t.Fatalf("inject %s into the new container: %v", name, err)
		}
	}
	return next
}

// TestArchiveRoundTripSurvivesAReplacedContainer is the regression guard the
// design names for the retention trap: archive a repo, lose the volume,
// bootstrap, unarchive, and assert the content is back.
//
// It is end-to-end on purpose. Every piece is invisible in isolation: the
// archived database replicates under a namespace whose retention is disabled (so
// its snapshot is still there), control.db carries the archived registry row
// home (so the archive is still advertised on the new volume), and the unarchive
// pulls the database from the replica because boot restores ACTIVE repos only.
// Break any one and this fails; test them separately and the gap between them is
// exactly where the round trip used to be broken.
//
// Identity, not merely openability, is asserted: a repo's ID is its root commit,
// so a database that came back empty, truncated, or from the wrong prefix cannot
// produce a match.
func TestArchiveRoundTripSurvivesAReplacedContainer(t *testing.T) {
	ctx := context.Background()
	cfg := backupCfg(t)
	injectKey(t, cfg)

	// --- First container: create a repo, archive it, let the archive replicate.
	m, bm := bootInstance(t, cfg)

	ri, err := m.Create(ctx, repos.CreateSpec{
		Name: "cold", Mode: "preset", OntologyPreset: "default",
	}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantID := ri.ID()
	if wantID == "" {
		t.Fatal("fixture: the repo has no resolvable ID, so nothing below would assert content")
	}

	info, err := m.Archive("cold")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// stopInstance flushes synchronously; this only fails fast and loudly if
	// replication of the archived copy never started at all.
	waitSynced(t, bm, backup.ArchiveName(info.ID))
	stopInstance(t, m, bm)

	// --- The container is replaced: empty volume, same bucket.
	next := replaceContainer(t, cfg)

	// --- Second container: everything it knows comes from the replica.
	m2, bm2 := bootInstance(t, next)
	t.Cleanup(func() { stopInstance(t, m2, bm2) })

	if _, serr := os.Stat(filepath.Join(next.Home, "control.db")); serr != nil {
		t.Fatalf("control.db was not restored: %v", serr)
	}
	listed, err := m2.ListArchived()
	if err != nil {
		t.Fatalf("ListArchived: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != info.ID {
		t.Fatalf("archived set after restore = %+v, want just %q", listed, info.ID)
	}
	// The trap: the archive is advertised, but its database is not on this
	// volume. An unarchive that did not reach for the replica stops here.
	archivedPath := filepath.Join(next.Home, "repos", "archive", info.ID+".db")
	if _, serr := os.Stat(archivedPath); !os.IsNotExist(serr) {
		t.Fatalf("fixture is wrong: %s exists, so the lazy fetch is not being exercised", archivedPath)
	}

	restored, err := m2.Restore(info.ID, "")
	if err != nil {
		t.Fatalf("Restore on a replaced container: %v — the archived database was replicated under a "+
			"retention-disabled prefix precisely so this would work", err)
	}
	if got := restored.ID(); got != wantID {
		t.Fatalf("restored repo ID = %q, want %q: the database that came back is not the one archived", got, wantID)
	}
	if m2.Get("cold") == nil {
		t.Fatal("the unarchived repo is not registered")
	}
	if left, lerr := m2.ListArchived(); lerr != nil || len(left) != 0 {
		t.Fatalf("ListArchived after restore = %+v (err %v), want empty", left, lerr)
	}
}

// TestArchivedRepoIsReTrackedAfterARestart covers the other half of the boot
// wiring. An archive that is still on the volume must be replicating again after
// a restart — otherwise it is replicated only for the lifetime of the process
// that archived it, and Purge's untrack becomes a permanent no-op.
func TestArchivedRepoIsReTrackedAfterARestart(t *testing.T) {
	ctx := context.Background()
	cfg := backupCfg(t)
	injectKey(t, cfg)

	m, bm := bootInstance(t, cfg)
	if _, err := m.Create(ctx, repos.CreateSpec{
		Name: "cold", Mode: "preset", OntologyPreset: "default",
	}, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	info, err := m.Archive("cold")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	waitSynced(t, bm, backup.ArchiveName(info.ID))
	stopInstance(t, m, bm)

	// Restart over the SAME volume: the archived database is still there.
	m2, bm2 := bootInstance(t, cfg)
	t.Cleanup(func() { stopInstance(t, m2, bm2) })

	name := backup.ArchiveName(info.ID)
	var tracked bool
	for _, st := range bm2.Status(ctx) {
		if st.Name == name {
			tracked = true
		}
	}
	if !tracked {
		t.Fatalf("%q is not tracked after a restart; the archive stopped being replicated and "+
			"a later purge would have nothing to untrack", name)
	}
}
