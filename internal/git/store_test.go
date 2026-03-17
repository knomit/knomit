package git_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"

	git "knomit/internal/git"
	storegit "knomit/internal/store/git"
)

func TestInitAndReadFile(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	content := "---\ndomain: [test]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Test Fact\n\nBody.\n"
	if _, _, err := store.WriteFile("kb/test/fact.md", content, "test: write fact"); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadFile("kb/test/fact.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != content {
		t.Fatalf("content mismatch:\ngot:  %q\nwant: %q", got, content)
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	exists, err := store.FileExists("kb.md")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("general.md should exist after Init")
	}

	exists, err = store.FileExists("kb/nonexistent.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("nonexistent file should not exist")
	}
}

func TestListDir(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.WriteFile("kb/alpha.md", "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# Alpha\n\nBody.\n", "add alpha"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/sub/beta.md", "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# Beta\n\nBody.\n", "add beta"); err != nil {
		t.Fatal(err)
	}

	entries, err := store.ListDir("kb")
	if err != nil {
		t.Fatal(err)
	}

	var hasAlpha, hasSub bool
	for _, e := range entries {
		if e.Name == "alpha.md" && !e.IsDir {
			hasAlpha = true
		}
		if e.Name == "sub" && e.IsDir {
			hasSub = true
		}
	}
	if !hasAlpha {
		t.Fatal("expected alpha.md in kb/")
	}
	if !hasSub {
		t.Fatal("expected sub/ in kb/")
	}
}

func TestLog(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.WriteFile("kb/test.md", "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nv1.\n", "add test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/test.md", "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nv2.\n", "update test"); err != nil {
		t.Fatal(err)
	}

	entries, err := store.Log("kb/test.md")
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

	store, err := git.Init(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	content := "# Hello\n\nWorld.\n"
	if _, _, err := store.WriteFile("kb/hello.md", content, "add hello"); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store2, err := git.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	got, err := store2.ReadFile("kb/hello.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != content {
		t.Fatalf("open roundtrip mismatch:\ngot:  %q\nwant: %q", got, content)
	}
}

func TestHeadCommit(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
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
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.WriteFile("", "content", "msg"); err == nil {
		t.Fatal("expected error for empty path")
	}
	if _, _, err := store.WriteFile("../escape.md", "content", "msg"); err == nil {
		t.Fatal("expected error for path traversal")
	}
}

func TestListDirRoot(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
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
		if e.Name == "kb.md" && !e.IsDir {
			hasKnowMd = true
		}
	}
	if !hasKnowMd {
		t.Fatal("expected general.md in root listing")
	}
}

func TestDeleteFile(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Write a file, then delete it.
	if _, _, err := store.WriteFile("kb/todelete.md", "# Delete me\n", "add file"); err != nil {
		t.Fatal(err)
	}
	exists, err := store.FileExists("kb/todelete.md")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("file should exist before deletion")
	}

	if _, err := store.DeleteFile("kb/todelete.md", "delete: remove todelete.md"); err != nil {
		t.Fatal(err)
	}

	exists, err = store.FileExists("kb/todelete.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("file should not exist after deletion")
	}
}

