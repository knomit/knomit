package git_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/storer"
	_ "github.com/mattn/go-sqlite3"

	storegit "knomit/internal/store/git"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func createSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	schema := `
CREATE TABLE IF NOT EXISTS objects (hash TEXT NOT NULL, type INTEGER NOT NULL, size INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY (hash, type));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target TEXT NOT NULL, is_symbolic INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value BLOB NOT NULL);
`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
}

func newTestStorer(t *testing.T) (*storegit.Storer, *sql.DB) {
	t.Helper()
	db := openTestDB(t)
	createSchema(t, db)
	return storegit.NewStorer(db), db
}

func storeBlob(t *testing.T, s *storegit.Storer, content string) plumbing.Hash {
	t.Helper()
	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(content))
	w.Close()
	h, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// --- Constructor ---

func TestNewStorer(t *testing.T) {
	db := openTestDB(t)
	s := storegit.NewStorer(db)
	if s == nil {
		t.Fatal("expected non-nil Storer")
	}
}

// --- Context-based tx propagation ---

func TestContextTxRoutesWritesThroughTransaction(t *testing.T) {
	_, db := newTestStorer(t)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	ctx := storegit.WithTx(context.Background(), tx)

	// Write through the context-propagated transaction
	conn := storegit.Conn(ctx, db)
	_, err = conn.ExecContext(ctx,
		`INSERT INTO objects (hash, type, size, data) VALUES (?, ?, ?, ?)`,
		"abcdabcdabcdabcdabcdabcdabcdabcdabcdabcd", 3, 5, []byte("hello"),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Before commit, the object should NOT be visible outside the tx
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM objects`).Scan(&count)
	if count != 0 {
		t.Fatalf("expected 0 objects outside tx, got %d", count)
	}

	tx.Commit()

	// After commit, the object should be visible
	db.QueryRow(`SELECT COUNT(*) FROM objects`).Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 object after commit, got %d", count)
	}
}

// --- Object round-trip ---

func TestObjectRoundTrip(t *testing.T) {
	s, _ := newTestStorer(t)

	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, _ := obj.Writer()
	w.Write([]byte("hello world"))
	w.Close()

	hash, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("SetEncodedObject: %v", err)
	}

	got, err := s.EncodedObject(plumbing.BlobObject, hash)
	if err != nil {
		t.Fatalf("EncodedObject: %v", err)
	}
	r, _ := got.Reader()
	buf := make([]byte, 11)
	r.Read(buf)
	if string(buf) != "hello world" {
		t.Fatalf("got %q, want %q", buf, "hello world")
	}
}

func TestIterEncodedObjects(t *testing.T) {
	s, _ := newTestStorer(t)

	for _, content := range []string{"foo", "bar"} {
		obj := s.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		w, err := obj.Writer()
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
		w.Close()
		if _, err := s.SetEncodedObject(obj); err != nil {
			t.Fatal(err)
		}
	}

	iter, err := s.IterEncodedObjects(plumbing.BlobObject)
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()

	count := 0
	if err := iter.ForEach(func(obj plumbing.EncodedObject) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 objects, got %d", count)
	}
}

func TestEncodedObjectAnyType(t *testing.T) {
	s, _ := newTestStorer(t)

	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, _ := obj.Writer()
	w.Write([]byte("any-type-test"))
	w.Close()
	hash, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		t.Fatalf("EncodedObject(AnyObject): %v", err)
	}
	if got.Type() != plumbing.BlobObject {
		t.Fatalf("expected BlobObject, got %v", got.Type())
	}
}

func TestHasEncodedObject(t *testing.T) {
	s, _ := newTestStorer(t)
	h := storeBlob(t, s, "has-test")

	if err := s.HasEncodedObject(h); err != nil {
		t.Fatalf("HasEncodedObject returned error for existing object: %v", err)
	}

	bogus := plumbing.NewHash("0000000000000000000000000000000000000000")
	if err := s.HasEncodedObject(bogus); err != plumbing.ErrObjectNotFound {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
}

func TestEncodedObjectSize(t *testing.T) {
	s, _ := newTestStorer(t)
	content := "size-test-content"
	h := storeBlob(t, s, content)

	size, err := s.EncodedObjectSize(h)
	if err != nil {
		t.Fatalf("EncodedObjectSize: %v", err)
	}
	if size != int64(len(content)) {
		t.Fatalf("expected size %d, got %d", len(content), size)
	}

	bogus := plumbing.NewHash("0000000000000000000000000000000000000000")
	_, err = s.EncodedObjectSize(bogus)
	if err != plumbing.ErrObjectNotFound {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
}

// --- Reference round-trip ---

func TestRefRoundTrip(t *testing.T) {
	s, _ := newTestStorer(t)

	ref := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err := s.SetReference(ref); err != nil {
		t.Fatal(err)
	}
	got, err := s.Reference("refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name() != ref.Name() || got.Hash() != ref.Hash() {
		t.Fatalf("got %v %v, want %v %v", got.Name(), got.Hash(), ref.Name(), ref.Hash())
	}
}

func TestSymbolicRef(t *testing.T) {
	s, _ := newTestStorer(t)

	head := plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/main")
	if err := s.SetReference(head); err != nil {
		t.Fatal(err)
	}
	got, err := s.Reference(plumbing.HEAD)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type() != plumbing.SymbolicReference {
		t.Fatalf("expected symbolic reference")
	}
	if got.Target() != "refs/heads/main" {
		t.Fatalf("got target %v", got.Target())
	}
}

func TestIterReferences(t *testing.T) {
	s, _ := newTestStorer(t)

	refs := []*plumbing.Reference{
		plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")),
		plumbing.NewHashReference("refs/heads/dev", plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")),
	}
	for _, r := range refs {
		if err := s.SetReference(r); err != nil {
			t.Fatal(err)
		}
	}

	iter, err := s.IterReferences()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := iter.ForEach(func(r *plumbing.Reference) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 refs, got %d", count)
	}
}

func TestRemoveReference(t *testing.T) {
	s, _ := newTestStorer(t)

	ref := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err := s.SetReference(ref); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveReference("refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Reference("refs/heads/main")
	if err != plumbing.ErrReferenceNotFound {
		t.Fatalf("expected ErrReferenceNotFound, got %v", err)
	}
}

func TestCheckAndSetReference(t *testing.T) {
	s, _ := newTestStorer(t)

	ref := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err := s.SetReference(ref); err != nil {
		t.Fatal(err)
	}

	wrongOld := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("cccccccccccccccccccccccccccccccccccccccc"))
	newRef := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	err := s.CheckAndSetReference(newRef, wrongOld)
	if err == nil {
		t.Fatal("expected ErrReferenceHasChanged")
	}

	correctOld := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err := s.CheckAndSetReference(newRef, correctOld); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCountLooseRefs(t *testing.T) {
	s, _ := newTestStorer(t)

	count, err := s.CountLooseRefs()
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 refs, got %d", count)
	}

	refs := []*plumbing.Reference{
		plumbing.NewHashReference("refs/heads/a", plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")),
		plumbing.NewHashReference("refs/heads/b", plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")),
		plumbing.NewHashReference("refs/heads/c", plumbing.NewHash("cccccccccccccccccccccccccccccccccccccccc")),
	}
	for _, r := range refs {
		if err := s.SetReference(r); err != nil {
			t.Fatal(err)
		}
	}

	count, err = s.CountLooseRefs()
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 refs, got %d", count)
	}
}

func TestPackRefs(t *testing.T) {
	s, _ := newTestStorer(t)
	if err := s.PackRefs(); err != nil {
		t.Fatalf("PackRefs returned error: %v", err)
	}
}

func TestForEachRef(t *testing.T) {
	s, _ := newTestStorer(t)

	expected := map[plumbing.ReferenceName]plumbing.Hash{
		"refs/heads/main": plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		"refs/heads/dev":  plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		"refs/tags/v1.0":  plumbing.NewHash("cccccccccccccccccccccccccccccccccccccccc"),
	}
	for name, hash := range expected {
		if err := s.SetReference(plumbing.NewHashReference(name, hash)); err != nil {
			t.Fatal(err)
		}
	}

	iter, err := s.IterReferences()
	if err != nil {
		t.Fatal(err)
	}

	visited := make(map[plumbing.ReferenceName]plumbing.Hash)
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		visited[ref.Name()] = ref.Hash()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(visited) != len(expected) {
		t.Fatalf("expected %d refs, visited %d", len(expected), len(visited))
	}
	for name, hash := range expected {
		if visited[name] != hash {
			t.Fatalf("ref %s: expected %s, got %s", name, hash, visited[name])
		}
	}
}

func TestForEachRefErrStop(t *testing.T) {
	s, _ := newTestStorer(t)

	for _, name := range []string{"refs/heads/a", "refs/heads/b", "refs/heads/c"} {
		if err := s.SetReference(plumbing.NewHashReference(
			plumbing.ReferenceName(name),
			plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		)); err != nil {
			t.Fatal(err)
		}
	}

	iter, err := s.IterReferences()
	if err != nil {
		t.Fatal(err)
	}

	count := 0
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		count++
		return storer.ErrStop
	})
	if err != nil {
		t.Fatalf("ForEach with ErrStop returned error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 visit before ErrStop, got %d", count)
	}
}

func TestRefIterClose(t *testing.T) {
	s, _ := newTestStorer(t)

	if err := s.SetReference(plumbing.NewHashReference(
		"refs/heads/main",
		plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	)); err != nil {
		t.Fatal(err)
	}

	iter, err := s.IterReferences()
	if err != nil {
		t.Fatal(err)
	}

	iter.Close()
	iter.Close()
}

// --- Config ---

func TestConfigRoundTrip(t *testing.T) {
	s, _ := newTestStorer(t)

	cfg := config.NewConfig()
	cfg.User.Name = "Test User"
	cfg.User.Email = "test@example.com"
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := s.Config()
	if err != nil {
		t.Fatal(err)
	}
	if got.User.Name != cfg.User.Name || got.User.Email != cfg.User.Email {
		t.Fatalf("config mismatch: got %v/%v, want %v/%v",
			got.User.Name, got.User.Email, cfg.User.Name, cfg.User.Email)
	}
}

func TestConfigRoundTripEmpty(t *testing.T) {
	s, _ := newTestStorer(t)

	cfg, err := s.Config()
	if err != nil {
		t.Fatalf("Config on fresh store: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
}

func TestConfigRoundTripWithRemote(t *testing.T) {
	s, _ := newTestStorer(t)

	cfg := config.NewConfig()
	cfg.Core.IsBare = true
	cfg.Remotes["origin"] = &config.RemoteConfig{
		Name: "origin",
		URLs: []string{"https://example.com/repo.git"},
	}
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}

	got, err := s.Config()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Core.IsBare {
		t.Fatal("expected Core.IsBare=true")
	}
	remote, ok := got.Remotes["origin"]
	if !ok {
		t.Fatal("expected remote 'origin'")
	}
	if remote.Name != "origin" {
		t.Fatalf("expected remote name 'origin', got %q", remote.Name)
	}
	if len(remote.URLs) != 1 || remote.URLs[0] != "https://example.com/repo.git" {
		t.Fatalf("unexpected remote URLs: %v", remote.URLs)
	}
}

// --- Index ---

func TestIndexRoundTrip(t *testing.T) {
	s, _ := newTestStorer(t)

	idx := &index.Index{Version: 2}
	if err := s.SetIndex(idx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Index()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != idx.Version {
		t.Fatalf("index version mismatch: got %d, want %d", got.Version, idx.Version)
	}
}

func TestSetIndexWithEntries(t *testing.T) {
	s, _ := newTestStorer(t)

	idx := &index.Index{
		Version: 2,
		Entries: []*index.Entry{
			{
				Name:  "file.txt",
				Hash:  plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
				Stage: index.Merged,
			},
		},
	}
	if err := s.SetIndex(idx); err != nil {
		t.Fatal(err)
	}

	got, err := s.Index()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 {
		t.Fatalf("expected version 2, got %d", got.Version)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got.Entries))
	}
	if got.Entries[0].Name != "file.txt" {
		t.Fatalf("expected entry name 'file.txt', got %q", got.Entries[0].Name)
	}
	if got.ModTime.IsZero() {
		t.Fatal("expected non-zero ModTime after SetIndex")
	}
}

func TestSetIndexEmpty(t *testing.T) {
	s, _ := newTestStorer(t)

	idx, err := s.Index()
	if err != nil {
		t.Fatal(err)
	}
	if idx.Version != 2 {
		t.Fatalf("expected default version 2, got %d", idx.Version)
	}
}

// --- Shallow ---

func TestShallowRoundTrip(t *testing.T) {
	s, _ := newTestStorer(t)

	hashes, err := s.Shallow()
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 0 {
		t.Fatalf("expected empty shallow list, got %d items", len(hashes))
	}

	commits := []plumbing.Hash{
		plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}
	if err := s.SetShallow(commits); err != nil {
		t.Fatal(err)
	}
	got, err := s.Shallow()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 shallow commits, got %d", len(got))
	}
	if got[0] != commits[0] || got[1] != commits[1] {
		t.Fatalf("shallow hash mismatch")
	}
}

// --- Module ---

func TestModuleRoundTrip(t *testing.T) {
	s, _ := newTestStorer(t)

	ms, err := s.Module("mymodule")
	if err != nil {
		t.Fatal(err)
	}
	if ms == nil {
		t.Fatal("expected non-nil module storer")
	}
}
