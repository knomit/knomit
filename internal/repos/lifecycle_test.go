package repos

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

func newLifecycleManager(t *testing.T) *Manager {
	return newLifecycleManagerWithRoot(t, "")
}

// newLifecycleManagerWithRoot builds a manager whose local-origin gate permits
// origins under originRoot (empty disables local origins). Clone-mode tests that
// fetch from a file:// remote on disk need this set to the remote's parent.
func newLifecycleManagerWithRoot(t *testing.T, originRoot string) *Manager {
	t.Helper()
	dir := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir, LocalOriginRoot: originRoot},
		AgentBranch: "machine/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	// Start opens only what the registry lists, and a fresh home lists nothing.
	// These tests all assume one pre-existing repo to act on, so create it.
	mustCreateRepo(t, m, testRepoName)
	return m
}

func TestCreate_PresetMode_RegistersRepo(t *testing.T) {
	m := newLifecycleManager(t)
	var steps []string
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, func(e Event) { steps = append(steps, e.Step) })
	require.NoError(t, err)
	require.NotNil(t, ri)
	require.Equal(t, "work", ri.Name())
	require.NotNil(t, m.Get("work"))
	require.Contains(t, steps, "done")
}

func TestCreate_RejectsExistingActiveName(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: testRepoName, Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.ErrorIs(t, err, ErrRepoExists)
}

func TestCreate_RejectsInvalidName(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "Bad Name", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.ErrorIs(t, err, ErrInvalidName)
}

func TestCreate_CustomOntology(t *testing.T) {
	m := newLifecycleManager(t)
	y, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "cust", Mode: "custom", OntologyYAML: string(y),
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, ri)
}

func TestActiveRepoWithOrigin_EmptyWhenNone(t *testing.T) {
	m := newLifecycleManager(t)
	require.Equal(t, "", m.ActiveRepoWithOrigin("https://example.com/x.git"))
}

// runGit runs a git command in dir (empty → process cwd), failing the test on
// error. Used to build a real bare remote that clone-mode Create fetches from.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// seedBareRemote builds a bare git repo with one commit on `main` and returns a
// file:// URL pointing at it — a stand-in for a real remote that clone-mode
// Create can fetch from.
func seedBareRemote(t *testing.T, bare string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed"), 0o644))
	runGit(t, work, "add", "seed.txt")
	runGit(t, work, "commit", "-m", "seed")
	runGit(t, work, "push", "origin", "main")
	runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return "file://" + bare
}

// TestCreate_CloneMode_FetchesAndPersistsOrigin exercises the clone path end to
// end against a real (file://) git remote: Create must fetch, register the repo,
// and persist the origin so ActiveRepoWithOrigin can find it by URL.
func TestCreate_CloneMode_FetchesAndPersistsOrigin(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManagerWithRoot(t, root)
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	var steps []string
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "cloned", Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main"},
	}, func(e Event) { steps = append(steps, e.Step) })
	require.NoError(t, err)
	require.NotNil(t, ri)
	require.Equal(t, "cloned", ri.Name())
	require.NotNil(t, m.Get("cloned"))
	require.Contains(t, steps, "clone")
	require.Contains(t, steps, "done")

	// The origin URL was persisted to the store, so origin-uniqueness sees it.
	require.Equal(t, "cloned", m.ActiveRepoWithOrigin(url))
}

