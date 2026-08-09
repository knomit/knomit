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

// A created repo gets a registry row and an opaque uid-named file. The NAME is
// registry metadata and never appears in a path.
func TestCreate_WritesRegistryRowAndUIDFile(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")

	uid := ri.UID()
	require.NotEmpty(t, uid)

	rec, ok, err := m.reg.Get(uid)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "core", rec.Name)
	require.Equal(t, StateActive, rec.State)
	require.NotEmpty(t, rec.RepoID, "identity recorded at create")

	require.FileExists(t, m.RepoPath(uid))
	require.NoFileExists(t, filepath.Join(m.deps.Cfg.Home, "repos", "core.db"))
}

// A failed create leaves nothing behind — no row, no file.
func TestCreate_RollsBackRowOnFailure(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "core", Mode: "custom", OntologyYAML: "this is not: valid: yaml:",
	}, nil)
	require.Error(t, err)

	active, lerr := m.reg.List(StateActive)
	require.NoError(t, lerr)
	require.Empty(t, active)

	dbFiles, gerr := filepath.Glob(filepath.Join(m.deps.Cfg.Home, "repos", "*.db"))
	require.NoError(t, gerr)
	require.Empty(t, dbFiles, "a failed create must leave no partial .db behind")
}

// Two creates cannot share a name. Drops the live map entry first so the
// second Create can only be stopped by the durable registry claim (the INSERT
// at lifecycle.go) rather than the live-map check in Create that runs before
// it — otherwise this duplicates TestCreate_RejectsExistingActiveName without
// ever reaching the code this test means to pin.
func TestCreate_DuplicateNameRejected(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	createRepo(t, m, "core")
	m.Remove("core")

	_, err := m.Create(context.Background(), CreateSpec{Name: "core", Mode: "preset"}, nil)
	require.ErrorIs(t, err, ErrRepoExists)
}

func TestCreate_RejectsExistingActiveName(t *testing.T) {
	m := newLifecycleManager(t)
	createRepo(t, m, testRepoName)
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

// TestCreate_CloneMode_RejectsOntologySpec pins that a clone request naming an
// ontology is refused rather than half-honoured. A clone takes its ontology from
// the origin — InitFromRemote overwrites the seed files whenever the remote has
// branches — so a preset would apply only for an EMPTY origin and be silently
// dropped otherwise. Both the preflight (HTTP 400) and Create itself (the
// authoritative guard) must refuse.
func TestCreate_CloneMode_RejectsOntologySpec(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManagerWithRoot(t, root)
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	for _, tc := range []struct {
		name string
		spec CreateSpec
	}{
		{"preset", CreateSpec{Name: "withpreset", Mode: "clone", OntologyPreset: "engineering",
			Origin: &OriginSpec{URL: url, Branch: "main"}}},
		{"yaml", CreateSpec{Name: "withyaml", Mode: "clone", OntologyYAML: "topics: [a]",
			Origin: &OriginSpec{URL: url, Branch: "main"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, m.CreatePreflight(tc.spec), ErrInvalidName)
			_, err := m.Create(context.Background(), tc.spec, nil)
			require.ErrorIs(t, err, ErrInvalidName)
			require.Nil(t, m.Get(tc.spec.Name), "rejected clone must not leave a registered repo")
		})
	}

	// The same spec without the ontology fields still clones.
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "plain", Mode: "clone", Origin: &OriginSpec{URL: url, Branch: "main"},
	}, nil)
	require.NoError(t, err)
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

	// No registry row and no .db file survive a cancelled create — the uid is
	// minted fresh per attempt, so there is nothing to name-match against; check
	// the registry directly and that repos/ holds no stray .db at all.
	active, lerr := m.reg.List(StateActive)
	require.NoError(t, lerr)
	require.Empty(t, active, "cancelled create must leave no registry row")
	dbFiles, gerr := filepath.Glob(filepath.Join(m.deps.Cfg.Home, "repos", "*.db"))
	require.NoError(t, gerr)
	require.Empty(t, dbFiles, "cancelled create must leave no partial .db behind")
}

