package gitstorer_test

import (
	"testing"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"knomit/internal/gitstorer"
)

func TestObjectRoundTrip(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Write a blob
	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, _ := obj.Writer()
	w.Write([]byte("hello world"))
	w.Close()

	hash, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("SetEncodedObject: %v", err)
	}

	// Read it back
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
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

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
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, _ := obj.Writer()
	w.Write([]byte("any-type-test"))
	w.Close()
	hash, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}

	// Should find with AnyObject
	got, err := s.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		t.Fatalf("EncodedObject(AnyObject): %v", err)
	}
	if got.Type() != plumbing.BlobObject {
		t.Fatalf("expected BlobObject, got %v", got.Type())
	}
}

func TestRefRoundTrip(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

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
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

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
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

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
	// schema_version is stored in kv, not refs — count should be exactly 2
	if count != 2 {
		t.Fatalf("expected 2 refs, got %d", count)
	}
}

func TestRemoveReference(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ref := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err := s.SetReference(ref); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveReference("refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Reference("refs/heads/main")
	if err != plumbing.ErrReferenceNotFound {
		t.Fatalf("expected ErrReferenceNotFound, got %v", err)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

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

func TestIndexRoundTrip(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

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

func TestShallowRoundTrip(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Empty initially
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

func TestModuleRoundTrip(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ms, err := s.Module("mymodule")
	if err != nil {
		t.Fatal(err)
	}
	if ms == nil {
		t.Fatal("expected non-nil module storer")
	}
}

func TestCheckAndSetReference(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ref := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err := s.SetReference(ref); err != nil {
		t.Fatal(err)
	}

	// Wrong old hash should fail
	wrongOld := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("cccccccccccccccccccccccccccccccccccccccc"))
	newRef := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	err = s.CheckAndSetReference(newRef, wrongOld)
	if err == nil {
		t.Fatal("expected ErrReferenceHasChanged")
	}

	// Correct old hash should succeed
	correctOld := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err := s.CheckAndSetReference(newRef, correctOld); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
