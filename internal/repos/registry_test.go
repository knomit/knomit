package repos

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// openRegistry opens a registry on a temp control.db with a working Crypt, so
// credential round-trips exercise real encryption.
func openRegistry(t *testing.T) *RepoRegistry {
	t.Helper()
	r, err := OpenRepoRegistry(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("OpenRepoRegistry: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	crypt, err := store.NewCrypt([]byte("test-key-material"))
	if err != nil {
		t.Fatalf("store.NewCrypt: %v", err)
	}
	r.SetCrypt(crypt)
	return r
}

func TestRegistryUpsertAndList(t *testing.T) {
	r := openRegistry(t)
	rec := RepoRecord{
		Name:         "core",
		OriginURL:    "git@github.com:acme/kb.git",
		OriginBranch: "main",
		State:        RepoActive,
		CreatedAt:    time.Unix(1700000000, 0).UTC(),
	}
	if err := r.Upsert(rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := r.List(RepoActive)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(List) = %d, want 1", len(got))
	}
	if got[0].Name != "core" || got[0].OriginURL != rec.OriginURL || got[0].OriginBranch != "main" {
		t.Errorf("round-trip mismatch: %+v", got[0])
	}
	if !got[0].CreatedAt.Equal(rec.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got[0].CreatedAt, rec.CreatedAt)
	}
}

func TestRegistryUpsertIsUpdateOnConflict(t *testing.T) {
	r := openRegistry(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.Upsert(RepoRecord{Name: "core", OriginURL: "old", State: RepoActive}))
	must(r.Upsert(RepoRecord{Name: "core", OriginURL: "new", State: RepoActive}))

	got, err := r.List("")
	must(err)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (upsert must not duplicate)", len(got))
	}
	if got[0].OriginURL != "new" {
		t.Errorf("OriginURL = %q, want %q", got[0].OriginURL, "new")
	}
}

func TestRegistryStateFilter(t *testing.T) {
	r := openRegistry(t)
	_ = r.Upsert(RepoRecord{Name: "core", State: RepoActive})
	_ = r.Upsert(RepoRecord{Name: "old", State: RepoArchived, ArchiveID: "arc-1"})

	active, err := r.List(RepoActive)
	if err != nil {
		t.Fatalf("List(active): %v", err)
	}
	if len(active) != 1 || active[0].Name != "core" {
		t.Errorf("active = %+v, want just core", active)
	}

	archived, err := r.List(RepoArchived)
	if err != nil {
		t.Fatalf("List(archived): %v", err)
	}
	if len(archived) != 1 || archived[0].ArchiveID != "arc-1" {
		t.Errorf("archived = %+v, want just old/arc-1", archived)
	}

	all, err := r.List("")
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("len(all) = %d, want 2", len(all))
	}
}