// Archiving flips a state and shuts the instance down. The file never moves.
func TestArchive_IsAStateFlip(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	path := m.RepoPath(uid)

	info, err := m.Archive("core")
	require.NoError(t, err)
	require.Equal(t, uid, info.ID)
	require.Equal(t, "core", info.Name)

	require.Nil(t, m.Get("core"))
	require.FileExists(t, path, "the database never moves")

	rec, ok, gerr := m.reg.Get(uid)
	require.NoError(t, gerr)
	require.True(t, ok)
	require.Equal(t, StateArchived, rec.State)
	require.NotZero(t, rec.ArchivedAt)
}

// The archive response and the listing that follows it must describe the same
// event with the same timestamp.
//
// The registry stores whole seconds (SetState takes a Unix second) and
// ListArchived renders back from that, so formatting an untruncated time.Now()
// in the response made GET /archived contradict the 200 the client had just
// been handed — same record, two archivedAt values, differing in the fractional
// second. Anything keyed or diffed on archivedAt sees two events.
func TestArchive_ArchivedAtMatchesTheListing(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)

	info, err := m.Archive("work")
	require.NoError(t, err)
	require.NotEmpty(t, info.ArchivedAt)

	list, err := m.ListArchived()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, info.ID, list[0].ID)
	require.Equal(t, info.ArchivedAt, list[0].ArchivedAt,
		"the archive response and the listing must agree on when this repo was archived")
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

// The archived name is free immediately, and restoring under a new name is
// just a rename.
func TestRestore_UnderNewName(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	_, err := m.Archive("core")
	require.NoError(t, err)

	createRepo(t, m, "core") // the name was released

	restored, err := m.Restore(uid, "core-old")
	require.NoError(t, err)
	require.NotNil(t, restored)
	require.Equal(t, uid, restored.UID())
	require.NotNil(t, m.Get("core-old"))
	require.NotNil(t, m.Get("core"))
}

// Restoring into a taken name is refused by the partial unique index.
func TestRestore_NameCollisionRejected(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	_, err := m.Archive("core")
	require.NoError(t, err)
	createRepo(t, m, "core")

	_, err = m.Restore(uid, "core")
	require.ErrorIs(t, err, ErrRepoExists)

	rec, _, gerr := m.reg.Get(uid)
	require.NoError(t, gerr)
	require.Equal(t, StateArchived, rec.State, "a failed restore leaves it archived")
}

// Purge destroys the row, the file, and the stored credential with them.
func TestPurge_RemovesRowFileAndCredential(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	require.NoError(t, m.origins.Set(uid, Origin{URL: "https://x.test/kb.git", Branch: "main"}))
	_, err := m.Archive("core")
	require.NoError(t, err)

	require.NoError(t, m.Purge(uid))

	_, ok, gerr := m.reg.Get(uid)
	require.NoError(t, gerr)
	require.False(t, ok)
	require.NoFileExists(t, m.RepoPath(uid))

	org, oerr := m.origins.Get(uid)
	require.NoError(t, oerr)
	require.Nil(t, org, "the credential dies with the repo")
}

// Archiving a repo must also drop any unavailable entry it still carries.
//
// clearUnavailable used to have exactly two callers — a successful open and
// Purge — so a uid that was flagged unavailable at boot and later came back
// stayed flagged for the whole process lifetime. That was invisible while
// unavailable repos were invisible; now that GET /repos lists them, the stale
// entry would resurrect an archived repo as a row the user cannot act on.
func TestArchive_ClearsUnavailableEntry(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()

	// Exactly the state openRegistered leaves behind for a boot-time failure.
	// The instance is live now (whatever broke the open has since cleared), so
	// this uid is both registered-live and flagged unavailable.
	rec, ok, err := m.reg.Get(uid)
	require.NoError(t, err)
	require.True(t, ok)
	m.markUnavailable(rec, "missing", "database file not found")
	require.Len(t, m.Unavailable(), 1, "sanity: the fixture is set up")

	_, err = m.Archive("core")
	require.NoError(t, err)

	require.Empty(t, m.Unavailable(),
		"archive must drop the unavailable entry, not leave a ghost row in the listing")
}

