package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedLegacyCredential writes a token into a repo's store the pre-migration
// way, then closes the manager — producing exactly the on-disk shape an
// install upgrading into this change has.
func seedLegacyCredential(t *testing.T, m *Manager, name, url, token string) {
	t.Helper()
	svc := testService(t, m.Get(name))
	require.NoError(t, svc.Remote().SetRemote(
		"origin", url, "main", "machine/test", 300, 300, "token", token))
}

func newKeyedManager(t *testing.T, home, root string) *Manager {
	t.Helper()
	keyPath := filepath.Join(home, "id_ed25519")
	if _, err := os.Stat(keyPath); err != nil {
		require.NoError(t, os.WriteFile(keyPath, []byte("fake-key-material"), 0o600))
	}
	return newTestManager(t, home, func(d *Deps) {
		d.Cfg.LocalOriginRoot = root
		d.KeyPath = keyPath
	})
}

func TestBootMigratesLegacyCredentialIntoControlDB(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	first := newKeyedManager(t, home, root)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "work")
	seedLegacyCredential(t, first, "work", url, "s3cret")
	require.NoError(t, first.Close())

	second := newKeyedManager(t, home, root)
	require.NoError(t, second.Start())
	require.NotNil(t, second.Get("work"), "a migratable repo must still be served")

	method, token, err := second.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "token", method)
	require.Equal(t, "s3cret", token)

	// The store's copy is gone: an empty auth_token IS the migrated marker.
	svc := testService(t, second.Get("work"))
	sm, sTok, err := svc.Remote().LegacyAuth("origin")
	require.NoError(t, err)
	require.Equal(t, "", sm)
	require.Equal(t, "", sTok)
}

func TestBootMigrationIsIdempotent(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	first := newKeyedManager(t, home, root)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "work")
	seedLegacyCredential(t, first, "work", url, "s3cret")
	require.NoError(t, first.Close())

	second := newKeyedManager(t, home, root)
	require.NoError(t, second.Start())
	require.NoError(t, second.Close())

	third := newKeyedManager(t, home, root)
	require.NoError(t, third.Start())
	require.NotNil(t, third.Get("work"))
	_, token, err := third.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "s3cret", token, "a second boot must not disturb the migrated credential")
}

// TestBootMigratesBeforeOriginReconcile pins the ORDER of the two boot steps,
// which is the only thing standing between an upgrading install and a silently
// destroyed credential.
//
// reconcileOrigin's materialize-down path writes through store.SetRemote, which
// is INSERT OR REPLACE with EMPTY auth columns — so it BLANKS the store's
// auth_token. Run second, the migration would find that emptied column,
// conclude "nothing to migrate", and the only copy of the credential would be
// gone with no error raised anywhere.
//
// Making control.db's URL disagree with the store's is what sends reconcile
// down that path, so this is the boot where the order is observable.
func TestBootMigratesBeforeOriginReconcile(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	storeURL := seedBareRemote(t, filepath.Join(root, "store-remote.git"))
	controlURL := seedBareRemote(t, filepath.Join(root, "control-remote.git"))

	first := newKeyedManager(t, home, root)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "work")
	seedLegacyCredential(t, first, "work", storeURL, "s3cret")

	rec, found, err := first.RepoRegistry().ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
	rec.OriginURL = controlURL
	require.NoError(t, first.RepoRegistry().Upsert(rec))
	require.NoError(t, first.Close())

	second := newKeyedManager(t, home, root)
	require.NoError(t, second.Start())
	require.NotNil(t, second.Get("work"))

	// Proof the blanking path really ran on this boot: without it the store
	// would still name the URL it was seeded with, and the test would be
	// asserting nothing.
	svc := testService(t, second.Get("work"))
	rm, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rm)
	require.Equal(t, controlURL, rm.URL, "reconcileOrigin must have materialized control.db's URL")

	_, token, err := second.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "s3cret", token,
		"the credential must be migrated BEFORE reconcileOrigin blanks the store's auth columns")
}

func TestBootRefusesRepoWhoseCredentialCannotBeMigrated(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	first := newKeyedManager(t, home, root)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "work")
	seedLegacyCredential(t, first, "work", url, "s3cret")
	require.NoError(t, first.Close())

	// Agent key gone: the stored ciphertext can no longer be decrypted.
	require.NoError(t, os.Remove(filepath.Join(home, "id_ed25519")))

	second := newTestManager(t, home, func(d *Deps) {
		d.Cfg.LocalOriginRoot = root
		d.KeyPath = filepath.Join(home, "does-not-exist")
	})
	require.NoError(t, second.Start(), "one bad repo must not fail the boot")
	require.Nil(t, second.Get("work"), "an unmigratable repo must not be served")

	_, found, err := second.RepoRegistry().ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found, "its registry row must survive so the state stays diagnosable")
	require.NoError(t, second.Close())

	// The central safety promise: a FAILED migration leaves the store's original
	// intact. Restore the key and the credential must still be there, whole —
	// which is only true because ClearAuth runs strictly AFTER control.db has
	// been written, and never on the error path. Reorder those two and this is
	// the assertion that notices.
	third := newKeyedManager(t, home, root)
	require.NoError(t, third.Start())
	require.NotNil(t, third.Get("work"), "with the key back the repo migrates and is served")
	_, token, err := third.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "s3cret", token,
		"a refused migration must leave the store's copy untouched, so a restored key recovers it")
}