// TestCreate_CloneMode_HonorsDisableBackgroundSync pins that a manager built
// with DisableBackgroundSync starts NO reconcile loop, even though clone-mode
// Create calls ActivateSync.
//
// ActivateSync used to launch the loop unconditionally — the flag was honoured
// in startSyncLoops and ignored here — and runReconcileLoop opens with an
// immediate tick that fetches AND pushes. So every harness that created a repo
// from an origin got a live remote conversation running concurrently with its
// assertions while believing background sync was off. The synchronous reconcile
// ActivateSync exists for still runs; only the loop is suppressed.
func TestCreate_CloneMode_HonorsDisableBackgroundSync(t *testing.T) {
	root := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	withRoot := func(d *Deps) { d.Cfg.LocalOriginRoot = root }

	const loopStarted = "reconcile loop started"

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&buf))
	t.Cleanup(func() { log.Logger = orig })

	quiet := newTestManager(t, t.TempDir(), withRoot) // DisableBackgroundSync: true
	require.NoError(t, quiet.Start())
	var steps []string
	_, err := quiet.Create(context.Background(), CreateSpec{
		Name: "cloned", Mode: "clone", Origin: &OriginSpec{URL: url},
	}, func(e Event) { steps = append(steps, e.Step) })
	require.NoError(t, err)
	require.Contains(t, steps, "sync", "ActivateSync must still run its synchronous reconcile")
	require.NoError(t, quiet.Close()) // drains the loops, so any started loop has logged by now
	require.NotContains(t, buf.String(), loopStarted,
		"DisableBackgroundSync must suppress the reconcile loop on the ActivateSync path too")

	// Positive control: the same create WITHOUT the flag does start the loop, so
	// the assertion above is about the gate and not about a log string that
	// silently stopped matching.
	buf.Reset()
	loud := New(context.Background(), Deps{
		Cfg:         config.Config{Home: t.TempDir(), LocalOriginRoot: root},
		AgentBranch: "machine/test",
	})
	t.Cleanup(func() { _ = loud.Close() })
	require.NoError(t, loud.Start())
	_, err = loud.Create(context.Background(), CreateSpec{
		Name: "cloned", Mode: "clone", Origin: &OriginSpec{URL: url},
	}, nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return strings.Contains(buf.String(), loopStarted) },
		5*time.Second, 20*time.Millisecond,
		"without the flag the loop must start — otherwise the negative assertion above proves nothing")
}

// TestCreate_CloneMode_RejectsDuplicateOrigin verifies that, after a successful
// clone, a second clone of the same origin URL is refused with ErrOriginInUse —
// the real-clone counterpart to the preflight check.
func TestCreate_CloneMode_RejectsDuplicateOrigin(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManagerWithRoot(t, root)
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "first", Mode: "clone", Origin: &OriginSpec{URL: url, Branch: "main"},
	}, nil)
	require.NoError(t, err)

	_, err = m.Create(context.Background(), CreateSpec{
		Name: "second", Mode: "clone", Origin: &OriginSpec{URL: url, Branch: "main"},
	}, nil)
	require.ErrorIs(t, err, ErrOriginInUse)
	require.Nil(t, m.Get("second"), "rejected clone must not leave a registered repo")
}

// TestCreate_CloneMode_CancelledContext verifies the ctx boundary check: a
// Create whose context is already cancelled aborts before fetching and leaves
// no registered repo or partial .db behind.
func TestCreate_CloneMode_CancelledContext(t *testing.T) {
	m := newLifecycleManager(t)
	url := seedBareRemote(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel up front

	_, err := m.Create(ctx, CreateSpec{
		Name: "aborted", Mode: "clone", Origin: &OriginSpec{URL: url, Branch: "main"},
	}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, m.Get("aborted"))
	_, statErr := os.Stat(filepath.Join(m.deps.Cfg.Home, "repos", "aborted.db"))
	require.True(t, os.IsNotExist(statErr), "partial .db must be cleaned up")
}

func TestArchive_MovesFileAndUnregisters(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)

	info, err := m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, "work", info.Name)
	require.NotEmpty(t, info.ID)
	require.Nil(t, m.Get("work"), "archived repo must be unregistered")

	archived, err := m.ListArchived()
	require.NoError(t, err)
	require.Len(t, archived, 1)
	require.Equal(t, "work", archived[0].Name)
	require.Equal(t, info.ID, archived[0].ID)
}

func TestArchive_NotFound(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Archive("nope")
	require.ErrorIs(t, err, ErrRepoNotFound)
}