// ListArchived's order must be total, not merely "newest first".
//
// archived_at has ONE-SECOND resolution (it is stored as a Unix second and read
// back with time.Unix(rec.ArchivedAt, 0)), so two repos archived in the same
// second compare equal on the primary key. Without a tiebreak their relative
// order is whatever the sort happens to do, and sort.Slice is not stable — two
// calls over one registry are free to disagree.
//
// The uids here are chosen to REVERSE the name order Registry.List returns, so
// this fails both for a missing tiebreak and for a tiebreak that is merely
// "whatever order the rows arrived in".
func TestListArchived_TotalOrderWithinOneSecond(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())

	const sameSecond = int64(1_700_000_000)
	seed := []struct{ uid, name string }{
		{"uid-c", "alpha"},
		{"uid-b", "beta"},
		{"uid-a", "gamma"},
	}
	for i, s := range seed {
		require.NoError(t, m.Repos().Insert(RepoRecord{
			UID: s.uid, Name: s.name, State: StateArchived, Profile: ProfileCode,
			CreatedAt: int64(i + 1), ArchivedAt: sameSecond,
		}))
	}
	// One row a second later, to pin that the tiebreak did not displace the
	// primary "newest first" key.
	require.NoError(t, m.Repos().Insert(RepoRecord{
		UID: "uid-z", Name: "delta", State: StateArchived, Profile: ProfileCode,
		CreatedAt: 4, ArchivedAt: sameSecond + 1,
	}))

	var first []string
	for range 5 {
		list, err := m.ListArchived()
		require.NoError(t, err)
		got := make([]string, 0, len(list))
		for _, a := range list {
			got = append(got, a.ID)
		}
		require.Equal(t, []string{"uid-z", "uid-a", "uid-b", "uid-c"}, got,
			"newest first, then uid ascending within one second")
		if first == nil {
			first = got
		}
		require.Equal(t, first, got, "repeated calls must agree")
	}
}

// Purging an ACTIVE repo is refused — archive it first.
func TestPurge_RefusesActiveRepo(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	require.Error(t, m.Purge(ri.UID()))
}

// TestArchiveRestoreArchive_RepeatsClean is the required regression test:
// Archive -> Restore -> Archive again on the same repo. This is the exact
// sequence that exposed the empty-uid bug in Task 6 (Restore used to register
// with uid == "", so Archive's SetState was silently skipped the second time
// around). It proves a restored repo is indistinguishable from a repo that was
// never archived: the second archive must succeed, the file must still be
// exactly where the registry says it is, and the row must end up archived.
func TestArchiveRestoreArchive_RepeatsClean(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	path := m.RepoPath(uid)
	require.Equal(t, uid, ri.UID(), "sanity: uid is stable")

	_, err := m.Archive("core")
	require.NoError(t, err)

	restored, err := m.Restore(uid, "")
	require.NoError(t, err)
	require.Equal(t, uid, restored.UID())

	info, err := m.Archive("core")
	require.NoError(t, err, "the second archive must succeed")
	require.Equal(t, uid, info.ID)
	require.Equal(t, "core", info.Name)

	require.Nil(t, m.Get("core"))
	require.FileExists(t, path, "the file is still where the registry says it is")

	rec, ok, gerr := m.reg.Get(uid)
	require.NoError(t, gerr)
	require.True(t, ok)
	require.Equal(t, StateArchived, rec.State)
	require.NotZero(t, rec.ArchivedAt)
}

// TestArchive_LastRepoLeavesZero pins that archiving the ONLY repo succeeds and
// leaves the manager empty. No repo is privileged and zero repos is a valid
// state — it is how knomit starts — so there is nothing here to protect. The
// archive is restorable, so emptying the manager loses no data.
func TestArchive_LastRepoLeavesZero(t *testing.T) {
	m := newLifecycleManager(t)
	createRepo(t, m, "work")
	require.Equal(t, []string{"work"}, m.Names())

	info, err := m.Archive("work")
	require.NoError(t, err)
	require.Empty(t, m.Names(), "archiving the last repo must leave zero repos")

	// The data is still there — the last repo is archived, not destroyed.
	archived, err := m.ListArchived()
	require.NoError(t, err)
	require.Len(t, archived, 1)
	require.Equal(t, info.ID, archived[0].ID)

	// And it can come back, so zero is a state you can leave as well as reach.
	ri, err := m.Restore(info.ID, "")
	require.NoError(t, err)
	require.Equal(t, "work", ri.Name())
	require.Equal(t, []string{"work"}, m.Names())
}