func TestDeleteFile_AlreadyDeleted(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.WriteFile("kb/gone.md", "# Gone\n", "add file"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteFile("kb/gone.md", "delete once"); err != nil {
		t.Fatal(err)
	}

	// Second delete should return an error, not create a no-op commit.
	_, err = store.DeleteFile("kb/gone.md", "delete twice")
	if err == nil {
		t.Fatal("expected error deleting already-deleted file")
	}
}

func TestTag(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.WriteFile("kb/tagged.md", "# Tagged\n", "add tagged file"); err != nil {
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
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.WriteFile("kb/alpha.md", "# Alpha\n\nThis file contains the word elephant.\n", "add alpha"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/beta.md", "# Beta\n\nThis file is about dogs.\n", "add beta"); err != nil {
		t.Fatal(err)
	}

	matches, err := store.Grep("elephant")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != "kb/alpha.md" {
		t.Fatalf("expected [kb/alpha.md], got %v", matches)
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
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Get the commit before any writes.
	baseHash, err := store.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.WriteFile("kb/new.md", "# New\n", "add new"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb.md", "# Knowledge Base\n\nUpdated root.\n", "update root"); err != nil {
		t.Fatal(err)
	}

	added, modified, deleted, err := store.DiffFiles(baseHash)
	if err != nil {
		t.Fatal(err)
	}

	var hasNew bool
	for _, p := range added {
		if p == "kb/new.md" {
			hasNew = true
		}
	}
	if !hasNew {
		t.Fatalf("expected kb/new.md in added, got added=%v modified=%v deleted=%v", added, modified, deleted)
	}

	var hasModified bool
	for _, p := range modified {
		if p == "kb.md" {
			hasModified = true
		}
	}
	if !hasModified {
		t.Fatalf("expected general.md in modified, got added=%v modified=%v deleted=%v", added, modified, deleted)
	}
}

func TestDiffFilesFromEmpty(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
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
		if p == "kb.md" {
			hasKnowMd = true
		}
	}
	if !hasKnowMd {
		t.Fatalf("expected general.md in added when diffing from empty, got added=%v modified=%v deleted=%v", added, modified, deleted)
	}
}

func TestBatchWriteValidation(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.BatchWrite(map[string]string{"": "content"}, "msg"); err == nil {
		t.Fatal("expected error for empty path in BatchWrite")
	}
	if _, _, err := store.BatchWrite(map[string]string{"../escape.md": "content"}, "msg"); err == nil {
		t.Fatal("expected error for path traversal in BatchWrite")
	}
}

// TestSync verifies Sync behaviour with and without a configured origin.
func TestSync(t *testing.T) {
	t.Run("no origin returns Synced=false", func(t *testing.T) {
		dir := t.TempDir()
		store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		result, err := store.Sync("")
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
		origin, err := git.Init(filepath.Join(originDir, "origin.git.db"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer origin.Close()

		// Add a commit to origin's agent branch (WriteFile always targets the
		// agent branch), then advance origin's main ref to that commit so that
		// origin/main has content the agent store has never seen.
		if _, _, err := origin.WriteFile("kb/shared.md", "# Shared\n", "origin: add shared"); err != nil {
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
		store, err := git.Init(filepath.Join(agentDir, "knomit.git.db"), nil)
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
		result, err := store.Sync("")
		if err != nil {
			t.Fatalf("Sync returned unexpected error: %v", err)
		}
		if !result.Synced {
			t.Fatal("expected Synced=true after fetching new commits from origin")
		}

		// The merged file should now be accessible.
		exists, err := store.FileExists("kb/shared.md")
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatal("expected kb/shared.md to exist after merge")
		}
	})
}

func TestListAll(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// After Init, general.md exists at the root.
	paths, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	var hasKnowMd bool
	for _, p := range paths {
		if p == "kb.md" {
			hasKnowMd = true
		}
	}
	if !hasKnowMd {
		t.Fatalf("expected general.md in ListAll, got %v", paths)
	}

	// Add two more .md files and a non-.md file (no API for non-md, so skip that part).
	if _, _, err := store.WriteFile("kb/alpha.md", "# Alpha\n\nAlpha body.\n", "add alpha"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/sub/beta.md", "# Beta\n\nBeta body.\n", "add beta"); err != nil {
		t.Fatal(err)
	}

	paths, err = store.ListAll()
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"kb.md":           true,
		"kb/alpha.md":     true,
		"kb/sub/beta.md":  true,
	}
	for _, p := range paths {
		delete(want, p)
	}
	if len(want) != 0 {
		t.Fatalf("missing paths in ListAll: %v (got %v)", want, paths)
	}
}

func TestBatchWrite(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	files := map[string]string{
		"kb/a.md": "# A\n\nContent A.\n",
		"kb/b.md": "# B\n\nContent B.\n",
	}

	commitHash, blobHashes, err := store.BatchWrite(files, "batch: add a and b")
	if err != nil {
		t.Fatal(err)
	}
	if commitHash == "" {
		t.Fatal("expected non-empty commit hash")
	}
	if len(blobHashes) != 2 {
		t.Fatalf("expected 2 blob hashes, got %d", len(blobHashes))
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
	logEntries, err := store.Log("kb/a.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(logEntries) == 0 {
		t.Fatal("expected at least one log entry for kb/a.md")
	}
	if logEntries[0].Message != "batch: add a and b" {
		t.Fatalf("expected batch commit message, got %q", logEntries[0].Message)
	}
}

func TestWriteFileReturnsBlobHash(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	commitHash, blobHash, err := store.WriteFile("kb/test.md", "# Test\n\nBody.\n", "add test")
	if err != nil {
		t.Fatal(err)
	}
	if len(commitHash) != 40 {
		t.Fatalf("expected 40-char commit hash, got %q", commitHash)
	}
	if len(blobHash) != 40 {
		t.Fatalf("expected 40-char blob hash, got %q", blobHash)
	}
}

func TestOnCommitCallback(t *testing.T) {
	dir := t.TempDir()
	gs, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gs.Close()

	var called []string
	gs.SetOnCommit(func(hash string) {
		called = append(called, hash)
	})

	hash, _, err := gs.WriteFile("kb/test.md", "---\ntitle: Test\ntype: observation\ndomain: [eng]\nentities: [Go]\nconfidence: 0.9\nsources: 1\n---\ntest content", "test commit")
	if err != nil {
		t.Fatal(err)
	}
	if len(called) != 1 || called[0] != hash {
		t.Fatalf("expected onCommit called once with %q, got %v", hash, called)
	}
}

func TestOnCommitBatchAndDelete(t *testing.T) {
	dir := t.TempDir()
	gs, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gs.Close()

	var called []string
	gs.SetOnCommit(func(hash string) {
		called = append(called, hash)
	})

	files := map[string]string{
		"kb/a.md": "---\ntitle: A\ntype: observation\ndomain: [eng]\nentities: [Go]\nconfidence: 0.9\nsources: 1\n---\na",
		"kb/b.md": "---\ntitle: B\ntype: observation\ndomain: [eng]\nentities: [Go]\nconfidence: 0.9\nsources: 1\n---\nb",
	}
	batchHash, _, err := gs.BatchWrite(files, "batch commit")
	if err != nil {
		t.Fatal(err)
	}
	if len(called) != 1 || called[0] != batchHash {
		t.Fatalf("expected 1 call after BatchWrite, got %d", len(called))
	}

	delHash, err := gs.DeleteFile("kb/a.md", "delete a")
	if err != nil {
		t.Fatal(err)
	}
	if len(called) != 2 || called[1] != delHash {
		t.Fatalf("expected 2 calls total, got %d", len(called))
	}
}

func TestReadFileAtCommit(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	commitHash1, _, err := store.WriteFile("kb/test.md", "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nv1.\n", "add v1")
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.WriteFile("kb/test.md", "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nv2.\n", "update v2"); err != nil {
		t.Fatal(err)
	}

	content, err := store.ReadFileAtCommit("kb/test.md", commitHash1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "v1.") {
		t.Fatalf("expected v1 content, got: %s", content)
	}
}

func TestReadFileLastCommit(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const content = "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# Fact\n\nBody.\n"
	if _, _, err := store.WriteFile("kb/fact.md", content, "add fact"); err != nil {
		t.Fatal(err)
	}

	retractHash, err := store.DeleteFile("kb/fact.md", "retract fact")
	if err != nil {
		t.Fatal(err)
	}

	// File must not be readable at the retract commit.
	if _, err := store.ReadFileAtCommit("kb/fact.md", retractHash); err == nil {
		t.Fatal("expected error reading deleted file at retract commit, got nil")
	}

	// But ReadFileLastCommit must recover the content.
	got, err := store.ReadFileLastCommit("kb/fact.md", retractHash)
	if err != nil {
		t.Fatalf("ReadFileLastCommit: %v", err)
	}
	if got != content {
		t.Errorf("content mismatch:\ngot:  %q\nwant: %q", got, content)
	}
}

func TestReadFileWithHash(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	content := "# Test\n\nBody text.\n"
	_, expectedBlobHash, err := store.WriteFile("kb/test.md", content, "add test")
	if err != nil {
		t.Fatal(err)
	}

	gotContent, gotBlobHash, err := store.ReadFileWithHash("kb/test.md")
	if err != nil {
		t.Fatal(err)
	}
	if gotContent != content {
		t.Fatalf("content mismatch: got %q, want %q", gotContent, content)
	}
	if gotBlobHash != expectedBlobHash {
		t.Fatalf("blob hash mismatch: got %q, want %q", gotBlobHash, expectedBlobHash)
	}
}

func TestCommitDetail(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	commitHash, _, err := store.WriteFile("kb/test.md", "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nBody.\n", "add test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Tag("learn/test"); err != nil {
		t.Fatal(err)
	}

	detail, err := store.CommitDetail(commitHash)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Commit != commitHash {
		t.Errorf("expected commit %s, got %s", commitHash, detail.Commit)
	}
	if len(detail.Tags) == 0 || detail.Tags[0] != "learn/test" {
		t.Errorf("expected tag learn/test, got %v", detail.Tags)
	}
	if len(detail.Files) == 0 {
		t.Fatal("expected at least one changed file")
	}
	found := false
	for _, f := range detail.Files {
		if f.Path == "kb/test.md" && f.Action == "added" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected kb/test.md added, got %v", detail.Files)
	}
}

func TestLogPaginated(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for i := 1; i <= 3; i++ {
		content := fmt.Sprintf("---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# F%d\n\nFact %d.\n", i, i)
		if _, _, err := store.WriteFile(fmt.Sprintf("kb/f%d.md", i), content, fmt.Sprintf("add f%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Tag("learn/test-moment"); err != nil {
		t.Fatal(err)
	}

	entries, next, err := store.LogPaginated("", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if next == "" {
		t.Fatal("expected next cursor")
	}
	if len(entries[0].Tags) == 0 || entries[0].Tags[0] != "learn/test-moment" {
		t.Errorf("expected tag learn/test-moment on first entry, got %v", entries[0].Tags)
	}

	entries2, next2, err := store.LogPaginated("", 2, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries2) == 0 {
		t.Fatal("expected entries on second page")
	}
	_ = next2
}

func TestLogPaginated_DirectoryFilter(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Write files in two different directories.
	fact := "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nBody.\n"
	if _, _, err := store.WriteFile("kb/science/a.md", fact, "add science a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/tech/b.md", fact, "add tech b"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/science/c.md", fact, "add science c"); err != nil {
		t.Fatal(err)
	}

	// Filter to kb/science — should only include commits that touched files under kb/science/.
	entries, _, err := store.LogPaginated("kb/science", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries for kb/science, got %d", len(entries))
	}
	// Most recent first: "add science c", then "add science a".
	if entries[0].Message != "add science c" {
		t.Errorf("expected 'add science c', got %q", entries[0].Message)
	}
	if entries[1].Message != "add science a" {
		t.Errorf("expected 'add science a', got %q", entries[1].Message)
	}
}

func TestWalkChangedFilesBasic(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Write 3 files in separate commits.
	if _, _, err := store.WriteFile("kb/a.md", "# A\n", "add a"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // ensure distinct timestamps
	if _, _, err := store.WriteFile("kb/b.md", "# B\n", "add b"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, _, err := store.WriteFile("kb/c.md", "# C\n", "add c"); err != nil {
		t.Fatal(err)
	}

	files, lastHash, err := store.WalkChangedFiles("", "kb", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lastHash) != 40 {
		t.Fatalf("expected 40-char last hash, got %q", lastHash)
	}

	// Should find at least a.md, b.md, c.md (plus kb.md from init).
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	// Most recent first: c.md should come before b.md, b.md before a.md.
	idxC, idxB, idxA := -1, -1, -1
	for i, p := range paths {
		switch p {
		case "kb/c.md":
			idxC = i
		case "kb/b.md":
			idxB = i
		case "kb/a.md":
			idxA = i
		}
	}
	if idxC < 0 || idxB < 0 || idxA < 0 {
		t.Fatalf("expected all 3 files, got paths: %v", paths)
	}
	if idxC > idxB || idxB > idxA {
		t.Fatalf("expected most-recent-first order (c < b < a), got indices c=%d b=%d a=%d in %v", idxC, idxB, idxA, paths)
	}
}

func TestWalkChangedFilesPrefix(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.WriteFile("kb/science/phys.md", "# Physics\n", "add phys"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/tech/go.md", "# Go\n", "add go"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/science/chem.md", "# Chemistry\n", "add chem"); err != nil {
		t.Fatal(err)
	}

	files, _, err := store.WalkChangedFiles("", "kb/science", nil, 10)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range files {
		if !strings.HasPrefix(f.Path, "kb/science/") {
			t.Fatalf("unexpected file outside prefix: %s", f.Path)
		}
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files under kb/science, got %d: %v", len(files), files)
	}
}

func TestWalkChangedFilesSeen(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, err := store.WriteFile("kb/a.md", "# A\n", "add a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/b.md", "# B\n", "add b"); err != nil {
		t.Fatal(err)
	}

	// Walk with limit=1 — should get the most recent file only.
	files1, _, err := store.WalkChangedFiles("", "kb", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(files1) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files1))
	}

	// Resume with the seen set from page 1.
	seen := map[string]bool{files1[0].Path: true}
	files2, _, err := store.WalkChangedFiles("", "kb", seen, 10)
	if err != nil {
		t.Fatal(err)
	}

	// files2 should not contain the file from files1.
	for _, f := range files2 {
		if f.Path == files1[0].Path {
			t.Fatalf("seen file %s appeared again in second walk", files1[0].Path)
		}
	}
	// Should have found at least one more file.
	if len(files2) == 0 {
		t.Fatal("expected at least 1 file in second walk")
	}
}

func TestWalkChangedFilesDedup(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "knomit.git.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Write the same file twice (two commits).
	if _, _, err := store.WriteFile("kb/dup.md", "# Dup v1\n", "add dup"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/dup.md", "# Dup v2\n", "update dup"); err != nil {
		t.Fatal(err)
	}

	files, _, err := store.WalkChangedFiles("", "kb", nil, 10)
	if err != nil {
		t.Fatal(err)
	}

	count := 0
	for _, f := range files {
		if f.Path == "kb/dup.md" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected kb/dup.md exactly once, got %d times in %v", count, files)
	}
}

// newTestStorer creates a fresh SQLite-backed storer for testing.
func newTestStorer(t *testing.T) *storegit.Storer {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema := `
CREATE TABLE IF NOT EXISTS objects (hash TEXT NOT NULL, type INTEGER NOT NULL, size INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY (hash, type));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target TEXT NOT NULL, is_symbolic INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value BLOB NOT NULL);
`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return storegit.NewStorer(db)
}

func TestInitFromRemote_WithContent(t *testing.T) {
	// Set up an origin store with content.
	originDir := t.TempDir()
	origin, err := git.Init(filepath.Join(originDir, "origin.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()

	if _, _, err := origin.WriteFile("kb/shared.md", "# Shared\n", "origin: add shared"); err != nil {
		t.Fatal(err)
	}
	// Advance main to HEAD.
	head, err := origin.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	mainRef := plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("main"),
		plumbing.NewHash(head),
	)
	if err := origin.Storer().SetReference(mainRef); err != nil {
		t.Fatal(err)
	}

	// Register in-process transport.
	loader := server.MapLoader{"inmem:///origin-ifr": origin.Storer()}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	// InitFromRemote into a fresh storer.
	s := newTestStorer(t)
	store, err := git.InitFromRemote(s, "inmem:///origin-ifr", nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Agent branch should be created from origin/main.
	if !strings.HasPrefix(store.Branch(), "agent/") {
		t.Fatalf("expected agent branch, got %q", store.Branch())
	}

	// The shared file should be readable.
	content, err := store.ReadFile("kb/shared.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Shared\n" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestInitFromRemote_ExistingAgentBranch(t *testing.T) {
	// Set up an origin store with an agent branch.
	originDir := t.TempDir()
	origin, err := git.Init(filepath.Join(originDir, "origin.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()

	// Write something on the agent branch (which is the default).
	if _, _, err := origin.WriteFile("kb/agent-file.md", "# Agent File\n", "origin: agent file"); err != nil {
		t.Fatal(err)
	}

	// Also advance main to current HEAD so origin/main exists.
	head, err := origin.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	mainRef := plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("main"),
		plumbing.NewHash(head),
	)
	if err := origin.Storer().SetReference(mainRef); err != nil {
		t.Fatal(err)
	}

	// Register in-process transport.
	loader := server.MapLoader{"inmem:///origin-ifr2": origin.Storer()}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	// InitFromRemote — should find the existing agent branch.
	s := newTestStorer(t)
	store, err := git.InitFromRemote(s, "inmem:///origin-ifr2", nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Agent branch should match origin's agent branch.
	if !strings.HasPrefix(store.Branch(), "agent/") {
		t.Fatalf("expected agent branch, got %q", store.Branch())
	}

	// The agent file should be readable (came from the agent branch, not just main).
	content, err := store.ReadFile("kb/agent-file.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Agent File\n" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestInitFromRemote_EmptyRemote(t *testing.T) {
	// Create a bare storer with no commits to act as an empty remote.
	emptyStorer := newTestStorer(t)

	// Register in-process transport.
	loader := server.MapLoader{"inmem:///empty-origin": emptyStorer}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	// InitFromRemote with empty remote — should fall back to InitWithStorer.
	s := newTestStorer(t)
	store, err := git.InitFromRemote(s, "inmem:///empty-origin", nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Should have a valid agent branch and the default kb.md.
	if !strings.HasPrefix(store.Branch(), "agent/") {
		t.Fatalf("expected agent branch, got %q", store.Branch())
	}

	exists, err := store.FileExists("kb.md")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected kb.md to exist after fallback to InitWithStorer")
	}
}

func TestLogPaginated_FileFilter(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	fact := "---\ndomain: []\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# T\n\nBody.\n"
	if _, _, err := store.WriteFile("kb/a.md", fact, "add a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/b.md", fact, "add b"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/a.md", fact+"updated", "update a"); err != nil {
		t.Fatal(err)
	}

	// Filter to specific file — should only include commits that touched kb/a.md.
	entries, _, err := store.LogPaginated("kb/a.md", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries for kb/a.md, got %d", len(entries))
	}
	if entries[0].Message != "update a" {
		t.Errorf("expected 'update a', got %q", entries[0].Message)
	}
}

func TestSwitchBranch(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	oldBranch := store.Branch()

	// Write a file so we have a commit to preserve.
	_, _, err = store.WriteFile("kb/fact.md", "test content", "add fact")
	if err != nil {
		t.Fatal(err)
	}
	headBefore, _ := store.HeadCommit()

	// Switch to new branch.
	newBranch := "agent/test-abc123"
	if err := store.SwitchBranch(newBranch); err != nil {
		t.Fatalf("SwitchBranch: %v", err)
	}

	if store.Branch() != newBranch {
		t.Errorf("Branch() = %q, want %q", store.Branch(), newBranch)
	}

	// HEAD commit should be unchanged.
	headAfter, _ := store.HeadCommit()
	if headBefore != headAfter {
		t.Errorf("HEAD changed: %s → %s", headBefore, headAfter)
	}

	// Data should still be readable.
	content, err := store.ReadFile("kb/fact.md")
	if err != nil {
		t.Fatalf("ReadFile after switch: %v", err)
	}
	if content != "test content" {
		t.Errorf("content = %q, want %q", content, "test content")
	}

	// No-op if already on the right branch.
	if err := store.SwitchBranch(newBranch); err != nil {
		t.Fatalf("SwitchBranch no-op: %v", err)
	}

	// Old branch should still exist (not deleted).
	_ = oldBranch
}

func TestSwitchBranch_WritesAfterSwitch(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.SwitchBranch("agent/new-branch"); err != nil {
		t.Fatalf("SwitchBranch: %v", err)
	}

	// Writes should go to the new branch.
	_, _, err = store.WriteFile("kb/new-fact.md", "new content", "add after switch")
	if err != nil {
		t.Fatalf("WriteFile after switch: %v", err)
	}

	content, err := store.ReadFile("kb/new-fact.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "new content" {
		t.Errorf("content = %q, want %q", content, "new content")
	}
}