func TestRestore_BringsBackAndUnarchives(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	info, err := m.Archive("work")
	require.NoError(t, err)

	ri, err := m.Restore(info.ID, "")
	require.NoError(t, err)
	require.Equal(t, "work", ri.Name())
	require.NotNil(t, m.Get("work"))

	left, _ := m.ListArchived()
	require.Empty(t, left)
}

func TestRestore_RenameOnCollision(t *testing.T) {
	m := newLifecycleManager(t)
	_, _ = m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	info, _ := m.Archive("work")
	_, _ = m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)

	_, err := m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrRepoExists)

	ri, err := m.Restore(info.ID, "work2")
	require.NoError(t, err)
	require.Equal(t, "work2", ri.Name())
}

// TestArchive_SurvivesNameReuse pins the reason the registry is keyed by
// (name, archive_id) rather than name alone: a repo name is unique only among
// ACTIVE repos. Archiving "work" and then creating a NEW "work" must leave the
// archived one findable and restorable — with a name-only key the new active
// row overwrote the archived one and the archive became unreachable, which was
// harmless while the record lived in repos/archive/<id>.json but is data loss
// now that the registry is the only place it exists.
func TestArchive_SurvivesNameReuse(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	info, err := m.Archive("work")
	require.NoError(t, err)

	// A brand-new repo claims the archived repo's name.
	_, err = m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)

	archived, err := m.ListArchived()
	require.NoError(t, err)
	require.Len(t, archived, 1, "reusing the name must not erase the archive record")
	require.Equal(t, info.ID, archived[0].ID)
	require.Equal(t, "work", archived[0].Name)

	// And it is still restorable under a free name.
	ri, err := m.Restore(info.ID, "work2")
	require.NoError(t, err)
	require.Equal(t, "work2", ri.Name())
	// Restoring must not disturb the live repo that took the old name.
	require.NotNil(t, m.Get("work"))
	left, err := m.ListArchived()
	require.NoError(t, err)
	require.Empty(t, left)
}

// TestArchive_ClonedRepoDoesNotResurrectOnRestart pins that archiving fully
// retires the repo's ACTIVE registry row. Under the composite (name,
// archive_id) key the archived row is a NEW row, so an Archive that only
// inserts leaves the old active row behind — and because a cloned repo's row
// carries an origin_url, the next Start sees "registered, no .db, has an
// origin" and re-clones it. The repo would come back from the dead, live and
// archived at once, with two copies of its database.
func TestArchive_ClonedRepoDoesNotResurrectOnRestart(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	withRoot := func(d *Deps) { d.Cfg.LocalOriginRoot = root }

	first := newTestManager(t, home, withRoot)
	require.NoError(t, first.Start())
	_, err := first.Create(context.Background(), CreateSpec{
		Name: "cloned", Mode: "clone", Origin: &OriginSpec{URL: url, Branch: "main"},
	}, nil)
	require.NoError(t, err)
	info, err := first.Archive("cloned")
	require.NoError(t, err)
	require.NoError(t, first.Close())

	second := newTestManager(t, home, withRoot)
	require.NoError(t, second.Start())
	require.Nil(t, second.Get("cloned"), "archived repo must not be re-cloned at boot")
	require.NoFileExists(t, filepath.Join(home, "repos", "cloned.db"),
		"a resurrected clone would recreate the active db file")

	// It is still exactly one archive, still restorable.
	archived, err := second.ListArchived()
	require.NoError(t, err)
	require.Len(t, archived, 1)
	require.Equal(t, info.ID, archived[0].ID)
}

// TestArchive_ThenStrictRestartBoots pins the same defect from the direction
// that bricks a server: a stale active row for a LOCAL repo has no origin to
// rebuild from, so once StrictMissing is on (which backup wiring does),
// archiving any repo would make the next boot fail with ErrRepoUnrecoverable.
func TestArchive_ThenStrictRestartBoots(t *testing.T) {
	home := t.TempDir()

	first := newTestManager(t, home)
	require.NoError(t, first.Start())
	_, err := first.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)
	_, err = first.Archive("work")
	require.NoError(t, err)
	require.NoError(t, first.Close())

	strict := newTestManager(t, home, func(d *Deps) { d.StrictMissing = true })
	require.NoError(t, strict.Start(),
		"archiving must not leave a row that a strict boot reads as unrecoverable")
	require.Nil(t, strict.Get("work"))
}