// TestArchive_EveryRepoInTurn pins that repos can be emptied out one by one,
// with no name treated specially along the way.
func TestArchive_EveryRepoInTurn(t *testing.T) {
	m := newLifecycleManager(t)
	createRepo(t, m, testRepoName)
	createRepo(t, m, "work")
	require.Len(t, m.Names(), 2)

	_, err := m.Archive("work")
	require.NoError(t, err)
	require.Equal(t, []string{testRepoName}, m.Names())

	_, err = m.Archive(testRepoName)
	require.NoError(t, err, "the repo that used to be the default is archivable like any other")
	require.Empty(t, m.Names())
}

// TestArchive_PersistsOrigin verifies the origin captured at archive time is
// the one recorded in control.db's repo_origins — Archive.Origin now comes
// from Origins.Get(uid), not the store's own remote row (Task 8), so only the
// Origins.Set below is what info.Origin reflects.
func TestArchive_PersistsOrigin(t *testing.T) {
	m := newLifecycleManager(t)
	ri, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)

	const originURL = "https://example.com/origin.git"
	require.NoError(t, m.origins.Set(ri.UID(), Origin{URL: originURL, Branch: "main"}))

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

	// Active repo "keeper" holds the origin. reserveNameAndOrigin's uniqueness
	// scan goes through ActiveRepoWithOrigin, a control.db query, so the origin
	// must be recorded via Origins.Set to be visible to it.
	keeper, err := m.Create(context.Background(), CreateSpec{Name: "keeper", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	require.NoError(t, m.origins.Set(keeper.UID(), Origin{URL: originURL, Branch: "main"}))

	// Second repo "work" also carries the same origin, then gets archived.
	// Archive's ArchiveInfo.Origin now comes from Origins.Get(uid) (Task 8), so
	// this also needs Origins.Set, not just the store-side remote row.
	work, err := m.Create(context.Background(), CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
	require.NoError(t, m.origins.Set(work.UID(), Origin{URL: originURL, Branch: "main"}))
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
		writer := makeLensRepo(t, m, "writer") // a valid write member for the lens

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
			_, errLens = m.CreateLens(context.Background(), Lens{Name: name, WriteUID: writer.UID()})
		}()
		close(start)
		wg.Wait()

		repoExists := m.Get(name) != nil
		_, lensExists, gerr := m.LensRegistry().Get(name)
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
		member := makeLensRepo(t, m, "member")

		start := make(chan struct{})
		var wg sync.WaitGroup
		var errLens, errArchive error

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errLens = m.CreateLens(context.Background(), Lens{Name: "view", WriteUID: member.UID()})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, errArchive = m.Archive("member")
		}()
		close(start)
		wg.Wait()

		_, lensExists, gerr := m.LensRegistry().Get("view")
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
// masked it only because Start opens repos without an inline ActivateSync.
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

// Every lifecycle operation reads the control.db tenants that Start assigns and
// Close nils, both under m.mu. Reading them as bare fields made an in-flight
// request racing shutdown two things at once: an unsynchronised read (-race
// flags it on its own) and, once Close had won, a nil dereference — a panic in
// the HTTP handler goroutine rather than a 5xx.
//
// The nil half is deterministic and pinned here; the race half is what -race
// checks over the rest of this package.
func TestLifecycleOpsRefuseAfterClose(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	require.NoError(t, m.Close())

	// Each of these used to dereference a nil *Registry or *Origins.
	_, err := m.Create(context.Background(), CreateSpec{Name: "later", Mode: "preset"}, nil)
	require.ErrorIs(t, err, ErrManagerStopped)

	_, err = m.Archive("core")
	// Archive fails at the map lookup first — Close detached the instances — but
	// the point is that it RETURNS rather than panicking.
	require.Error(t, err)

	_, err = m.ListArchived()
	require.ErrorIs(t, err, ErrManagerStopped)

	_, err = m.Restore(uid, "")
	require.ErrorIs(t, err, ErrManagerStopped)

	require.ErrorIs(t, m.Purge(uid), ErrManagerStopped)
}
