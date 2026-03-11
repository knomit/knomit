package git_test

import (
	"path/filepath"
	"testing"

	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"

	git "knomit/internal/git"
)

func TestInitAndReadFile(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	content := "---\ndomain: [test]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Test Fact\n\nBody.\n"
	if err := store.WriteFile("know/test/fact.md", content, "test: write fact"); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadFile("know/test/fact.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != content {
		t.Fatalf("content mismatch:\ngot:  %q\nwant: %q", got, content)
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	exists, err := store.FileExists("know.md")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("know.md should exist after Init")
	}

	exists, err = store.FileExists("know/nonexistent.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("nonexistent file should not exist")
	}
}

func TestListDir(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.WriteFile("know/alpha.md", "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# Alpha\n\nBody.\n", "add alpha"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFile("know/sub/beta.md", "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# Beta\n\nBody.\n", "add beta"); err != nil {
		t.Fatal(err)
	}

	entries, err := store.ListDir("know")
	if err != nil {
		t.Fatal(err)
	}

	var hasAlpha, hasSub bool
	for _, e := range entries {
		if e.Name == "alpha.md" && !e.IsWorld {
			hasAlpha = true
		}
		if e.Name == "sub" && e.IsWorld {
			hasSub = true
		}
	}
	if !hasAlpha {
		t.Fatal("expected alpha.md in know/")
	}
	if !hasSub {
		t.Fatal("expected sub/ in know/")
	}
}

func TestLog(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.WriteFile("know/test.md", "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nv1.\n", "add test"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFile("know/test.md", "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nv2.\n", "update test"); err != nil {
		t.Fatal(err)
	}

	entries, err := store.Log("know/test.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected >=2 log entries, got %d", len(entries))
	}
}

func TestOpenRoundtrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "knomit.git.db")

	store, err := git.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	content := "# Hello\n\nWorld.\n"
	if err := store.WriteFile("know/hello.md", content, "add hello"); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store2, err := git.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	got, err := store2.ReadFile("know/hello.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != content {
		t.Fatalf("open roundtrip mismatch:\ngot:  %q\nwant: %q", got, content)
	}
}

func TestHeadCommit(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	h, err := store.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 40 {
		t.Fatalf("expected 40-char hex hash, got %q (len %d)", h, len(h))
	}
}

func TestWriteFileValidation(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.WriteFile("", "content", "msg"); err == nil {
		t.Fatal("expected error for empty path")
	}
	if err := store.WriteFile("../escape.md", "content", "msg"); err == nil {
		t.Fatal("expected error for path traversal")
	}
}

func TestListDirRoot(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	entries, err := store.ListDir("")
	if err != nil {
		t.Fatal(err)
	}

	var hasKnowMd bool
	for _, e := range entries {
		if e.Name == "know.md" && !e.IsWorld {
			hasKnowMd = true
		}
	}
	if !hasKnowMd {
		t.Fatal("expected know.md in root listing")
	}
}

func TestDeleteFile(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Write a file, then delete it.
	if err := store.WriteFile("know/todelete.md", "# Delete me\n", "add file"); err != nil {
		t.Fatal(err)
	}
	exists, err := store.FileExists("know/todelete.md")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("file should exist before deletion")
	}

	if err := store.DeleteFile("know/todelete.md", "delete: remove todelete.md"); err != nil {
		t.Fatal(err)
	}

	exists, err = store.FileExists("know/todelete.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("file should not exist after deletion")
	}
}

func TestTag(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.WriteFile("know/tagged.md", "# Tagged\n", "add tagged file"); err != nil {
		t.Fatal(err)
	}

	if err := store.Tag("v1.0"); err != nil {
		t.Fatal(err)
	}

	// Check that the tag ref exists and points to HEAD.
	headHash, err := store.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}

	tags, err := store.TagsContaining(headHash)
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, tag := range tags {
		if tag == "v1.0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected tag v1.0 to contain HEAD commit %s, got tags: %v", headHash, tags)
	}
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.WriteFile("know/alpha.md", "# Alpha\n\nThis file contains the word elephant.\n", "add alpha"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFile("know/beta.md", "# Beta\n\nThis file is about dogs.\n", "add beta"); err != nil {
		t.Fatal(err)
	}

	matches, err := store.Grep("elephant")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != "know/alpha.md" {
		t.Fatalf("expected [know/alpha.md], got %v", matches)
	}

	// Grep for something in both files.
	matches, err = store.Grep("This file")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %v", matches)
	}
}

func TestDiffFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Get the commit before any writes.
	baseHash, err := store.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}

	if err := store.WriteFile("know/new.md", "# New\n", "add new"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFile("know.md", "# Knowledge Base\n\nUpdated root.\n", "update root"); err != nil {
		t.Fatal(err)
	}

	added, modified, deleted, err := store.DiffFiles(baseHash)
	if err != nil {
		t.Fatal(err)
	}

	var hasNew bool
	for _, p := range added {
		if p == "know/new.md" {
			hasNew = true
		}
	}
	if !hasNew {
		t.Fatalf("expected know/new.md in added, got added=%v modified=%v deleted=%v", added, modified, deleted)
	}

	var hasModified bool
	for _, p := range modified {
		if p == "know.md" {
			hasModified = true
		}
	}
	if !hasModified {
		t.Fatalf("expected know.md in modified, got added=%v modified=%v deleted=%v", added, modified, deleted)
	}
}

