package git_test

import (
	"path/filepath"
	"testing"

	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"

	git "knomit/internal/git"
	storegit "knomit/internal/store/git"
)

// setupOriginAndAgent creates two knomit stores (origin, agent) with a
// configured remote and shared init commit. Uses in-memory transport for
// the initial sync, then switches to direct object copying for tests
// that involve diverged histories (go-git's in-memory transport can't
// handle diverged fetch negotiation).
func setupOriginAndAgent(t *testing.T) (origin, agent *git.Store) {
	t.Helper()

	originDir := t.TempDir()
	origin, err := git.Init(filepath.Join(originDir, "origin.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { origin.Close() })

	// Point origin's main at HEAD.
	syncMainToHead(t, origin)

	// Register in-process transport for initial sync.
	loader := server.MapLoader{"inmem:///origin": origin.Storer()}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	// Create agent store.
	agentDir := t.TempDir()
	agent, err = git.Init(filepath.Join(agentDir, "agent.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Close() })

	// Configure origin remote on agent.
	cfg, err := agent.Storer().Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Remotes["origin"] = &gogitconfig.RemoteConfig{
		Name:  "origin",
		URLs:  []string{"inmem:///origin"},
		Fetch: []gogitconfig.RefSpec{"+refs/heads/*:refs/remotes/origin/*"},
	}
	if err := agent.Storer().SetConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Initial sync so agent has origin/main.
	if _, err := agent.Sync(""); err != nil {
		t.Fatal(err)
	}

	return origin, agent
}

// syncMainToHead sets origin's main branch to its current HEAD.
func syncMainToHead(t *testing.T, s *git.Store) {
	t.Helper()
	head, err := s.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	ref := plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("main"),
		plumbing.NewHash(head),
	)
	if err := s.Storer().SetReference(ref); err != nil {
		t.Fatal(err)
	}
}

// advanceOriginMain writes files to origin then advances main.
func advanceOriginMain(t *testing.T, origin *git.Store, files map[string]string) {
	t.Helper()
	for path, content := range files {
		if _, _, err := origin.WriteFile(path, content, "origin: "+path); err != nil {
			t.Fatal(err)
		}
	}
	syncMainToHead(t, origin)
}

// deleteOnOriginMain deletes files from origin then advances main.
func deleteOnOriginMain(t *testing.T, origin *git.Store, paths []string) {
	t.Helper()
	for _, path := range paths {
		if _, err := origin.DeleteFile(path, "origin: delete "+path); err != nil {
			t.Fatal(err)
		}
	}
	syncMainToHead(t, origin)
}

// simulateFetch copies all objects from origin's storer to agent's storer
// and updates the remote tracking ref. This bypasses the git transport
// protocol which can't handle diverged histories in go-git's in-memory server.
func simulateFetch(t *testing.T, origin, agent *git.Store) {
	t.Helper()
	copyObjects(t, origin.Storer(), agent.Storer())
	// Update refs/remotes/origin/main in agent to origin's main hash.
	mainRef, err := origin.Storer().Reference(plumbing.NewBranchReferenceName("main"))
	if err != nil {
		t.Fatal(err)
	}
	remoteRef := plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", "main"),
		mainRef.Hash(),
	)
	if err := agent.Storer().SetReference(remoteRef); err != nil {
		t.Fatal(err)
	}
}

// copyObjects copies all objects from src to dst storer.
func copyObjects(t *testing.T, src, dst *storegit.Storer) {
	t.Helper()
	iter, err := src.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		t.Fatal(err)
	}
	err = iter.ForEach(func(obj plumbing.EncodedObject) error {
		_, err := dst.SetEncodedObject(obj)
		return err
	})
	if err != nil && err != storer.ErrStop {
		t.Fatal(err)
	}
}

// syncWithSimulatedFetch simulates a fetch then calls Sync with an empty
// remote branch (which skips the fetch since origin/main is already updated).
// This is used for tests with diverged histories.
func syncWithSimulatedFetch(t *testing.T, origin, agent *git.Store) git.SyncResult {
	t.Helper()
	simulateFetch(t, origin, agent)
	result, err := agent.Sync("")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSyncThreeWay_AddedFile(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	advanceOriginMain(t, origin, map[string]string{
		"kb/new-from-origin.md": "# New\nFrom origin.\n",
	})

	result, err := agent.Sync("")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	content, err := agent.ReadFile("kb/new-from-origin.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# New\nFrom origin.\n" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestSyncThreeWay_ModifiedFile(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	advanceOriginMain(t, origin, map[string]string{
		"kb/shared.md": "# Shared v1\n",
	})
	if _, err := agent.Sync(""); err != nil {
		t.Fatal(err)
	}

	advanceOriginMain(t, origin, map[string]string{
		"kb/shared.md": "# Shared v2 (origin)\n",
	})

	result, err := agent.Sync("")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	content, err := agent.ReadFile("kb/shared.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Shared v2 (origin)\n" {
		t.Fatalf("expected origin version, got: %q", content)
	}
}

func TestSyncThreeWay_DeletedFile(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	advanceOriginMain(t, origin, map[string]string{
		"kb/to-delete.md": "# Will be deleted\n",
	})
	if _, err := agent.Sync(""); err != nil {
		t.Fatal(err)
	}

	exists, err := agent.FileExists("kb/to-delete.md")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected kb/to-delete.md to exist before delete")
	}

	deleteOnOriginMain(t, origin, []string{"kb/to-delete.md"})

	result, err := agent.Sync("")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	exists, err = agent.FileExists("kb/to-delete.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected kb/to-delete.md to be deleted after sync")
	}
}

func TestSyncThreeWay_AgentOnlyFilePreserved(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	// Agent adds its own file (diverges from origin).
	if _, _, err := agent.WriteFile("kb/agent-only.md", "# Agent Only\n", "agent: add local"); err != nil {
		t.Fatal(err)
	}

	// Origin adds a different file.
	advanceOriginMain(t, origin, map[string]string{
		"kb/origin-file.md": "# Origin\n",
	})

	// Use simulated fetch since histories have diverged.
	result := syncWithSimulatedFetch(t, origin, agent)
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	// Agent-only file should still exist.
	content, err := agent.ReadFile("kb/agent-only.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Agent Only\n" {
		t.Fatalf("agent-only file modified: %q", content)
	}

	// Origin's file should also exist.
	if _, err := agent.ReadFile("kb/origin-file.md"); err != nil {
		t.Fatal(err)
	}
}

func TestSyncThreeWay_OriginOverwritesAgentChange(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	// Both have a shared file.
	advanceOriginMain(t, origin, map[string]string{
		"kb/contested.md": "# Base version\n",
	})
	if _, err := agent.Sync(""); err != nil {
		t.Fatal(err)
	}

	// Agent modifies the file (diverges).
	if _, _, err := agent.WriteFile("kb/contested.md", "# Agent version\n", "agent: modify"); err != nil {
		t.Fatal(err)
	}

	// Origin also modifies the file.
	advanceOriginMain(t, origin, map[string]string{
		"kb/contested.md": "# Origin version\n",
	})

	// Use simulated fetch since histories have diverged.
	result := syncWithSimulatedFetch(t, origin, agent)
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	// Origin wins.
	content, err := agent.ReadFile("kb/contested.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Origin version\n" {
		t.Fatalf("expected origin version, got: %q", content)
	}
}

func TestSyncFastForward(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	advanceOriginMain(t, origin, map[string]string{
		"kb/ff-file.md": "# Fast Forward\n",
	})

	result, err := agent.Sync("")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}
	if !result.FastForward {
		t.Fatal("expected FastForward=true when agent has no divergent commits")
	}
	if result.MergeCommit != "" {
		t.Fatalf("expected empty MergeCommit for fast-forward, got: %s", result.MergeCommit)
	}
}

func TestSyncNoOp(t *testing.T) {
	_, agent := setupOriginAndAgent(t)

	result, err := agent.Sync("")
	if err != nil {
		t.Fatal(err)
	}
	if result.Synced {
		t.Fatal("expected Synced=false for no-op sync")
	}
}

func TestSyncAlreadyMerged(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	advanceOriginMain(t, origin, map[string]string{
		"kb/merged.md": "# Merged\n",
	})
	if _, err := agent.Sync(""); err != nil {
		t.Fatal(err)
	}

	result, err := agent.Sync("")
	if err != nil {
		t.Fatal(err)
	}
	if result.Synced {
		t.Fatal("expected Synced=false when origin is already merged")
	}
}

func TestSyncThreeWay_MergeCommitHasTwoParents(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	// Agent diverges.
	if _, _, err := agent.WriteFile("kb/agent-file.md", "# Agent\n", "agent: diverge"); err != nil {
		t.Fatal(err)
	}

	// Origin adds a file.
	advanceOriginMain(t, origin, map[string]string{
		"kb/origin-file.md": "# Origin\n",
	})

	result := syncWithSimulatedFetch(t, origin, agent)
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}
	if result.FastForward {
		t.Fatal("expected merge, not fast-forward when both sides diverged")
	}
	if result.MergeCommit == "" {
		t.Fatal("expected non-empty MergeCommit for three-way merge")
	}
}

func TestSyncThreeWay_SubsequentMergesWork(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	// First divergence + merge.
	if _, _, err := agent.WriteFile("kb/a.md", "# A\n", "agent: a"); err != nil {
		t.Fatal(err)
	}
	advanceOriginMain(t, origin, map[string]string{
		"kb/b.md": "# B\n",
	})
	result := syncWithSimulatedFetch(t, origin, agent)
	if !result.Synced {
		t.Fatal("expected Synced=true on first merge")
	}

	// Second divergence + merge.
	if _, _, err := agent.WriteFile("kb/c.md", "# C\n", "agent: c"); err != nil {
		t.Fatal(err)
	}
	advanceOriginMain(t, origin, map[string]string{
		"kb/d.md": "# D\n",
	})

	result = syncWithSimulatedFetch(t, origin, agent)
	if !result.Synced {
		t.Fatal("expected Synced=true on second merge")
	}

	// All four files should exist.
	for _, path := range []string{"kb/a.md", "kb/b.md", "kb/c.md", "kb/d.md"} {
		exists, err := agent.FileExists(path)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("expected %s to exist after second merge", path)
		}
	}
}

func TestSyncThreeWay_DeletedFileAgentModified(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	// Both have a shared file.
	advanceOriginMain(t, origin, map[string]string{
		"kb/will-delete.md": "# Base\n",
	})
	if _, err := agent.Sync(""); err != nil {
		t.Fatal(err)
	}

	// Agent modifies the file.
	if _, _, err := agent.WriteFile("kb/will-delete.md", "# Agent modified\n", "agent: modify"); err != nil {
		t.Fatal(err)
	}

	// Origin deletes it.
	deleteOnOriginMain(t, origin, []string{"kb/will-delete.md"})

	result := syncWithSimulatedFetch(t, origin, agent)
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	// Origin wins — file should be deleted even though agent modified it.
	exists, err := agent.FileExists("kb/will-delete.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected kb/will-delete.md to be deleted (origin wins)")
	}
}

func TestSyncNoRemote(t *testing.T) {
	dir := t.TempDir()
	s, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// No remote configured — Sync should return empty result, no error.
	result, err := s.Sync("")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.Synced {
		t.Fatal("expected Synced=false with no remote")
	}
	if result.FastForward {
		t.Fatal("expected FastForward=false with no remote")
	}
	if result.MergeCommit != "" {
		t.Fatalf("expected empty MergeCommit, got: %s", result.MergeCommit)
	}
}

func TestSyncDefaultRemoteBranch(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	// Advance origin/main.
	advanceOriginMain(t, origin, map[string]string{
		"kb/default-branch.md": "# Default branch test\n",
	})

	// Call Sync with empty string — should default to "main".
	result, err := agent.Sync("")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Synced {
		t.Fatal("expected Synced=true when using default remoteBranch")
	}

	// Verify the file arrived.
	content, err := agent.ReadFile("kb/default-branch.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Default branch test\n" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestSyncOriginRefNotFoundAfterFetch(t *testing.T) {
	origin, agent := setupOriginAndAgent(t)

	// Reconfigure agent to fetch a branch that doesn't exist on origin.
	cfg, err := agent.Storer().Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Remotes["origin"] = &gogitconfig.RemoteConfig{
		Name:  "origin",
		URLs:  []string{"inmem:///origin"},
		Fetch: []gogitconfig.RefSpec{"+refs/heads/*:refs/remotes/origin/*"},
	}
	if err := agent.Storer().SetConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Advance origin on main (not on "nonexistent").
	advanceOriginMain(t, origin, map[string]string{
		"kb/x.md": "# X\n",
	})

	// Sync looking for "nonexistent" branch — origin/nonexistent won't exist.
	result, err := agent.Sync("nonexistent")
	if err != nil {
		t.Fatalf("expected no error for missing origin ref, got: %v", err)
	}
	if result.Synced {
		t.Fatal("expected Synced=false when origin ref not found")
	}
}

func TestConfigureRemote(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.ConfigureRemote("https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}

	// Idempotent.
	if err := store.ConfigureRemote("https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}

	// Different URL replaces.
	if err := store.ConfigureRemote("https://example.com/other.git", "master"); err != nil {
		t.Fatal(err)
	}
}

func TestPush(t *testing.T) {
	t.Run("no remote returns Pushed=false", func(t *testing.T) {
		dir := t.TempDir()
		s, err := git.Init(filepath.Join(dir, "test.db"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		result, err := s.Push()
		if err != nil {
			t.Fatal(err)
		}
		if result.Pushed {
			t.Fatal("expected Pushed=false with no remote")
		}
	})

	t.Run("pushes agent branch to origin", func(t *testing.T) {
		origin, agent := setupOriginAndAgent(t)

		// Agent writes a file.
		if _, _, err := agent.WriteFile("kb/agent-push.md", "# Push test\n", "agent: push test"); err != nil {
			t.Fatal(err)
		}

		// Push agent branch to origin.
		result, err := agent.Push()
		if err != nil {
			t.Fatal(err)
		}
		if !result.Pushed {
			t.Fatal("expected Pushed=true")
		}

		// Verify origin has the agent branch with the file.
		// The agent branch on origin should now have the pushed content.
		agentBranch := agent.Branch()
		agentRef, err := origin.Storer().Reference(plumbing.NewBranchReferenceName(agentBranch))
		if err != nil {
			t.Fatalf("expected agent branch on origin, got: %v", err)
		}
		if agentRef.Hash().IsZero() {
			t.Fatal("expected non-zero hash for agent branch on origin")
		}
	})

	t.Run("already up to date returns Pushed=false", func(t *testing.T) {
		_, agent := setupOriginAndAgent(t)

		// Push with no local changes (initial push).
		result, err := agent.Push()
		if err != nil {
			t.Fatal(err)
		}

		// Second push should be no-op.
		result, err = agent.Push()
		if err != nil {
			t.Fatal(err)
		}
		if result.Pushed {
			t.Fatal("expected Pushed=false when already up to date")
		}
	})
}
