package git_test

import (
	"os"
	"path/filepath"
	"testing"

	git "knomit/internal/git"
)

func TestBranch(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "local"
	}

	want := "agent/" + hostname
	got := store.Branch()
	if got != want {
		t.Fatalf("Branch() = %q, want %q", got, want)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := git.DefaultConfig()

	if !cfg.Serve {
		t.Fatal("DefaultConfig().Serve should be true")
	}
	if cfg.Origin != "" {
		t.Fatalf("DefaultConfig().Origin should be empty, got %q", cfg.Origin)
	}
	if cfg.Port != "" {
		t.Fatalf("DefaultConfig().Port should be empty, got %q", cfg.Port)
	}
}

func TestReadFileNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, err = store.ReadFile("general/nonexistent.md")
	if err == nil {
		t.Fatal("expected error when reading nonexistent file")
	}
}

func TestFileExistsAfterWriteAndDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// general.md exists after init.
	exists, err := store.FileExists("kb.md")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("general.md should exist after Init")
	}

	// A file that was never created should not exist.
	exists, err = store.FileExists("general/nope.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("general/nope.md should not exist")
	}

	// A deeply nested nonexistent path should not exist (exercises directory-not-found path).
	exists, err = store.FileExists("general/deep/nested/nothing.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("deeply nested nonexistent path should not exist")
	}
}

func TestDeleteFileRoundtrip(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Write a file.
	if _, _, err := store.WriteFile("general/ephemeral.md", "# Ephemeral\n\nTemporary.\n", "add ephemeral", "learn"); err != nil {
		t.Fatal(err)
	}

	// Verify it exists and is readable.
	content, err := store.ReadFile("general/ephemeral.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Ephemeral\n\nTemporary.\n" {
		t.Fatalf("unexpected content: %q", content)
	}

	// Delete it.
	if _, err := store.DeleteFile("general/ephemeral.md", "delete ephemeral", "retract"); err != nil {
		t.Fatal(err)
	}

	// Verify it no longer exists.
	exists, err := store.FileExists("general/ephemeral.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("file should not exist after deletion")
	}

	// Reading it should error.
	_, err = store.ReadFile("general/ephemeral.md")
	if err == nil {
		t.Fatal("expected error reading deleted file")
	}
}

func TestListAllEmptyStore(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// A freshly initialized store should have exactly general.md.
	paths, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 path (general.md) in fresh store, got %v", paths)
	}
	if paths[0] != "kb.md" {
		t.Fatalf("expected general.md, got %q", paths[0])
	}
}

func TestGrepMatchAndNoMatch(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.WriteFile("general/cats.md", "# Cats\n\nCats are wonderful pets.\n", "add cats", "learn"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("general/dogs.md", "# Dogs\n\nDogs are loyal companions.\n", "add dogs", "learn"); err != nil {
		t.Fatal(err)
	}

	// Grep for a term that matches one file.
	matches, err := store.Grep("wonderful")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != "general/cats.md" {
		t.Fatalf("expected [general/cats.md], got %v", matches)
	}

	// Grep for a term that matches no files.
	matches, err = store.Grep("elephant")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no matches, got %v", matches)
	}

	// Grep with a regex pattern.
	matches, err = store.Grep("loyal.*companions")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != "general/dogs.md" {
		t.Fatalf("expected [general/dogs.md], got %v", matches)
	}

	// Grep for a term in both files.
	matches, err = store.Grep("are")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %v", matches)
	}
}

func TestDiffFilesWithDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Write a file and record the commit hash.
	if _, _, err := store.WriteFile("general/willdelete.md", "# Will Delete\n", "add willdelete", "learn"); err != nil {
		t.Fatal(err)
	}
	afterAdd, err := store.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}

	// Delete the file.
	if _, err := store.DeleteFile("general/willdelete.md", "delete willdelete", "retract"); err != nil {
		t.Fatal(err)
	}

	// Diff from the afterAdd commit to HEAD should show the file as deleted.
	added, modified, deleted, err := store.DiffFiles(afterAdd)
	if err != nil {
		t.Fatal(err)
	}

	var hasDeleted bool
	for _, p := range deleted {
		if p == "general/willdelete.md" {
			hasDeleted = true
		}
	}
	if !hasDeleted {
		t.Fatalf("expected general/willdelete.md in deleted, got added=%v modified=%v deleted=%v", added, modified, deleted)
	}
	if len(added) != 0 {
		t.Fatalf("expected no added files, got %v", added)
	}
	if len(modified) != 0 {
		t.Fatalf("expected no modified files, got %v", modified)
	}
}