// TestRegistryDeleteActiveAndArchive replaces the original SetState/Delete test.
// Those two took a bare name, which stopped identifying a single row once the
// key widened to (name, archive_id) — archiving is now "insert the archived row,
// retire the active one", so the registry exposes the two deletes that operation
// actually needs. This pins that each one touches ONLY its own row: the case
// where a name carries both a live repo and an archive of an earlier repo with
// the same name is exactly where a name-keyed delete did damage.
func TestRegistryDeleteActiveAndArchive(t *testing.T) {
	r := openRegistry(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// One name, two rows: a live repo and an archive of an earlier repo that
	// happened to share the name.
	must(r.Upsert(RepoRecord{Name: "work", State: RepoActive}))
	must(r.Upsert(RepoRecord{Name: "work", State: RepoArchived, ArchiveID: "arc-1"}))

	must(r.DeleteActive("work"))
	archived, err := r.List(RepoArchived)
	must(err)
	if len(archived) != 1 || archived[0].ArchiveID != "arc-1" {
		t.Errorf("DeleteActive took the archive with it: %+v", archived)
	}
	active, err := r.List(RepoActive)
	must(err)
	if len(active) != 0 {
		t.Errorf("DeleteActive left the active row: %+v", active)
	}

	// Put the live repo back, then purge the archive and confirm the reverse.
	must(r.Upsert(RepoRecord{Name: "work", State: RepoActive}))
	must(r.DeleteArchive("arc-1"))
	all, err := r.List("")
	must(err)
	if len(all) != 1 || all[0].State != RepoActive {
		t.Errorf("DeleteArchive must leave only the live repo, got %+v", all)
	}
}

// TestRegistryRecordLookups covers the single-row getters Archive's rollback and
// Restore's provenance carry-over depend on.
func TestRegistryRecordLookups(t *testing.T) {
	r := openRegistry(t)
	created := time.Unix(1700000000, 0).UTC()
	if err := r.Upsert(RepoRecord{Name: "work", State: RepoActive, CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if err := r.Upsert(RepoRecord{
		Name: "work", State: RepoArchived, ArchiveID: "arc-1", CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}

	act, ok, err := r.ActiveRecord("work")
	if err != nil || !ok {
		t.Fatalf("ActiveRecord: %v ok=%v", err, ok)
	}
	if act.ArchiveID != "" || !act.CreatedAt.Equal(created) {
		t.Errorf("ActiveRecord returned the wrong row: %+v", act)
	}

	arc, ok, err := r.ArchiveRecord("arc-1")
	if err != nil || !ok {
		t.Fatalf("ArchiveRecord: %v ok=%v", err, ok)
	}
	if arc.ArchiveID != "arc-1" || !arc.CreatedAt.Equal(created) {
		t.Errorf("ArchiveRecord returned the wrong row: %+v", arc)
	}

	if _, ok, err := r.ActiveRecord("nope"); err != nil || ok {
		t.Errorf("missing name: ok=%v err=%v, want false/nil", ok, err)
	}
	if _, ok, err := r.ArchiveRecord(""); err != nil || ok {
		t.Errorf("empty archive id: ok=%v err=%v, want false/nil", ok, err)
	}
}

// TestUpsertDoesNotDisturbCredentials is the guardrail regression test.
//
// Upsert builds its INSERT from a RepoRecord, which deliberately has no
// AuthToken field — so it can only ever pass the empty string. If the ON
// CONFLICT clause named the auth columns, every RecordOrigin write-through
// (boot, create, rescan, every origin edit) would blank the credential
// seconds after it was set, silently.
func TestUpsertDoesNotDisturbCredentials(t *testing.T) {
	reg := openRegistry(t)
	require.NoError(t, reg.Upsert(RepoRecord{Name: "work", State: RepoActive}))
	require.NoError(t, reg.SetOriginCredential("work", "token", "s3cret"))

	// An ordinary row upsert, exactly as RecordOrigin performs it.
	rec, found, err := reg.ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
	rec.OriginURL = "https://example.com/x.git"
	rec.OriginBranch = "main"
	require.NoError(t, reg.Upsert(rec))

	method, token, err := reg.OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "token", method, "upsert must not blank auth_method")
	require.Equal(t, "s3cret", token, "upsert must not blank auth_token")

	// A second upsert built from a FRESH RepoRecord, with no AuthMethod set —
	// exactly how Create (lifecycle.go) and Restore (lifecycle.go) build the
	// record they upsert, and how rebuildFromOrigin (manager.go) re-registers
	// an existing repo by routing through Create. The first assertion above
	// round-trips AuthMethod through a record read back from ActiveRecord, so
	// it stays populated for an unrelated reason and would not catch a
	// regression that starts from a zero-value RepoRecord.
	require.NoError(t, reg.Upsert(RepoRecord{Name: "work", State: RepoActive}))

	method, token, err = reg.OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "token", method, "upsert from a fresh record must not blank auth_method")
	require.Equal(t, "s3cret", token, "upsert from a fresh record must not blank auth_token")
}

func TestOriginCredentialRoundTrips(t *testing.T) {
	reg := openRegistry(t)
	require.NoError(t, reg.Upsert(RepoRecord{Name: "work", State: RepoActive}))
	require.NoError(t, reg.SetOriginCredential("work", "basic", "user:pass"))

	method, token, err := reg.OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "basic", method)
	require.Equal(t, "user:pass", token)
}

func TestOriginCredentialIsStoredEncrypted(t *testing.T) {
	reg := openRegistry(t)
	require.NoError(t, reg.Upsert(RepoRecord{Name: "work", State: RepoActive}))
	require.NoError(t, reg.SetOriginCredential("work", "token", "s3cret"))

	var raw string
	require.NoError(t, reg.db.QueryRow(
		`SELECT auth_token FROM repos WHERE name='work' AND archive_id=''`).Scan(&raw))
	require.NotEmpty(t, raw)
	require.NotContains(t, raw, "s3cret", "the credential must never be stored in plaintext")
}

func TestAuthMethodIsReadableOnRecord(t *testing.T) {
	reg := openRegistry(t)
	require.NoError(t, reg.Upsert(RepoRecord{Name: "work", State: RepoActive}))
	require.NoError(t, reg.SetOriginCredential("work", "token", "s3cret"))

	rec, found, err := reg.ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "token", rec.AuthMethod)
}

