package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchivedDBReplicatesUnderArchivePrefix(t *testing.T) {
	m, home := newTestManager(t)
	archivePath := filepath.Join(home, "repos", "archive", "arc-1.db")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDBWithValue(t, archivePath, "archived-content")

	if err := m.TrackArchived("arc-1", archivePath); err != nil {
		t.Fatalf("TrackArchived: %v", err)
	}
	waitInSync(t, m, "archive/arc-1")

	// Wipe locally, then restore straight from the archive prefix.
	if err := m.Untrack("archive/arc-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archivePath); err != nil {
		t.Fatal(err)
	}
	restored, err := m.restoreIfAbsent(context.Background(), "archive/arc-1.db", archivePath)
	if err != nil {
		t.Fatalf("restore archived: %v", err)
	}
	if !restored {
		t.Fatal("archived DB was not restored")
	}
	assertDBValue(t, archivePath, "archived-content")
}

// TestArchiveHandoverLetsAReclaimedNameReplicate is the end-to-end proof of the
// archive ordering, run against real litestream rather than a double — because
// the hazard is at the INODE level and no path-comparing double can show it.
//
// The sequence is exactly repos.Archive's: untrack the live name, move the file,
// track it under the archive id. A NEW database then takes the freed path — the
// SAME path string, a different inode. litestream.DB pins its descriptor with a
// single os.Open at init, so had the live entry stayed tracked, the reclaiming
// database would replicate nothing at all: the pinned descriptor still refers to
// the moved inode, the name is "already tracked" so Track is a no-op, and Status
// keeps reporting in sync. The assertion is that BOTH prefixes decode to their
// own content.
func TestArchiveHandoverLetsAReclaimedNameReplicate(t *testing.T) {
	m, home := newTestManager(t)
	ctx := context.Background()

	livePath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(livePath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDBWithValue(t, livePath, "first-tenant")
	if err := m.Track("core", livePath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	first := waitInSync(t, m, "core")

	// Archive, in the order repos.Archive uses.
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	archivePath := filepath.Join(home, "repos", "archive", "arc-4.db")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(livePath, archivePath); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Rename(livePath+suffix, archivePath+suffix)
	}
	if err := m.TrackArchived("arc-4", archivePath); err != nil {
		t.Fatalf("TrackArchived: %v", err)
	}
	waitInSync(t, m, "archive/arc-4")

	// A new repo claims the freed name — same path, new inode.
	makeDBWithValue(t, livePath, "second-tenant")
	if err := m.Track("core", livePath); err != nil {
		t.Fatalf("Track(reclaimed): %v", err)
	}
	// Strictly past the first tenant's position: proves the NEW inode's content
	// actually reached the live prefix, not merely that the name looks in sync.
	waitReplicatedPast(t, m, "core", first)

	if err := m.Untrack("core"); err != nil {
		t.Fatal(err)
	}
	if err := m.Untrack("archive/arc-4"); err != nil {
		t.Fatal(err)
	}

	liveOut := filepath.Join(t.TempDir(), "live-restored.db")
	if _, err := m.restoreIfAbsent(ctx, m.relFor("core"), liveOut); err != nil {
		t.Fatalf("restore live prefix: %v", err)
	}
	assertDBValue(t, liveOut, "second-tenant")

	archiveOut := filepath.Join(t.TempDir(), "archive-restored.db")
	if _, err := m.restoreIfAbsent(ctx, m.relFor("archive/arc-4"), archiveOut); err != nil {
		t.Fatalf("restore archive prefix: %v", err)
	}
	assertDBValue(t, archiveOut, "first-tenant")
}

// TestDeleteArchivedReplicaRemovesOnlyThatArchive is the blast-radius test for
// the one place knomit ever deletes replica data.
//
// It runs against the real file backend rather than a double, because what needs
// pinning is the PREFIX SCOPING — the property standing between "purge one
// archive" and "destroy every backup this instance has". A double that records
// the archive id it was handed cannot observe that at all.
//
// "Gone" and "survives" are both asserted by RESTORING: an archive is gone when
// the replica no longer holds a backup for it, and a neighbour survives when it
// restores to its exact original content. The doomed prefix is also checked
// directly on disk, so a pass means the objects are actually deleted and not
// merely an unreadable chain.
func TestDeleteArchivedReplicaRemovesOnlyThatArchive(t *testing.T) {
	m, home := newTestManager(t)
	ctx := context.Background()

	livePath := filepath.Join(home, "repos", "live.db")
	doomedPath := filepath.Join(home, "repos", "archive", "doomed.db")
	keptPath := filepath.Join(home, "repos", "archive", "kept.db")
	for path, content := range map[string]string{
		livePath:   "live-content",
		doomedPath: "doomed-content",
		keptPath:   "kept-content",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		makeDBWithValue(t, path, content)
	}

	if err := m.Track("live", livePath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := m.TrackArchived("doomed", doomedPath); err != nil {
		t.Fatalf("TrackArchived(doomed): %v", err)
	}
	if err := m.TrackArchived("kept", keptPath); err != nil {
		t.Fatalf("TrackArchived(kept): %v", err)
	}
	waitInSync(t, m, "live")
	waitInSync(t, m, ArchiveName("doomed"))
	waitInSync(t, m, ArchiveName("kept"))

	// Untrack before deleting, as Purge does: removing objects from under a live
	// replica only invites it to upload them again.
	if err := m.UntrackArchived("doomed"); err != nil {
		t.Fatalf("UntrackArchived: %v", err)
	}
	if err := m.DeleteArchivedReplica("doomed"); err != nil {
		t.Fatalf("DeleteArchivedReplica: %v", err)
	}

	// (a) The doomed archive is gone — as objects on the replica...
	replicaRoot := strings.TrimPrefix(m.cfg.URL, "file://")
	doomedPrefix := filepath.Join(replicaRoot, m.cfg.Instance, "archive", "doomed.db")
	if _, err := os.Stat(doomedPrefix); !os.IsNotExist(err) {
		t.Errorf("the purged archive's objects are still at %s (stat err = %v)", doomedPrefix, err)
	}
	// ...and as anything restorable.
	_, err := m.restoreIfAbsent(ctx, m.relFor(ArchiveName("doomed")), filepath.Join(t.TempDir(), "doomed.db"))
	if !isNoSnapshot(err) {
		t.Errorf("the purged archive still restores (err = %v); its objects outlived the purge", err)
	}

	// (b) The SIBLING archive is untouched — the assertion that fails if the
	// delete is ever widened to the archive namespace rather than one id.
	assertStillRestores(t, m, ArchiveName("kept"), "kept-content",
		"purging one archive destroyed a SIBLING archive")

	// (c) The LIVE repo is untouched — the assertion that fails if the delete is
	// ever widened to the instance root.
	//
	// Reported independently of (b) rather than after a Fatalf, so an over-broad
	// prefix names everything it took rather than only the first casualty.
	assertStillRestores(t, m, "live", "live-content",
		"purging an archive destroyed a LIVE repo's backup")
}

// assertStillRestores requires that name's replica still restores to want.
// It reports rather than aborting, so a caller can check several neighbours in
// one run and see the full blast radius of a bad prefix.
func assertStillRestores(t *testing.T, m *Manager, name, want, msg string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "restored.db")
	restored, err := m.restoreIfAbsent(context.Background(), m.relFor(name), out)
	if err != nil || !restored {
		t.Errorf("%s: %q no longer restores (restored=%v err=%v)", msg, name, restored, err)
		return
	}
	assertDBValue(t, out, want)
}

// TestTrackRefusesADifferentPathForATrackedName closes the silent half of the
// archive bug — the half a path comparison CAN see.
//
// litestream.DB pins a file descriptor via os.Open at init and never reopens it,
// so a Track swallowed as "already tracked" leaves the caller's file replicated
// by nobody, with no error anywhere. Note what this check does and does not
// cover: a reclaimed repo name resolves to the same path string, so only
// Archive's untrack can close that case (see the test above). This one catches
// the rest — a name re-pointed at a genuinely different file, which is always a
// caller bug rather than a no-op.
func TestTrackRefusesADifferentPathForATrackedName(t *testing.T) {
	m, home := newTestManager(t)

	first := filepath.Join(home, "first.db")
	second := filepath.Join(home, "second.db")
	makeDBWithValue(t, first, "first")
	makeDBWithValue(t, second, "second")

	if err := m.Track("core", first); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := m.Track("core", first); err != nil {
		t.Fatalf("re-Track of the same path must stay a no-op: %v", err)
	}
	err := m.Track("core", second)
	if err == nil {
		t.Fatal("Track silently ignored a different path for an already-tracked name; " +
			"the new database would replicate nothing, with no error anywhere")
	}
	if got := m.Status(context.Background()); len(got) != 1 || got[0].Name != "core" {
		t.Fatalf("the rejected Track disturbed the tracked set: %+v", got)
	}
}
