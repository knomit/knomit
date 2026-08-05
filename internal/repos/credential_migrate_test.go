package repos

import (
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
}

func TestBootNeverBlocksRepoWithNoCredential(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()

	first := newKeyedManager(t, home, root)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "local")
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