// TestRestore_PreservesCreationTime pins that a repo's creation time survives
// the archive/restore round trip. Stamping time.Now() on restore would quietly
// rewrite the repo's provenance, making a five-year-old knowledge base look
// like it was created this morning.
func TestRestore_PreservesCreationTime(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)

	reg := m.RepoRegistry()
	require.NotNil(t, reg)
	before, ok, err := reg.ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, before.CreatedAt.IsZero(), "precondition: Create records a creation time")

	info, err := m.Archive("work")
	require.NoError(t, err)
	// The archived row must carry it too — it is the only copy while archived.
	arch, ok, err := reg.ArchiveRecord(info.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, before.CreatedAt.Equal(arch.CreatedAt),
		"archived row lost the creation time: %v vs %v", before.CreatedAt, arch.CreatedAt)

	_, err = m.Restore(info.ID, "")
	require.NoError(t, err)

	after, ok, err := reg.ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, before.CreatedAt.Equal(after.CreatedAt),
		"restore reset the creation time: %v -> %v", before.CreatedAt, after.CreatedAt)
}

func TestPurge_RemovesArchive(t *testing.T) {
	m := newLifecycleManager(t)
	_, _ = m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	info, _ := m.Archive("work")
	require.NoError(t, m.Purge(info.ID))
	left, _ := m.ListArchived()
	require.Empty(t, left)
	require.ErrorIs(t, m.Purge(info.ID), ErrArchiveNotFound)
}

// TestArchive_LastRepoSucceedsLeavingZero pins the state that used to be
// unreachable: archiving the ONLY remaining repo succeeds and leaves the
// manager with zero repos. There is no default repo to keep one alive, and both
// the default-repo and last-repo guards are gone — a user who created a repo by
// mistake must be able to undo it.
//
// It also pins the registry half of the active-row invariant across that edge:
// an archived repo has NO active row and exactly one archived row.
func TestArchive_LastRepoSucceedsLeavingZero(t *testing.T) {
	m := newLifecycleManager(t)
	require.Equal(t, []string{testRepoName}, m.Names(), "precondition: exactly one repo")

	info, err := m.Archive(testRepoName)
	require.NoError(t, err, "the last remaining repo must be archivable")
	require.Equal(t, testRepoName, info.Name)
	require.Empty(t, m.Names(), "archiving the last repo must leave zero repos")
	require.Nil(t, m.Get(testRepoName))

	reg := m.RepoRegistry()
	require.NotNil(t, reg)
	active, err := reg.List(RepoActive)
	require.NoError(t, err)
	require.Empty(t, active, "an archived repo must leave no active registry row behind")
	archived, err := reg.List(RepoArchived)
	require.NoError(t, err)
	require.Len(t, archived, 1, "the archived row is what makes the moved db findable again")

	// Zero repos is a state you can come back from.
	ri, err := m.Restore(info.ID, "")
	require.NoError(t, err)
	require.Equal(t, testRepoName, ri.Name())
	require.Equal(t, []string{testRepoName}, m.Names())
}

// TestArchive_RepoNamedCoreIsOrdinary pins that "core" carries no privilege.
// It used to be rejected outright by a name comparison, which would have made
// any user-created repo that happened to be called core un-archivable.
func TestArchive_RepoNamedCoreIsOrdinary(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)

	_, err = m.Archive("core")
	require.NoError(t, err, "a repo named \"core\" is archivable like any other")
	require.Equal(t, []string{"work"}, m.Names())
}

