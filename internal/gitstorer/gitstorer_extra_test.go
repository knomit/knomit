package gitstorer_test

import (
	"testing"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"knomit/internal/gitstorer"
)

func storeBlob(t *testing.T, s *gitstorer.Storer, content string) plumbing.Hash {
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

func TestHasEncodedObject(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	h := storeBlob(t, s, "has-test")

	// Existing object should return nil.
	if err := s.HasEncodedObject(h); err != nil {
		t.Fatalf("HasEncodedObject returned error for existing object: %v", err)
	}

	// Non-existent object should return ErrObjectNotFound.
	bogus := plumbing.NewHash("0000000000000000000000000000000000000000")
	if err := s.HasEncodedObject(bogus); err != plumbing.ErrObjectNotFound {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
}

func TestEncodedObjectSize(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	content := "size-test-content"
	h := storeBlob(t, s, content)

	size, err := s.EncodedObjectSize(h)
	if err != nil {
		t.Fatalf("EncodedObjectSize: %v", err)
	}
	if size != int64(len(content)) {
		t.Fatalf("expected size %d, got %d", len(content), size)
	}

	// Non-existent object should return ErrObjectNotFound.
	bogus := plumbing.NewHash("0000000000000000000000000000000000000000")
	_, err = s.EncodedObjectSize(bogus)
	if err != plumbing.ErrObjectNotFound {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
}

func TestCountLooseRefs(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Initially zero refs.
	count, err := s.CountLooseRefs()
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 refs, got %d", count)
	}

	// Add some refs.
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
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// PackRefs is a no-op; just verify it returns nil.
	if err := s.PackRefs(); err != nil {
		t.Fatalf("PackRefs returned error: %v", err)
	}
}

func TestForEachRef(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	expected := map[plumbing.ReferenceName]plumbing.Hash{
		"refs/heads/main":    plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		"refs/heads/dev":     plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		"refs/tags/v1.0":     plumbing.NewHash("cccccccccccccccccccccccccccccccccccccccc"),
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
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

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
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

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

	// Close without iterating; should not panic.
	iter.Close()

	// Double close should also be safe.
	iter.Close()
}

func TestConfigRoundTripEmpty(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Config on a fresh store should return an empty config, not an error.
	cfg, err := s.Config()
	if err != nil {
		t.Fatalf("Config on fresh store: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
}

func TestConfigRoundTripWithRemote(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

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

func TestSetIndexWithEntries(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	idx := &index.Index{
		Version: 2,
		Entries: []*index.Entry{
			{
				Name: "file.txt",
				Hash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
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

	// ModTime should have been set (non-zero).
	if got.ModTime.IsZero() {
		t.Fatal("expected non-zero ModTime after SetIndex")
	}
}

func TestSetIndexEmpty(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Index on fresh store returns default.
	idx, err := s.Index()
	if err != nil {
		t.Fatal(err)
	}
	if idx.Version != 2 {
		t.Fatalf("expected default version 2, got %d", idx.Version)
	}
}