// TestFailedMigrationLeavesTheStoresCredentialIntact pins the plan's central
// safety promise: a migration that fails writes NOTHING, so the store keeps the
// only copy and the next boot retries from a clean state.
//
// The refusal test above cannot pin this on its own, and the reason is worth
// stating: there the read itself fails, so neither the control.db write nor
// ClearAuth is ever reached, and reordering those two is invisible to it. The
// dangerous window is a failure strictly AFTER a successful read, which is what
// this constructs — the store can decrypt, but control.db cannot encrypt, so the
// migration fails exactly at the write.
func TestFailedMigrationLeavesTheStoresCredentialIntact(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	m := newKeyedManager(t, home, root)
	require.NoError(t, m.Start())
	mustCreateRepo(t, m, "work")
	seedLegacyCredential(t, m, "work", url, "s3cret")

	// SetOriginCredential refuses to store a token with no key rather than
	// writing plaintext, so dropping the registry's crypt makes the write fail
	// while LegacyAuth still decrypts happily.
	m.RepoRegistry().SetCrypt(nil)

	dbPath := filepath.Join(home, "repos", "work.db")
	require.Error(t, m.gateCredential("work", dbPath), "an unwritable credential must refuse")
	require.Nil(t, m.Get("work"))

	// Re-open the store the gate just shut down and check the original is whole.
	require.NoError(t, m.Add("work", dbPath))
	svc := testService(t, m.Get("work"))
	method, token, err := svc.Remote().LegacyAuth("origin")
	require.NoError(t, err)
	require.Equal(t, "token", method)
	require.Equal(t, "s3cret", token,
		"ClearAuth must run strictly AFTER control.db is written, and never on the error path")
}

// TestCredentialGateShutsTheRefusedInstanceDown covers the OTHER half of
// refusing to serve. Dropping the name from the map is what makes a repo
// invisible; shutting the instance down is what stops it holding its SQLite
// handle, task hub and background goroutines for the life of the process. A test
// asserting only m.Get() == nil passes with the shutdown deleted.
//
// This exercises the shared gate directly, so it covers the Start, Rescan and
// Restore call sites at once — none of which can hand a test the instance
// pointer, since the refusal removes it from the map before they return.
func TestCredentialGateShutsTheRefusedInstanceDown(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	first := newKeyedManager(t, home, root)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "work")
	seedLegacyCredential(t, first, "work", url, "s3cret")
	require.NoError(t, first.Close())

	require.NoError(t, os.Remove(filepath.Join(home, "id_ed25519")))
	m := newTestManager(t, home, func(d *Deps) {
		d.Cfg.LocalOriginRoot = root
		d.KeyPath = filepath.Join(home, "does-not-exist")
	})
	require.NoError(t, m.Start())
	require.Nil(t, m.Get("work"), "Start must already have refused it")

	// Re-open it out of band — the same m.Add a rescan performs — so the test can
	// hold the pointer the gate is about to reject.
	dbPath := filepath.Join(home, "repos", "work.db")
	require.NoError(t, m.Add("work", dbPath))
	inst := m.Get("work")
	require.NotNil(t, inst)

	require.Error(t, m.gateCredential("work", dbPath))
	require.Nil(t, m.Get("work"), "refused: unregistered")

	_, _, aerr := inst.Acquire()
	require.Error(t, aerr,
		"refused: SHUT DOWN too — a live handle here would leak for the process lifetime")
	require.ErrorIs(t, aerr, ErrRepoClosed)
}

// TestRescanDoesNotServeRepoWhoseCredentialCannotBeMigrated closes the bypass
// that mattered most: Rescan's own doc advertises it as how a repo Start skipped
// comes back, so without the gate an operator's rescan would undo the boot's
// refusal and sync a private origin anonymously.
func TestRescanDoesNotServeRepoWhoseCredentialCannotBeMigrated(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	first := newKeyedManager(t, home, root)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "work")
	seedLegacyCredential(t, first, "work", url, "s3cret")
	require.NoError(t, first.Close())

	require.NoError(t, os.Remove(filepath.Join(home, "id_ed25519")))
	second := newTestManager(t, home, func(d *Deps) {
		d.Cfg.LocalOriginRoot = root
		d.KeyPath = filepath.Join(home, "does-not-exist")
	})
	require.NoError(t, second.Start())
	require.Nil(t, second.Get("work"))

	res, err := second.Rescan()
	require.NoError(t, err, "a refused repo must not fail the whole rescan")
	require.Nil(t, second.Get("work"), "a rescan must not re-serve what the gate refused")
	require.NotContains(t, res.Added, "work")
	require.Len(t, res.Errors, 1, "the refusal is reported with its reason")
	require.Equal(t, "work", res.Errors[0].Repo)

	// control.db still holds no credential, so a served repo really would have
	// synced anonymously — this is what the gate is protecting.
	_, token, err := second.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "", token)
}

