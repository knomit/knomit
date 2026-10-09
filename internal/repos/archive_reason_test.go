package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// restartIn closes nothing: it builds and starts a NEW manager over home, the
// way a knomit restart would, and closes it at cleanup.
func restartIn(t *testing.T, home string, deps Deps) *Manager {
	t.Helper()
	deps.Cfg.Home = home
	m := New(context.Background(), deps)
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// archivedByUID returns the archived row for uid, failing when there is none.
func archivedByUID(t *testing.T, m *Manager, uid string) ArchiveInfo {
	t.Helper()
	list, err := m.ListArchived()
	require.NoError(t, err)
	for _, a := range list {
		if a.ID == uid {
			return a
		}
	}
	t.Fatalf("uid %s is not archived: %+v", uid, list)
	return ArchiveInfo{}
}

// An UNOPENABLE repo (a store that will not open, NOT a symlinked ontology, so
// boot leaves it to the user) is archived without mounting it, with the
// state it was in recorded; until then it blocks a re-add of the SAME
// knowledge base, and archiving alone unblocks it. Purge then removes it.
//
// Clones over a file:// transport — sequential on purpose (issue #446): no
// t.Parallel.
//
// Sabotage: archive back on the live map only → ErrRepoNotFound → red; drop
// the Condition fill → condition empty → red; drop delete(m.unavailable) in
// the unavailable branch → still listed → red.
func TestArchive_UnopenableRepoArchivesAndUnblocksReAdd(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	url := sourceRepo(t, root, "remote", sourceSpec{})
	home := t.TempDir()
	deps := Deps{
		Cfg:         config.Config{OntologyRoot: "kb", LocalOriginRoot: root},
		AgentBranch: "agent/test-abc",
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	}
	m := restartIn(t, home, deps)
	ri, err := m.Create(ctx, CreateSpec{Name: "kb1", Mode: "clone", Origin: &OriginSpec{URL: url}}, nil)
	require.NoError(t, err)
	uid := ri.UID()
	require.NoError(t, m.Close())
	require.NoError(t, os.WriteFile(m.RepoPath(uid), []byte("not a sqlite db"), 0o644))

	m2 := restartIn(t, home, deps)
	un := m2.Unavailable()
	require.Len(t, un, 1, "a corrupt store is NOT archived at boot: it can heal by itself")
	require.Equal(t, "unopenable", un[0].Reason)
	require.Empty(t, un[0].Cause)

	// The dead end this closes: the same knowledge base cannot be added again.
	_, err = m2.Create(ctx, CreateSpec{Name: "kb2", Mode: "clone", Origin: &OriginSpec{URL: url}}, nil)
	require.Error(t, err, "an unavailable registration still holds its origin")

	info, err := m2.ArchiveWith("kb1", ArchiveReason{Source: ArchiveByUser, Note: "file is corrupt"})
	require.NoError(t, err)
	require.Equal(t, uid, info.ID)
	require.NotNil(t, info.Reason)
	require.Equal(t, ArchiveByUser, info.Reason.Source)
	require.Equal(t, "file is corrupt", info.Reason.Note)
	require.Contains(t, info.Reason.Condition, "unopenable: ")
	require.Empty(t, m2.Unavailable(), "archived: no longer listed as unavailable")

	listed := archivedByUID(t, m2, uid)
	require.Equal(t, *info.Reason, *listed.Reason, "the archive response and the listing agree")

	_, err = m2.Create(ctx, CreateSpec{Name: "kb2", Mode: "clone", Origin: &OriginSpec{URL: url}}, nil)
	require.NoError(t, err, "archived, it no longer blocks the same knowledge base")

	require.NoError(t, m2.Purge(uid))
	_, err = os.Stat(m2.RepoPath(uid))
	require.True(t, os.IsNotExist(err), "purge removes the file")
	_, ok, err := m2.Repos().ArchiveReasonOf(uid)
	require.NoError(t, err)
	require.False(t, ok, "purge takes the reason row with it")
}

// A CONFLICT repo (its knowledge base is held by another active repo) is not
// archived at boot — which copy to keep is the user's call — and the user can
// archive it, with the conflict recorded, then purge it. The holder is
// untouched throughout.
//
// The conflict is made by copying one repo's database over another's file
// while knomit is down: b then holds a's root commit, and a's row already
// records it, so b loses at Identify whatever the mount order.
func TestArchive_ConflictRepoArchivesAndPurges(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	createRepo(t, m, "a")
	b := createRepo(t, m, "b")
	aPath := m.RepoPath(m.Get("a").UID())
	bUID := b.UID()
	home := m.deps.Cfg.Home
	require.NoError(t, m.Close())
	data, err := os.ReadFile(aPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(m.RepoPath(bUID), data, 0o644))

	m2 := restartIn(t, home, m.deps)
	require.NotNil(t, m2.Get("a"))
	un := m2.Unavailable()
	require.Len(t, un, 1, "a conflict is NOT archived at boot")
	require.Equal(t, "conflict", un[0].Reason)
	require.Equal(t, CauseIdentityConflict, un[0].Cause)

	info, err := m2.Archive("b")
	require.NoError(t, err)
	require.Equal(t, ArchiveByUser, info.Reason.Source)
	require.Empty(t, info.Reason.Note)
	require.Contains(t, info.Reason.Condition, "conflict: ")
	require.Empty(t, m2.Unavailable())
	require.NoError(t, m2.Purge(bUID))
	require.NotNil(t, m2.Get("a"), "the holder is untouched")
	createRepo(t, m2, "b") // the name is free again
}

// A MISSING repo (no file) archives too; its size is 0 and purge copes with
// the absent file.
func TestArchive_MissingRepoArchivesAndPurges(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	require.NoError(t, m.Start())
	uid := createRepo(t, m, "core").UID()
	home := m.deps.Cfg.Home
	require.NoError(t, m.Close())
	require.NoError(t, os.Remove(m.RepoPath(uid)))

	m2 := restartIn(t, home, m.deps)
	require.Len(t, m2.Unavailable(), 1, "a missing file is NOT archived at boot")
	info, err := m2.Archive("core")
	require.NoError(t, err)
	require.Zero(t, info.SizeBytes)
	require.Equal(t, "missing: database file not found", info.Reason.Condition)
	require.NoError(t, m2.Purge(uid))
	_, found, err := m2.Repos().Get(uid)
	require.NoError(t, err)
	require.False(t, found)
}

// A SYMLINKED ontology is archived at boot by the system, with the refusal as
// its reason. Restore re-runs Identify and is refused with the SAME text; the
// repo stays archived with its reason intact; purge then clears it. A
// lens-referenced one is NOT archived (archive refuses lens members) and stays
// unavailable.
//
// Sabotage: drop autoArchiveRefused from Start → still unavailable → red;
// clear the reason before mountExisting in Restore → reason gone after the
// refused restore → red; archive every Cause → the lens case is unaffected
// but the conflict/missing tests above go red.
func TestStart_SymlinkedOntologyIsArchivedBySystem(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	require.NoError(t, m.Start())
	linked := createRepo(t, m, "linked")
	member := createRepo(t, m, "member")
	createRepo(t, m, "fine")
	for _, ri := range []*RepoInstance{linked, member} {
		_, err := testService(t, ri).RawSymlinkForTest(ctx, "agent/test", OntologyPath, craftedLinkOntology, "symlink the ontology")
		require.NoError(t, err)
	}
	_, err := m.LensRegistry().Create(Lens{Name: "eng", WriteUID: member.UID(), CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	uid := linked.UID()
	home := m.deps.Cfg.Home
	require.NoError(t, m.Close())

	m2 := restartIn(t, home, m.deps)
	require.NotNil(t, m2.Get("fine"))
	require.Nil(t, m2.Get("linked"))
	un := m2.Unavailable()
	require.Len(t, un, 1, "only the lens member stays unavailable")
	require.Equal(t, "member", un[0].Record.Name)
	require.Equal(t, CauseOntologySymlink, un[0].Cause)

	a := archivedByUID(t, m2, uid)
	require.NotNil(t, a.Reason)
	require.Equal(t, ArchiveBySystem, a.Reason.Source)
	require.Contains(t, a.Reason.Reason, OntologyPath+" is a symlink, and knomit does not follow symlinks")
	require.Equal(t, "unopenable", a.Reason.Condition)

	_, err = m2.Restore(uid, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), OntologyPath+" is a symlink", "restore refuses with the same reason")
	require.Nil(t, m2.Get("linked"))
	again := archivedByUID(t, m2, uid)
	require.Equal(t, *a.Reason, *again.Reason, "a refused restore keeps the reason")

	require.NoError(t, m2.Purge(uid))
	createRepo(t, m2, "linked") // name free again
}

// A successful restore clears the reason; a user archive of a live repo
// records the note; an archived row from before reasons existed has none.
func TestArchiveReason_UserRestoreAndLegacy(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	require.NoError(t, m.Start())
	uid := createRepo(t, m, "work").UID()
	legacyUID := createRepo(t, m, "old").UID()

	info, err := m.ArchiveWith("work", ArchiveReason{Source: ArchiveByUser, Note: "done with it"})
	require.NoError(t, err)
	require.Equal(t, ArchiveReason{Source: ArchiveByUser, Note: "done with it"}, *info.Reason,
		"a live repo records no condition")

	_, err = m.Restore(uid, "")
	require.NoError(t, err)
	_, ok, err := m.Repos().ArchiveReasonOf(uid)
	require.NoError(t, err)
	require.False(t, ok, "a successful restore clears the reason")

	// Legacy: archived the way every build before reasons did — a bare state
	// flip, no reason row.
	m.Remove("old")
	require.NoError(t, m.Repos().SetState(legacyUID, StateArchived, 1))
	got, err := m.GetArchived(legacyUID)
	require.NoError(t, err)
	require.Nil(t, got.Reason, "no reason recorded")

	_, err = m.GetArchived(uid)
	require.ErrorIs(t, err, ErrArchiveNotFound, "an active repo is not an archived item")
}

// DeleteRepo records a SYSTEM reason: visible only if its purge fails, so it
// is asserted on the registry before the purge, through the archive half.
func TestDeleteRepo_RecordsSystemReason(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	require.NoError(t, m.Start())
	createRepo(t, m, "gone")
	info, err := m.archive("gone", ArchiveReason{Source: ArchiveBySystem, Reason: "create cancelled after it completed"})
	require.NoError(t, err)
	why, ok, err := m.Repos().ArchiveReasonOf(info.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, ArchiveBySystem, why.Source)
	require.NoError(t, m.Purge(info.ID))

	// And the whole of DeleteRepo leaves nothing behind.
	createRepo(t, m, "gone2")
	require.NoError(t, m.DeleteRepo("gone2", "test"))
	list, err := m.ListArchived()
	require.NoError(t, err)
	require.Empty(t, list)
	matches, _ := filepath.Glob(filepath.Join(m.deps.Cfg.Home, "repos", "*.db"))
	require.Empty(t, matches)
}

// Registry.Archive refuses an unknown source rather than storing one no
// reader understands.
func TestRegistryArchive_RejectsUnknownSource(t *testing.T) {
	t.Parallel()
	reg, err := OpenRegistry(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })
	require.NoError(t, reg.Insert(RepoRecord{UID: "u", Name: "n", State: StateActive, Profile: ProfileCode, CreatedAt: 1}))
	require.Error(t, reg.Archive("u", 2, ArchiveReason{Source: "robot"}))
	rec, _, err := reg.Get("u")
	require.NoError(t, err)
	require.Equal(t, StateActive, rec.State, "a refused archive changes nothing")
}
