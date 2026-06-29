package repos

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
		Name: config.DefaultRepoName, Mode: "preset", OntologyPreset: "default",
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

func TestArchive_BlocksDefaultRepo(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Archive(config.DefaultRepoName)
	require.ErrorIs(t, err, ErrCannotArchiveDefault)
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

func TestPurge_RemovesArchive(t *testing.T) {
	m := newLifecycleManager(t)
	_, _ = m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	info, _ := m.Archive("work")
	require.NoError(t, m.Purge(info.ID))
	left, _ := m.ListArchived()
	require.Empty(t, left)
	require.ErrorIs(t, m.Purge(info.ID), ErrArchiveNotFound)
}

// TestArchive_DefaultStillBlocked documents that with only core present,
// Archive(core) returns ErrCannotArchiveDefault — the default-repo check
// fires before the last-repo guard, so ErrCannotArchiveLast is never reached
// via the default path. This is the realistic reachable behavior.
func TestArchive_DefaultStillBlocked(t *testing.T) {
	m := newLifecycleManager(t)
	require.Len(t, m.Names(), 1) // only core
	_, err := m.Archive(config.DefaultRepoName)
	require.ErrorIs(t, err, ErrCannotArchiveDefault)
}

// TestArchive_LastNonDefault_StillSucceeds documents that archiving the last
// NON-default repo succeeds, leaving only core. ErrCannotArchiveLast does not
// fire here because len(m.repos)==2 (core + work) at the time of the check.
func TestArchive_LastNonDefault_StillSucceeds(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	require.Len(t, m.Names(), 2)

	_, err = m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, []string{config.DefaultRepoName}, m.Names())
}

// TestArchive_BlocksLastActiveRepo constructs the ErrCannotArchiveLast
// condition directly. In normal operation it is unreachable because core is
// always present and is rejected by the default-repo guard first (see
// TestArchive_DefaultStillBlocked). To exercise the defensive guard we use
// in-package access to make the map contain exactly one non-default repo, then
// confirm Archive refuses to remove it.
func TestArchive_BlocksLastActiveRepo(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)

	// Drop the default repo from the map so "work" is the only (non-default)
	// repo. This is in-package access; it is the only way to reach
	// len(m.repos)<=1 with a non-default name, since the default-repo check
	// would otherwise fire first. Capture the default instance so we can
	// re-register it for clean teardown.
	m.mu.Lock()
	core := m.repos[config.DefaultRepoName]
	delete(m.repos, config.DefaultRepoName)
	m.mu.Unlock()
	require.Equal(t, []string{"work"}, m.Names())

	_, err = m.Archive("work")
	require.ErrorIs(t, err, ErrCannotArchiveLast)
	// Guard must NOT have removed the repo from the map.
	require.NotNil(t, m.Get("work"))

	// Re-register the default repo so the manager's Close cleanup tears it down too.
	m.Set(config.DefaultRepoName, core)
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