func TestDiffFilesFromEmpty(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// DiffFiles with empty fromCommit = diff from empty tree.
	added, modified, deleted, err := store.DiffFiles("")
	if err != nil {
		t.Fatal(err)
	}

	var hasKnowMd bool
	for _, p := range added {
		if p == "know.md" {
			hasKnowMd = true
		}
	}
	if !hasKnowMd {
		t.Fatalf("expected know.md in added when diffing from empty, got added=%v modified=%v deleted=%v", added, modified, deleted)
	}
}

func TestBatchWriteValidation(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.BatchWrite(map[string]string{"": "content"}, "msg"); err == nil {
		t.Fatal("expected error for empty path in BatchWrite")
	}
	if err := store.BatchWrite(map[string]string{"../escape.md": "content"}, "msg"); err == nil {
		t.Fatal("expected error for path traversal in BatchWrite")
	}
}

// TestSync verifies Sync behaviour with and without a configured origin.
func TestSync(t *testing.T) {
	t.Run("no origin returns Synced=false", func(t *testing.T) {
		dir := t.TempDir()
		store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		result, err := store.Sync(nil)
		if err != nil {
			t.Fatalf("Sync with no remote returned unexpected error: %v", err)
		}
		if result.Synced {
			t.Fatal("Sync with no remote should return Synced=false")
		}
	})

	t.Run("with origin merges new commit", func(t *testing.T) {
		// Set up origin store (SQLite-backed go-git repo).
		originDir := t.TempDir()
		origin, err := git.Init(filepath.Join(originDir, "origin.git.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer origin.Close()

		// Add a commit to origin's agent branch (WriteFile always targets the
		// agent branch), then advance origin's main ref to that commit so that
		// origin/main has content the agent store has never seen.
		if err := origin.WriteFile("know/shared.md", "# Shared\n", "origin: add shared"); err != nil {
			t.Fatal(err)
		}
		originHead, err := origin.HeadCommit()
		if err != nil {
			t.Fatal(err)
		}
		// Point origin's main branch at the new commit so that
		// refs/remotes/origin/main will be ahead of the agent's history.
		mainRef := plumbing.NewHashReference(
			plumbing.NewBranchReferenceName("main"),
			plumbing.NewHash(originHead),
		)
		if err := origin.Storer().SetReference(mainRef); err != nil {
			t.Fatal(err)
		}

		// Register an in-process transport so the knomit store can fetch
		// from the origin without network or git binary.
		loader := server.MapLoader{
			"inmem:///origin": origin.Storer(),
		}
		client.InstallProtocol("inmem", server.NewClient(loader))
		t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

		// Create the agent store.
		agentDir := t.TempDir()
		store, err := git.Init(filepath.Join(agentDir, "knomit.git.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		// Configure origin remote in the agent store via the storer's Config API.
		cfg, err := store.Storer().Config()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Remotes["origin"] = &gogitconfig.RemoteConfig{
			Name:  "origin",
			URLs:  []string{"inmem:///origin"},
			Fetch: []gogitconfig.RefSpec{"+refs/heads/*:refs/remotes/origin/*"},
		}
		if err := store.Storer().SetConfig(cfg); err != nil {
			t.Fatal(err)
		}

		// Sync should fetch origin/main and merge it.
		result, err := store.Sync(nil)
		if err != nil {
			t.Fatalf("Sync returned unexpected error: %v", err)
		}
		if !result.Synced {
			t.Fatal("expected Synced=true after fetching new commits from origin")
		}

		// The merged file should now be accessible.
		exists, err := store.FileExists("know/shared.md")
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatal("expected know/shared.md to exist after merge")
		}
	})
}

func TestBatchWrite(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	files := map[string]string{
		"know/a.md": "# A\n\nContent A.\n",
		"know/b.md": "# B\n\nContent B.\n",
	}

	if err := store.BatchWrite(files, "batch: add a and b"); err != nil {
		t.Fatal(err)
	}

	// Verify both files exist and have correct content.
	for path, want := range files {
		got, err := store.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		if got != want {
			t.Fatalf("content mismatch for %q:\ngot:  %q\nwant: %q", path, got, want)
		}
	}

	// A batch write should be a single commit (not two).
	logEntries, err := store.Log("know/a.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(logEntries) == 0 {
		t.Fatal("expected at least one log entry for know/a.md")
	}
	if logEntries[0].Message != "batch: add a and b" {
		t.Fatalf("expected batch commit message, got %q", logEntries[0].Message)
	}
}