// TestRestore_RefusesExistingDestFile guards against the leftover-db case: a
// db file already at the destination path (from a prior failed restore) must
// not be clobbered. Restore should refuse with ErrRepoExists.
func TestRestore_RefusesExistingDestFile(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	info, err := m.Archive("work")
	require.NoError(t, err)

	// Plant a stray db file at the destination path and remove the active
	// registration so the m.Get(target) check passes but the on-disk guard
	// trips.
	dstDB := filepath.Join(m.deps.Cfg.Home, "repos", "work.db")
	require.NoError(t, os.WriteFile(dstDB, []byte("stray"), 0o644))

	_, err = m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrRepoExists)

	// The archive manifest must be untouched (not deleted) so the repo stays
	// recoverable.
	left, _ := m.ListArchived()
	require.Len(t, left, 1)
}

// TestArchive_PersistsOrigin verifies the origin captured at archive time is
// the one persisted in the store's remote record — exercising the WithRead +
// SetRemote round-trip that ActiveRepoWithOrigin and Archive both rely on.
func TestArchive_PersistsOrigin(t *testing.T) {
	m := newLifecycleManager(t)
	ri, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)

	const originURL = "https://example.com/origin.git"
	ri.WithRead(func(svc *store.Service) {
		require.NotNil(t, svc)
		require.NoError(t, svc.Remote().SetRemote(
			"origin", originURL, "main", m.deps.AgentBranch, 300, 300, "", "",
		))
	})

	// ActiveRepoWithOrigin should now find "work" by its origin URL.
	require.Equal(t, "work", m.ActiveRepoWithOrigin(originURL))

	info, err := m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, originURL, info.Origin)
}

// TestRestore_OriginInUse verifies a restore is refused when the archived
// repo's origin matches an active repo's origin. Persists an origin on a live
// active repo, archives a second repo carrying the same origin, then asserts
// Restore returns ErrOriginInUse.
func TestRestore_OriginInUse(t *testing.T) {
	m := newLifecycleManager(t)
	const originURL = "https://example.com/shared.git"

	// Active repo "keeper" holds the origin.
	keeper, err := m.Create(context.Background(), CreateSpec{Name: "keeper", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	keeper.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Remote().SetRemote(
			"origin", originURL, "main", m.deps.AgentBranch, 300, 300, "", "",
		))
	})

	// Second repo "work" also carries the same origin, then gets archived.
	work, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	work.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Remote().SetRemote(
			"origin", originURL, "main", m.deps.AgentBranch, 300, 300, "", "",
		))
	})
	info, err := m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, originURL, info.Origin)

	_, err = m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrOriginInUse)
}

// TestRestore_HonorsInFlightReservation pins the fix for the Restore TOCTOU:
// Restore must take the same name reservation that Create uses, so it refuses
// (rather than racing) when another operation is already bringing the target
// name into the active map. We hold the reservation directly to stand in for an
// in-flight Create on the same name.
func TestRestore_HonorsInFlightReservation(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	info, err := m.Archive("work")
	require.NoError(t, err)

	// Simulate a concurrent Create holding the name reservation.
	release, err := m.reserveNameAndOrigin("work", "")
	require.NoError(t, err)
	defer release()

	_, err = m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrCreateInFlight)

	// The archive must remain intact and recoverable — the refused restore must
	// not have moved the db or dropped the manifest.
	left, _ := m.ListArchived()
	require.Len(t, left, 1)
	require.Equal(t, info.ID, left[0].ID)
}

// TestCreateRestore_ConcurrentSameName_NoDoubleRegister fires a Create and a
// Restore at the same name simultaneously and asserts exactly one wins while the
// other is cleanly refused — never both succeeding and never leaving the name
// unregistered or its db clobbered. Run under -race to catch any unsynchronised
// access in the registration path.
func TestCreateRestore_ConcurrentSameName_NoDoubleRegister(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	info, err := m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, "work", info.Name)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var errCreate, errRestore error

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errCreate = m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errRestore = m.Restore(info.ID, "")
	}()
	close(start)
	wg.Wait()

	// Exactly one of the two operations succeeded.
	require.True(t, (errCreate == nil) != (errRestore == nil),
		"exactly one of Create/Restore must win: errCreate=%v errRestore=%v", errCreate, errRestore)
	// The loser failed with a name-collision error, not some torn-state error.
	loser := errCreate
	if loser == nil {
		loser = errRestore
	}
	require.True(t, isNameCollision(loser), "loser must be a clean name-collision error, got %v", loser)
	// Whichever won, "work" is registered exactly once and usable.
	require.NotNil(t, m.Get("work"))
}

