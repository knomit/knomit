package git_test

import (
	"os"
	"path/filepath"
	"testing"

	git "knomit/internal/git"
)

func TestBranch(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"))
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

	if cfg.Remote {
		t.Fatal("DefaultConfig().Remote should be false")
	}
	if cfg.Port != "" {
		t.Fatalf("DefaultConfig().Port should be empty, got %q", cfg.Port)
	}
}

func TestReadFileNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, err = store.ReadFile("know/nonexistent.md")
	if err == nil {
		t.Fatal("expected error when reading nonexistent file")
	}
}

func TestFileExistsAfterWriteAndDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// know.md exists after init.
	exists, err := store.FileExists("know.md")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("know.md should exist after Init")
	}

	// A file that was never created should not exist.
	exists, err = store.FileExists("know/nope.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("know/nope.md should not exist")
	}

	// A deeply nested nonexistent path should not exist (exercises directory-not-found path).
	exists, err = store.FileExists("know/deep/nested/nothing.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("deeply nested nonexistent path should not exist")
	}
}

func TestDeleteFileRoundtrip(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Write a file.
	if _, _, err := store.WriteFile("know/ephemeral.md", "# Ephemeral\n\nTemporary.\n", "add ephemeral"); err != nil {
		t.Fatal(err)
	}

	// Verify it exists and is readable.
	content, err := store.ReadFile("know/ephemeral.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Ephemeral\n\nTemporary.\n" {
		t.Fatalf("unexpected content: %q", content)
	}

	// Delete it.
	if _, err := store.DeleteFile("know/ephemeral.md", "delete ephemeral"); err != nil {
		t.Fatal(err)
	}

	// Verify it no longer exists.
	exists, err := store.FileExists("know/ephemeral.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("file should not exist after deletion")
	}

	// Reading it should error.
	_, err = store.ReadFile("know/ephemeral.md")
	if err == nil {
		t.Fatal("expected error reading deleted file")
	}
}

func TestListAllEmptyStore(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// A freshly initialized store should have exactly know.md.
	paths, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 path (know.md) in fresh store, got %v", paths)
	}
	if paths[0] != "know.md" {
		t.Fatalf("expected know.md, got %q", paths[0])
	}
}

func TestGrepMatchAndNoMatch(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.WriteFile("know/cats.md", "# Cats\n\nCats are wonderful pets.\n", "add cats"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("know/dogs.md", "# Dogs\n\nDogs are loyal companions.\n", "add dogs"); err != nil {
		t.Fatal(err)
	}

	// Grep for a term that matches one file.
	matches, err := store.Grep("wonderful")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != "know/cats.md" {
		t.Fatalf("expected [know/cats.md], got %v", matches)
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
	if len(matches) != 1 || matches[0] != "know/dogs.md" {
		t.Fatalf("expected [know/dogs.md], got %v", matches)
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
	store, err := git.Init(filepath.Join(dir, "test.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Write a file and record the commit hash.
	if _, _, err := store.WriteFile("know/willdelete.md", "# Will Delete\n", "add willdelete"); err != nil {
		t.Fatal(err)
	}
	afterAdd, err := store.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}

	// Delete the file.
	if _, err := store.DeleteFile("know/willdelete.md", "delete willdelete"); err != nil {
		t.Fatal(err)
	}

	// Diff from the afterAdd commit to HEAD should show the file as deleted.
	added, modified, deleted, err := store.DiffFiles(afterAdd)
	if err != nil {
		t.Fatal(err)
	}

	var hasDeleted bool
	for _, p := range deleted {
		if p == "know/willdelete.md" {
			hasDeleted = true
		}
	}
	if !hasDeleted {
		t.Fatalf("expected know/willdelete.md in deleted, got added=%v modified=%v deleted=%v", added, modified, deleted)
	}
	if len(added) != 0 {
		t.Fatalf("expected no added files, got %v", added)
	}
	if len(modified) != 0 {
		t.Fatalf("expected no modified files, got %v", modified)
	}
}
