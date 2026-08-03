package repos

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The registry row's origin is the ONLY record of where a repo came from once
// its database is gone — Manager.Start reads it to decide whether a registered
// repo whose .db is absent can be re-cloned or is simply lost. Every test here
// pins one way that record could silently drift away from the store, which is
// the source of truth.

// TestEnsureActivePreservesProvenance is the guard on Rescan's registry write.
//
// Upsert is a whole-row write — every column comes from `excluded` — so
// re-registering a repo with a partially-filled RepoRecord blanks its origin
// and restamps its creation time. That is right for a repo being created and
// wrong for one being re-adopted, and Rescan does the latter: Manager.Start
// skips and logs a repo whose open failed, leaving the row behind, and a rescan
// is exactly how such a repo comes back.
//
// If this fails, a rescan has just destroyed the field Start's
// rebuild-from-origin branch depends on.
func TestEnsureActivePreservesProvenance(t *testing.T) {
	r := openRegistry(t)
	created := time.Unix(1700000000, 0).UTC()
	require.NoError(t, r.Upsert(RepoRecord{
		Name:         "work",
		OriginURL:    "git@github.com:acme/kb.git",
		OriginBranch: "master",
		State:        RepoActive,
		CreatedAt:    created,
	}))

	// Re-register it, as a rescan would, with a fresh timestamp and no origin.
	require.NoError(t, r.EnsureActive("work", time.Unix(1900000000, 0).UTC()))

	got, ok, err := r.ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "git@github.com:acme/kb.git", got.OriginURL, "origin must survive re-registration")
	require.Equal(t, "master", got.OriginBranch, "upstream branch must survive re-registration")
	require.True(t, got.CreatedAt.Equal(created), "creation time is when the repo was made, not when it was rescanned")
	require.Equal(t, RepoActive, got.State)
}

// TestEnsureActiveInsertsWhenAbsent pins the other half: a name with no row at
// all gets one, stamped with the time it was adopted. Without this the
// preserve-on-conflict arm above would be indistinguishable from a no-op.
func TestEnsureActiveInsertsWhenAbsent(t *testing.T) {
	r := openRegistry(t)
	created := time.Unix(1700000000, 0).UTC()
	require.NoError(t, r.EnsureActive("fresh", created))

	got, ok, err := r.ActiveRecord("fresh")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, RepoActive, got.State)
	require.True(t, got.CreatedAt.Equal(created))
	require.Empty(t, got.OriginURL)
}

// TestEnsureActiveLeavesArchivedRowsAlone: a name can carry one active row and
// any number of archived ones, and re-registering the live repo must not
// resurrect an archive that happens to share its name.
func TestEnsureActiveLeavesArchivedRowsAlone(t *testing.T) {
	r := openRegistry(t)
	require.NoError(t, r.Upsert(RepoRecord{
		Name:      "work",
		State:     RepoArchived,
		ArchiveID: "2abcXYZ",
		OriginURL: "git@github.com:acme/old.git",
	}))
	require.NoError(t, r.EnsureActive("work", time.Unix(1700000000, 0).UTC()))

	arch, ok, err := r.ArchiveRecord("2abcXYZ")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, RepoArchived, arch.State, "the archived row must stay archived")
	require.Equal(t, "git@github.com:acme/old.git", arch.OriginURL)

	active, ok, err := r.ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, active.ArchiveID)
}

// TestRecordOriginWritesStoreOriginThrough is the happy path of the
// write-through: an origin attached to the store after the repo was created
// must reach control.db without waiting for a restart.
//
// Before this existed, only clone-mode Create ever wrote an origin into the
// registry. A repo created empty and connected to a remote afterwards through
// the API carried its origin in the store alone, so a volume loss left it
// registered with nothing to rebuild from.
func TestRecordOriginWritesStoreOriginThrough(t *testing.T) {
	home := t.TempDir()
	m := newTestManager(t, home)
	require.NoError(t, m.Start())

	ri := mustCreateRepo(t, m, testRepoName)
	svc := testService(t, ri)

	// Attach an origin the way the web layer does — straight at the store.
	require.NoError(t, svc.Remote().SetRemote("origin",
		"git@github.com:acme/kb.git", "master", m.deps.AgentBranch, 300, 300, "", ""))

	before, ok, err := m.RepoRegistry().ActiveRecord(testRepoName)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, before.OriginURL, "preset create records no origin; that is the state this fixes")

	m.RecordOrigin(testRepoName)

	got, ok, err := m.RepoRegistry().ActiveRecord(testRepoName)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "git@github.com:acme/kb.git", got.OriginURL)
	require.Equal(t, "master", got.OriginBranch)
	require.True(t, got.CreatedAt.Equal(before.CreatedAt), "a write-through must not restamp provenance")
}