// isNameCollision reports whether err is one of the expected "name already
// being taken" outcomes from a concurrent Create/Restore on the same name.
func isNameCollision(err error) bool {
	return errors.Is(err, ErrCreateInFlight) || errors.Is(err, ErrRepoExists)
}

// TestCreateLens_VsRepoCreate_SameName_NeverBoth is the P2 stress test: a repo
// Create and a lens CreateLens for the SAME name, fired concurrently in a loop,
// must never both persist (invariant M-1). The loser must fail cleanly — the
// name reservation both ops share (m.creating) makes at least one observe the
// other. Run under -race.
func TestCreateLens_VsRepoCreate_SameName_NeverBoth(t *testing.T) {
	for round := 0; round < 50; round++ {
		m := newLifecycleManager(t)
		makeLensRepo(t, m, "writer") // a valid write member for the lens

		const name = "clash"
		start := make(chan struct{})
		var wg sync.WaitGroup
		var errRepo, errLens error

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errRepo = m.Create(context.Background(), CreateSpec{
				Name: name, Mode: "preset", OntologyPreset: "default",
			}, nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, errLens = m.CreateLens(context.Background(), Lens{Name: name, Write: "writer"})
		}()
		close(start)
		wg.Wait()

		repoExists := m.Get(name) != nil
		_, lensExists, gerr := m.Registry().Get(name)
		require.NoError(t, gerr)
		require.False(t, repoExists && lensExists,
			"round %d: repo AND lens both persisted under %q (errRepo=%v errLens=%v)",
			round, name, errRepo, errLens)
	}
}

// TestCreateLens_VsArchive_MembersAlwaysRegistered is the P1 stress test: a lens
// CreateLens naming repo R and an Archive(R), fired concurrently in a loop, must
// never leave a persisted lens pointing at an archived (unregistered) member. If
// the lens persisted, R must still be registered; if Archive won, CreateLens must
// have failed. Run under -race.
func TestCreateLens_VsArchive_MembersAlwaysRegistered(t *testing.T) {
	for round := 0; round < 50; round++ {
		m := newLifecycleManager(t)
		makeLensRepo(t, m, "member")

		start := make(chan struct{})
		var wg sync.WaitGroup
		var errLens, errArchive error

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errLens = m.CreateLens(context.Background(), Lens{Name: "view", Write: "member"})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, errArchive = m.Archive("member")
		}()
		close(start)
		wg.Wait()

		_, lensExists, gerr := m.Registry().Get("view")
		require.NoError(t, gerr)
		if lensExists {
			require.NotNil(t, m.Get("member"),
				"round %d: lens persisted but member was archived (errLens=%v errArchive=%v)",
				round, errLens, errArchive)
			require.Error(t, errArchive, "round %d: Archive must have been blocked by the lens ref", round)
			require.ErrorIs(t, errArchive, ErrRepoInUseByLens)
		} else {
			// Lens did not persist: Archive won (member gone, CreateLens failed) or
			// the lens was rejected. Either way there must be no dangling lens.
			require.Error(t, errLens, "round %d: no lens persisted, so CreateLens must have failed", round)
		}
	}
}