func TestSetOriginCredentialClearsWithEmptyValues(t *testing.T) {
	reg := openRegistry(t)
	require.NoError(t, reg.Upsert(RepoRecord{Name: "work", State: RepoActive}))
	require.NoError(t, reg.SetOriginCredential("work", "token", "s3cret"))
	require.NoError(t, reg.SetOriginCredential("work", "", ""))

	method, token, err := reg.OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "", method)
	require.Equal(t, "", token)
}

func TestCopyCredentialToArchivedRow(t *testing.T) {
	reg := openRegistry(t)
	require.NoError(t, reg.Upsert(RepoRecord{Name: "work", State: RepoActive}))
	require.NoError(t, reg.SetOriginCredential("work", "token", "s3cret"))
	require.NoError(t, reg.Upsert(RepoRecord{
		Name: "work", State: RepoArchived, ArchiveID: "arch1"}))

	var wantMethod, wantRaw string
	require.NoError(t, reg.db.QueryRow(
		`SELECT auth_method, auth_token FROM repos WHERE name='work' AND archive_id=''`).
		Scan(&wantMethod, &wantRaw))
	require.NotEmpty(t, wantRaw, "sanity: the source row must actually carry a credential")

	require.NoError(t, reg.CopyCredential("work", "", "work", "arch1"))

	// NotEmpty alone would pass if CopyCredential transposed its arguments, or
	// wrote the method string into auth_token, or wrote any other non-empty
	// junk. The actual requirement is that the archived row carries the
	// SOURCE row's ciphertext and method exactly, so it decrypts to the same
	// credential on restore.
	var gotMethod, gotRaw string
	require.NoError(t, reg.db.QueryRow(
		`SELECT auth_method, auth_token FROM repos WHERE name='work' AND archive_id='arch1'`).
		Scan(&gotMethod, &gotRaw))
	require.Equal(t, wantMethod, gotMethod, "the archived row's auth_method must match the source's")
	require.Equal(t, wantRaw, gotRaw, "the archived row's auth_token ciphertext must match the source's exactly")
}

// TestSetOriginCredentialRefusesPlaintextWithoutCrypt pins the most
// security-load-bearing line in this change: with no Crypt configured,
// SetOriginCredential must refuse to write a token rather than persist it in
// plaintext, and OriginCredential must refuse to read one back rather than
// hand the caller ciphertext it cannot decrypt. openRegistry always installs
// a Crypt, so this test deliberately builds its own registry without one.
func TestSetOriginCredentialRefusesPlaintextWithoutCrypt(t *testing.T) {
	reg, err := OpenRepoRegistry(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })
	require.NoError(t, reg.Upsert(RepoRecord{Name: "work", State: RepoActive}))

	err = reg.SetOriginCredential("work", "token", "s3cret")
	require.Error(t, err, "no crypt configured: a non-empty token must be refused, not written in plaintext")

	var raw string
	require.NoError(t, reg.db.QueryRow(
		`SELECT auth_token FROM repos WHERE name='work' AND archive_id=''`).Scan(&raw))
	require.Empty(t, raw, "a refused write must not leave a plaintext token in the row")

	// Mirror case: a credential written while a Crypt WAS available, then read
	// back after the Crypt is gone, must error rather than return ciphertext
	// to the caller as if it were the plaintext token.
	crypt, err := store.NewCrypt([]byte("test-key-material"))
	require.NoError(t, err)
	reg.SetCrypt(crypt)
	require.NoError(t, reg.SetOriginCredential("work", "token", "s3cret"))
	reg.SetCrypt(nil)

	_, _, err = reg.OriginCredential("work")
	require.Error(t, err, "no crypt configured: a stored credential must not be returned as ciphertext")
}