// TestRecordOriginClearsADisconnectedOrigin is the case a "never erase with a
// blank" rule gets wrong, and it is not symmetric with the one above.
//
// Disconnecting an origin is an origin change like any other. A registry that
// kept the URL the user just removed would have the next boot silently re-clone
// this repo from a remote they deliberately detached — restoring data they
// meant to stop tracking, from credentials they may have revoked.
func TestRecordOriginClearsADisconnectedOrigin(t *testing.T) {
	home := t.TempDir()
	m := newTestManager(t, home)
	require.NoError(t, m.Start())

	ri := mustCreateRepo(t, m, testRepoName)
	svc := testService(t, ri)
	require.NoError(t, svc.Remote().SetRemote("origin",
		"git@github.com:acme/kb.git", "main", m.deps.AgentBranch, 300, 300, "", ""))
	m.RecordOrigin(testRepoName)

	rec, _, err := m.RepoRegistry().ActiveRecord(testRepoName)
	require.NoError(t, err)
	require.Equal(t, "git@github.com:acme/kb.git", rec.OriginURL, "precondition")

	require.NoError(t, svc.Remote().DeleteRemote("origin"))
	m.RecordOrigin(testRepoName)

	got, ok, err := m.RepoRegistry().ActiveRecord(testRepoName)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, got.OriginURL, "a disconnect must reach control.db")
	require.Empty(t, got.OriginBranch)
}

// TestRecordOriginLeavesTheRegistryAloneWhenTheStoreIsUnreadable is the other
// side of the same coin, and the reason originOf reports `ok` rather than just
// an empty string.
//
// A store that cannot be read has told us NOTHING. Reading that as "this repo
// has no origin" would erase a good registry row over a transient failure —
// destroying, from a hiccup, the exact field that exists to survive losing the
// store.
func TestRecordOriginLeavesTheRegistryAloneWhenTheStoreIsUnreadable(t *testing.T) {
	home := t.TempDir()
	m := newTestManager(t, home)
	require.NoError(t, m.Start())

	ri := mustCreateRepo(t, m, testRepoName)
	svc := testService(t, ri)
	require.NoError(t, svc.Remote().SetRemote("origin",
		"git@github.com:acme/kb.git", "main", m.deps.AgentBranch, 300, 300, "", ""))
	m.RecordOrigin(testRepoName)

	// Detach the store: Acquire now fails, which is what WithRead sees as a nil
	// service — the same state a repo is in mid-SwapStore.
	old := ri.detachStore(false)
	require.NotNil(t, old)
	old.wg.Wait()
	old.svc.Close()

	m.RecordOrigin(testRepoName)

	got, ok, err := m.RepoRegistry().ActiveRecord(testRepoName)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "git@github.com:acme/kb.git", got.OriginURL,
		"an unreadable store must not be mistaken for a repo with no origin")
	require.Equal(t, "main", got.OriginBranch)
}

// TestRescanKeepsProvenance is the end-to-end version of
// TestEnsureActivePreservesProvenance, driven through the production Rescan
// path rather than the registry method it calls.
//
// The reachable sequence: Manager.Start skips a repo whose open failed and
// leaves its row alone, then a rescan adopts the .db off the disk. Rescan must
// re-register the name without discarding what the row already knows.
//
// The origin is set on the STORE as well as the registry, because that is the
// only state the two can actually be in once RecordOrigin keeps them in step —
// and it makes the assertion sharper rather than weaker. If Rescan's write
// reverted to a whole-row Upsert, the origin would be blanked and then
// re-derived by RecordOrigin, so origin alone could pass a broken
// implementation. created_at could not: nothing re-derives it, so it is the
// field that proves EnsureActive did its job.
func TestRescanKeepsProvenance(t *testing.T) {
	home := t.TempDir()
	m := newTestManager(t, home)
	require.NoError(t, m.Start())

	ri := mustCreateRepo(t, m, testRepoName)
	svc := testService(t, ri)
	require.NoError(t, svc.Remote().SetRemote("origin",
		"git@github.com:acme/kb.git", "master", m.deps.AgentBranch, 300, 300, "", ""))

	created := time.Unix(1700000000, 0).UTC()
	require.NoError(t, m.RepoRegistry().Upsert(RepoRecord{
		Name:         testRepoName,
		OriginURL:    "git@github.com:acme/kb.git",
		OriginBranch: "master",
		State:        RepoActive,
		CreatedAt:    created,
	}))

	// Drop it out of the live map so Rescan treats the on-disk .db as
	// unregistered — the state Start leaves behind when an open fails.
	m.mu.Lock()
	stale := m.repos[testRepoName]
	delete(m.repos, testRepoName)
	m.mu.Unlock()
	stale.shutdown()

	res, err := m.Rescan()
	require.NoError(t, err)
	require.Contains(t, res.Added, testRepoName)

	got, ok, err := m.RepoRegistry().ActiveRecord(testRepoName)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, got.CreatedAt.Equal(created),
		"a rescan re-registers a repo; it does not create one, and nothing else can recover this field")
	require.Equal(t, "git@github.com:acme/kb.git", got.OriginURL,
		"rescan must not blank the origin Start's rebuild path reads")
	require.Equal(t, "master", got.OriginBranch)
}