// TestCreate_ConcurrentSameOrigin_OnlyOneWins pins the fix for the origin-race:
// two clones of the SAME origin under DIFFERENT names must not both succeed
// (which would leave two active repos sharing one remote). The name reservation
// can't catch this — the names differ — so origin uniqueness is enforced by the
// origin reservation in reserveNameAndOrigin. Run under -race.
func TestCreate_ConcurrentSameOrigin_OnlyOneWins(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManagerWithRoot(t, root)
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	start := make(chan struct{})
	var wg sync.WaitGroup
	var errA, errB error

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errA = m.Create(context.Background(), CreateSpec{
			Name: "alpha", Mode: "clone", Origin: &OriginSpec{URL: url, Branch: "main"},
		}, nil)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errB = m.Create(context.Background(), CreateSpec{
			Name: "beta", Mode: "clone", Origin: &OriginSpec{URL: url, Branch: "main"},
		}, nil)
	}()
	close(start)
	wg.Wait()

	// Exactly one clone won; the other was refused for origin-in-use.
	require.True(t, (errA == nil) != (errB == nil),
		"exactly one clone of a shared origin must win: errA=%v errB=%v", errA, errB)
	loser := errA
	if loser == nil {
		loser = errB
	}
	require.ErrorIs(t, loser, ErrOriginInUse)

	// And only one active repo carries the origin.
	require.Equal(t, 1, countActiveWithOrigin(m, url),
		"exactly one active repo may carry the shared origin")
}

// countActiveWithOrigin returns how many active repos have origin url.
func countActiveWithOrigin(m *Manager, url string) int {
	n := 0
	m.ForEach(func(_ string, ri *RepoInstance) {
		ri.WithRead(func(svc *store.Service) {
			if svc == nil {
				return
			}
			if rm, err := svc.Remote().GetRemote("origin"); err == nil && rm != nil && rm.URL == url {
				n++
			}
		})
	})
	return n
}

// seedBareRemoteWithFact builds a bare git repo on `main` containing one valid
// kb fact (plus the default ontology), returning a file:// URL. The fact gives a
// clone-mode Create real index work to do, so the background heal is still in
// flight when Create's ActivateSync fires.
func seedBareRemoteWithFact(t *testing.T, bare string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)

	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(work, "domains"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, "domains", "ontology.yaml"), ont, 0o644))

	f := fact.NewFact("kb/test/f.md")
	f.Title = "Seed fact"
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = []string{"ai-governance"}
	f.Entities = []string{"x"}
	f.Type = fact.Observation
	out, err := fact.SerializeFact(f)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(work, "kb", "test"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, "kb", "test", "f.md"), []byte(out), 0o644))

	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "seed fact")
	runGit(t, work, "push", "origin", "main")
	runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return "file://" + bare
}

// TestCreate_CloneMode_ActivateSyncDoesNotKillIndex regresses the runtime
// clone-create bug where the search index stayed pinned at "indexing" forever.
// openOne launches the heavy index heal in the background; Create then calls
// ActivateSync, whose startSync cancels the (shared) sync context and waits on
// the (shared) waitgroup to (re)start the reconcile loop. That cancel killed the
// in-flight heal, which returned WITHOUT marking the index ready/failed —
// leaving IndexStatus stuck at "indexing" (done=0/total=0). A server restart
// masked it only because Start/Rescan open repos without an inline ActivateSync.
// The heal must own a separate lifecycle so ActivateSync no longer disturbs it.
func TestCreate_CloneMode_ActivateSyncDoesNotKillIndex(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir, LocalOriginRoot: root},
		AgentBranch: "machine/test",
		Embedder:    testEmbedder{},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	url := seedBareRemoteWithFact(t, filepath.Join(root, "remote.git"))

	// Real production path: clone -> Add (launches the background index heal) ->
	// ActivateSync. Pre-fix, ActivateSync's syncCancel cancelled the in-flight
	// heal mid-rebuild; the heal then returned WITHOUT marking the index
	// ready/failed, pinning it at "indexing" forever. The heal reliably loses the
	// race because its rebuild does real SQL/embed work while the cancel is
	// instant.
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "cloned", Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main"},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, ri)

	// The cloned repo's index must reach "ready" — not stay pinned at "indexing".
	require.Eventually(t, func() bool {
		s, _, _ := ri.IndexStatus()
		return s == "ready"
	}, 10*time.Second, 50*time.Millisecond,
		"clone-create index must reach 'ready'; ActivateSync must not kill the background heal")
}