// TestRestoreDoesNotServeRepoWhoseCredentialCannotBeMigrated covers the third
// door into service. An archived repo carries its legacy credential inside the
// .db that Restore moves back, so restoring is a path that puts an unmigrated
// repo into service.
func TestRestoreDoesNotServeRepoWhoseCredentialCannotBeMigrated(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	first := newKeyedManager(t, home, root)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "work")
	seedLegacyCredential(t, first, "work", url, "s3cret")
	info, err := first.Archive("work")
	require.NoError(t, err)
	require.NoError(t, first.Close())

	require.NoError(t, os.Remove(filepath.Join(home, "id_ed25519")))
	second := newTestManager(t, home, func(d *Deps) {
		d.Cfg.LocalOriginRoot = root
		d.KeyPath = filepath.Join(home, "does-not-exist")
	})
	require.NoError(t, second.Start())

	ri, rerr := second.Restore(info.ID, "")
	require.Error(t, rerr, "restoring an unmigratable repo must not report success")
	require.Nil(t, ri)
	require.Nil(t, second.Get("work"), "and it must not be left in service")

	// Restored-but-unserved, deliberately: the row survives so the next boot
	// retries once the key is back, rather than the restore being half-undone.
	_, found, err := second.RepoRegistry().ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
}

// TestBootNeverBlocksRepoWithNoCredential pins the gate's blast-radius promise:
// only a repo that ACTUALLY holds a credential may ever be refused.
//
// It covers BOTH shapes a credential-less repo comes in, because they take
// different paths through LegacyAuth and only one of them is the common case:
//
//   - "local" has no remotes row at all — LegacyAuth's sql.ErrNoRows branch.
//   - "public" has a remotes row that EXISTS with auth_token='' — the shape every
//     public-HTTPS, SSH and file:// origin has, and the one an install is
//     overwhelmingly likely to be made of.
//
// The second case is what a regression would actually break. LegacyAuth checks
// its empty token BEFORE it checks for a Crypt; hoist the crypt==nil check above
// that and every origin-having repo on a machine with an unreadable agent key is
// refused at boot — while the ErrNoRows case, which returns even earlier, keeps
// passing and reports nothing wrong.
func TestBootNeverBlocksRepoWithNoCredential(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	first := newKeyedManager(t, home, root)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "local")
	mustCreateRepo(t, first, "public")
	// A real origin, no credential: SetOrigin writes the remotes row with EMPTY
	// auth columns, which is exactly the row shape under test.
	require.NoError(t, first.SetOrigin(context.Background(), "public",
		OriginSpec{URL: url, Branch: "main"}, 300, 300))

	// Precondition, so the row-present case cannot go vacuous by turning into
	// the row-absent one behind the test's back.
	svc := testService(t, first.Get("public"))
	rm, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rm, "setup: the remotes row must EXIST for this case to differ from \"local\"")
	_, tok, err := svc.Remote().LegacyAuth("origin")
	require.NoError(t, err)
	require.Equal(t, "", tok, "setup: and it must hold no credential")
	require.NoError(t, first.Close())

	// No agent key at all. A repo with nothing to migrate has nothing
	// unmigrated about it, so the gate must not touch it.
	second := newTestManager(t, home, func(d *Deps) {
		d.Cfg.LocalOriginRoot = root
		d.KeyPath = filepath.Join(home, "does-not-exist")
	})
	require.NoError(t, second.Start())
	require.NotNil(t, second.Get("local"),
		"a repo with no credential must be served even with no agent key")
	require.NotNil(t, second.Get("public"),
		"a repo whose remotes row exists with an EMPTY auth_token must be served too: that is every "+
			"public, SSH and file:// origin, and refusing it would take a whole install offline")
}

func TestBootDoesNotReEncryptUndecryptableCiphertext(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	first := newKeyedManager(t, home, root)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "work")
	seedLegacyCredential(t, first, "work", url, "s3cret")
	require.NoError(t, first.Close())

	// Rotate the key: same length, different material.
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "id_ed25519"), []byte("rotated-key-material"), 0o600))

	second := newKeyedManager(t, home, root)
	require.NoError(t, second.Start())
	require.Nil(t, second.Get("work"), "an undecryptable credential must refuse, not re-encrypt")

	// control.db must hold NOTHING rather than double-encrypted garbage.
	var raw string
	require.NoError(t, second.RepoRegistry().db.QueryRow(
		`SELECT auth_token FROM repos WHERE name='work' AND archive_id=''`).Scan(&raw))
	require.Equal(t, "", raw, "a failed migration must not write a credential")
}
