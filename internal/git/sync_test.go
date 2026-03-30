package git_test

import (
	"context"
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
func setupOriginAndAgent(t *testing.T) (origin, agent *git.Store, originSto, agentSto *storegit.Storer) {
	t.Helper()

	originSto = newTestStorer(t)
	var err error
	origin, err = git.InitWithStorer(originSto, nil, testBranch)
	if err != nil {
		t.Fatal(err)
	}

	// Point origin's main at HEAD.
	syncMainToHead(t, origin, originSto)

	// Register in-process transport for initial sync.
	loader := server.MapLoader{"inmem:///origin": originSto}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	// Create agent store.
	agentSto = newTestStorer(t)
	agent, err = git.InitWithStorer(agentSto, nil, testBranch)
	if err != nil {
		t.Fatal(err)
	}

	// Configure origin remote on agent.
	cfg, err := agentSto.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Remotes["origin"] = &gogitconfig.RemoteConfig{
		Name:  "origin",
		URLs:  []string{"inmem:///origin"},
		Fetch: []gogitconfig.RefSpec{"+refs/heads/*:refs/remotes/origin/*"},
	}
	if err := agentSto.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Initial sync so agent has origin/main.
	if _, err := agent.Sync(context.Background(), testBranch, ""); err != nil {
		t.Fatal(err)
	}

	return origin, agent, originSto, agentSto
}

// syncMainToHead sets a store's main branch ref to its current HEAD.
func syncMainToHead(t *testing.T, s *git.Store, sto *storegit.Storer) {
	t.Helper()
	head, err := s.HeadCommit(context.Background(), testBranch)
	if err != nil {
		t.Fatal(err)
	}
	ref := plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("main"),
		plumbing.NewHash(head),
	)
	if err := sto.SetReference(ref); err != nil {
		t.Fatal(err)
	}
}

// advanceOriginMain writes files to origin then advances main.
func advanceOriginMain(t *testing.T, origin *git.Store, originSto *storegit.Storer, files map[string]string) {
	t.Helper()
	for path, content := range files {
		if _, _, err := origin.WriteFile(context.Background(), testBranch, path, content, "origin: "+path, "learn"); err != nil {
			t.Fatal(err)
		}
	}
	syncMainToHead(t, origin, originSto)
}

// deleteOnOriginMain deletes files from origin then advances main.
func deleteOnOriginMain(t *testing.T, origin *git.Store, originSto *storegit.Storer, paths []string) {
	t.Helper()
	for _, path := range paths {
		if _, err := origin.DeleteFile(context.Background(), testBranch, path, "origin: delete "+path, "retract"); err != nil {
			t.Fatal(err)
		}
	}
	syncMainToHead(t, origin, originSto)
}

// simulateFetch copies all objects from origin's storer to agent's storer
// and updates the remote tracking ref. This bypasses the git transport
// protocol which can't handle diverged histories in go-git's in-memory server.
func simulateFetch(t *testing.T, originSto, agentSto *storegit.Storer) {
	t.Helper()
	copyObjects(t, originSto, agentSto)
	// Update refs/remotes/origin/main in agent to origin's main hash.
	mainRef, err := originSto.Reference(plumbing.NewBranchReferenceName("main"))
	if err != nil {
		t.Fatal(err)
	}
	remoteRef := plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", "main"),
		mainRef.Hash(),
	)
	if err := agentSto.SetReference(remoteRef); err != nil {
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
func syncWithSimulatedFetch(t *testing.T, agent *git.Store, originSto, agentSto *storegit.Storer) git.SyncResult {
	t.Helper()
	simulateFetch(t, originSto, agentSto)
	result, err := agent.Sync(context.Background(), testBranch, "")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSyncThreeWay_AddedFile(t *testing.T) {
	origin, agent, originSto, _ := setupOriginAndAgent(t)

	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/new-from-origin.md": "# New\nFrom origin.\n",
	})

	result, err := agent.Sync(context.Background(), testBranch, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	content, err := agent.ReadFile(context.Background(), testBranch, "kb/new-from-origin.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# New\nFrom origin.\n" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestSyncThreeWay_ModifiedFile(t *testing.T) {
	origin, agent, originSto, _ := setupOriginAndAgent(t)

	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/shared.md": "# Shared v1\n",
	})
	if _, err := agent.Sync(context.Background(), testBranch, ""); err != nil {
		t.Fatal(err)
	}

	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/shared.md": "# Shared v2 (origin)\n",
	})

	result, err := agent.Sync(context.Background(), testBranch, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	content, err := agent.ReadFile(context.Background(), testBranch, "kb/shared.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Shared v2 (origin)\n" {
		t.Fatalf("expected origin version, got: %q", content)
	}
}

func TestSyncThreeWay_DeletedFile(t *testing.T) {
	origin, agent, originSto, _ := setupOriginAndAgent(t)

	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/to-delete.md": "# Will be deleted\n",
	})
	if _, err := agent.Sync(context.Background(), testBranch, ""); err != nil {
		t.Fatal(err)
	}

	exists, err := agent.FileExists(context.Background(), testBranch, "kb/to-delete.md")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected kb/to-delete.md to exist before delete")
	}

	deleteOnOriginMain(t, origin, originSto, []string{"kb/to-delete.md"})

	result, err := agent.Sync(context.Background(), testBranch, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	exists, err = agent.FileExists(context.Background(), testBranch, "kb/to-delete.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected kb/to-delete.md to be deleted after sync")
	}
}

func TestSyncThreeWay_AgentOnlyFilePreserved(t *testing.T) {
	origin, agent, originSto, agentSto := setupOriginAndAgent(t)

	// Agent adds its own file (diverges from origin).
	if _, _, err := agent.WriteFile(context.Background(), testBranch, "kb/agent-only.md", "# Agent Only\n", "agent: add local", "learn"); err != nil {
		t.Fatal(err)
	}

	// Origin adds a different file.
	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/origin-file.md": "# Origin\n",
	})

	// Use simulated fetch since histories have diverged.
	result := syncWithSimulatedFetch(t, agent, originSto, agentSto)
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	// Agent-only file should still exist.
	content, err := agent.ReadFile(context.Background(), testBranch, "kb/agent-only.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Agent Only\n" {
		t.Fatalf("agent-only file modified: %q", content)
	}

	// Origin's file should also exist.
	if _, err := agent.ReadFile(context.Background(), testBranch, "kb/origin-file.md"); err != nil {
		t.Fatal(err)
	}
}

func TestSyncThreeWay_OriginOverwritesAgentChange(t *testing.T) {
	origin, agent, originSto, agentSto := setupOriginAndAgent(t)

	// Both have a shared file.
	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/contested.md": "# Base version\n",
	})
	if _, err := agent.Sync(context.Background(), testBranch, ""); err != nil {
		t.Fatal(err)
	}

	// Agent modifies the file (diverges).
	if _, _, err := agent.WriteFile(context.Background(), testBranch, "kb/contested.md", "# Agent version\n", "agent: modify", "learn"); err != nil {
		t.Fatal(err)
	}

	// Origin also modifies the file.
	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/contested.md": "# Origin version\n",
	})

	// Use simulated fetch since histories have diverged.
	result := syncWithSimulatedFetch(t, agent, originSto, agentSto)
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	// Origin wins.
	content, err := agent.ReadFile(context.Background(), testBranch, "kb/contested.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Origin version\n" {
		t.Fatalf("expected origin version, got: %q", content)
	}
}

func TestSyncFastForward(t *testing.T) {
	origin, agent, originSto, _ := setupOriginAndAgent(t)

	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/ff-file.md": "# Fast Forward\n",
	})

	result, err := agent.Sync(context.Background(), testBranch, "")
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
	_, agent, _, _ := setupOriginAndAgent(t)

	result, err := agent.Sync(context.Background(), testBranch, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Synced {
		t.Fatal("expected Synced=false for no-op sync")
	}
}

func TestSyncAlreadyMerged(t *testing.T) {
	origin, agent, originSto, _ := setupOriginAndAgent(t)

	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/merged.md": "# Merged\n",
	})
	if _, err := agent.Sync(context.Background(), testBranch, ""); err != nil {
		t.Fatal(err)
	}

	result, err := agent.Sync(context.Background(), testBranch, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Synced {
		t.Fatal("expected Synced=false when origin is already merged")
	}
}

func TestSyncThreeWay_MergeCommitHasTwoParents(t *testing.T) {
	origin, agent, originSto, agentSto := setupOriginAndAgent(t)

	// Agent diverges.
	if _, _, err := agent.WriteFile(context.Background(), testBranch, "kb/agent-file.md", "# Agent\n", "agent: diverge", "learn"); err != nil {
		t.Fatal(err)
	}

	// Origin adds a file.
	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/origin-file.md": "# Origin\n",
	})

	result := syncWithSimulatedFetch(t, agent, originSto, agentSto)
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
	origin, agent, originSto, agentSto := setupOriginAndAgent(t)

	// First divergence + merge.
	if _, _, err := agent.WriteFile(context.Background(), testBranch, "kb/a.md", "# A\n", "agent: a", "learn"); err != nil {
		t.Fatal(err)
	}
	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/b.md": "# B\n",
	})
	result := syncWithSimulatedFetch(t, agent, originSto, agentSto)
	if !result.Synced {
		t.Fatal("expected Synced=true on first merge")
	}

	// Second divergence + merge.
	if _, _, err := agent.WriteFile(context.Background(), testBranch, "kb/c.md", "# C\n", "agent: c", "learn"); err != nil {
		t.Fatal(err)
	}
	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/d.md": "# D\n",
	})

	result = syncWithSimulatedFetch(t, agent, originSto, agentSto)
	if !result.Synced {
		t.Fatal("expected Synced=true on second merge")
	}

	// All four files should exist.
	for _, path := range []string{"kb/a.md", "kb/b.md", "kb/c.md", "kb/d.md"} {
		exists, err := agent.FileExists(context.Background(), testBranch, path)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("expected %s to exist after second merge", path)
		}
	}
}

func TestSyncThreeWay_DeletedFileAgentModified(t *testing.T) {
	origin, agent, originSto, agentSto := setupOriginAndAgent(t)

	// Both have a shared file.
	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/will-delete.md": "# Base\n",
	})
	if _, err := agent.Sync(context.Background(), testBranch, ""); err != nil {
		t.Fatal(err)
	}

	// Agent modifies the file.
	if _, _, err := agent.WriteFile(context.Background(), testBranch, "kb/will-delete.md", "# Agent modified\n", "agent: modify", "learn"); err != nil {
		t.Fatal(err)
	}

	// Origin deletes it.
	deleteOnOriginMain(t, origin, originSto, []string{"kb/will-delete.md"})

	result := syncWithSimulatedFetch(t, agent, originSto, agentSto)
	if !result.Synced {
		t.Fatal("expected Synced=true")
	}

	// Origin wins — file should be deleted even though agent modified it.
	exists, err := agent.FileExists(context.Background(), testBranch, "kb/will-delete.md")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected kb/will-delete.md to be deleted (origin wins)")
	}
}

func TestSyncNoRemote(t *testing.T) {
	s := newTestStore(t)

	// No remote configured — Sync should return empty result, no error.
	result, err := s.Sync(context.Background(), testBranch, "")
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
	origin, agent, originSto, _ := setupOriginAndAgent(t)

	// Advance origin/main.
	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/default-branch.md": "# Default branch test\n",
	})

	// Call Sync with empty string — should default to "main".
	result, err := agent.Sync(context.Background(), testBranch, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Synced {
		t.Fatal("expected Synced=true when using default remoteBranch")
	}

	// Verify the file arrived.
	content, err := agent.ReadFile(context.Background(), testBranch, "kb/default-branch.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Default branch test\n" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestSyncOriginRefNotFoundAfterFetch(t *testing.T) {
	origin, agent, originSto, agentSto := setupOriginAndAgent(t)

	// Reconfigure agent to fetch a branch that doesn't exist on origin.
	cfg, err := agentSto.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Remotes["origin"] = &gogitconfig.RemoteConfig{
		Name:  "origin",
		URLs:  []string{"inmem:///origin"},
		Fetch: []gogitconfig.RefSpec{"+refs/heads/*:refs/remotes/origin/*"},
	}
	if err := agentSto.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Advance origin on main (not on "nonexistent").
	advanceOriginMain(t, origin, originSto, map[string]string{
		"kb/x.md": "# X\n",
	})

	// Sync looking for "nonexistent" branch — origin/nonexistent won't exist.
	result, err := agent.Sync(context.Background(), testBranch, "nonexistent")
	if err != nil {
		t.Fatalf("expected no error for missing origin ref, got: %v", err)
	}
	if result.Synced {
		t.Fatal("expected Synced=false when origin ref not found")
	}
}

func TestConfigureRemote(t *testing.T) {
	store := newTestStore(t)

	if err := store.ConfigureRemote(context.Background(), "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}

	// Idempotent.
	if err := store.ConfigureRemote(context.Background(), "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}

	// Different URL replaces.
	if err := store.ConfigureRemote(context.Background(), "https://example.com/other.git", "master"); err != nil {
		t.Fatal(err)
	}
}

func TestSyncOriginAlreadyAncestorOfAgent(t *testing.T) {
	// If the agent has local commits on top of origin's tip, origin is already
	// an ancestor of agent — Sync should be a no-op (Synced=false).
	_, agent, _, _ := setupOriginAndAgent(t)

	// Agent diverges ahead of origin (origin hasn't advanced).
	if _, _, err := agent.WriteFile(context.Background(), testBranch, "kb/local.md", "# Local\n", "local write", "learn"); err != nil {
		t.Fatal(err)
	}

	result, err := agent.Sync(context.Background(), testBranch, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Synced {
		t.Fatal("expected Synced=false when origin is already an ancestor of agent")
	}
}

func TestPush(t *testing.T) {
	t.Run("no remote returns Pushed=false", func(t *testing.T) {
		s := newTestStore(t)

		result, err := s.Push(context.Background(), testBranch)
		if err != nil {
			t.Fatal(err)
		}
		if result.Pushed {
			t.Fatal("expected Pushed=false with no remote")
		}
	})

	t.Run("pushes agent branch to origin", func(t *testing.T) {
		_, agent, originSto, _ := setupOriginAndAgent(t)

		// Agent writes a file.
		if _, _, err := agent.WriteFile(context.Background(), testBranch, "kb/agent-push.md", "# Push test\n", "agent: push test", "learn"); err != nil {
			t.Fatal(err)
		}

		// Push agent branch to origin.
		result, err := agent.Push(context.Background(), testBranch)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Pushed {
			t.Fatal("expected Pushed=true")
		}

		// Verify origin has the agent branch with the file.
		// The agent branch on origin should now have the pushed content.
		agentRef, err := originSto.Reference(plumbing.NewBranchReferenceName(testBranch))
		if err != nil {
			t.Fatalf("expected agent branch on origin, got: %v", err)
		}
		if agentRef.Hash().IsZero() {
			t.Fatal("expected non-zero hash for agent branch on origin")
		}
	})

	t.Run("already up to date returns Pushed=false", func(t *testing.T) {
		_, agent, _, _ := setupOriginAndAgent(t)

		// Push with no local changes (initial push).
		result, err := agent.Push(context.Background(), testBranch)
		if err != nil {
			t.Fatal(err)
		}

		// Second push should be no-op.
		result, err = agent.Push(context.Background(), testBranch)
		if err != nil {
			t.Fatal(err)
		}
		if result.Pushed {
			t.Fatal("expected Pushed=false when already up to date")
		}
	})

	t.Run("non-fast-forward retries with force push", func(t *testing.T) {
		// Regression test: after an origin session clone+swap, the remote may
		// have the agent branch with a different history. Push should detect
		// the non-fast-forward error and retry with a force push.
		origin, agent, originSto, _ := setupOriginAndAgent(t)

		// Agent writes and pushes a file.
		if _, _, err := agent.WriteFile(context.Background(), testBranch, "kb/first.md", "# First\n", "first", "learn"); err != nil {
			t.Fatal(err)
		}
		if _, err := agent.Push(context.Background(), testBranch); err != nil {
			t.Fatal(err)
		}

		// Simulate divergence: write a different commit directly on origin's
		// copy of the agent branch, so origin's agent branch is ahead.
		if _, _, err := origin.WriteFile(context.Background(), testBranch, "kb/origin-only.md", "# Origin\n", "origin diverge", "learn"); err != nil {
			t.Fatal(err)
		}
		// Point origin's agent branch ref at origin's HEAD (diverged).
		originHead, _ := origin.HeadCommit(context.Background(), testBranch)
		originAgentRef := plumbing.NewHashReference(
			plumbing.NewBranchReferenceName(testBranch),
			plumbing.NewHash(originHead),
		)
		if err := originSto.SetReference(originAgentRef); err != nil {
			t.Fatal(err)
		}

		// Agent writes another file locally — now its agent branch has diverged
		// from origin's copy of the agent branch.
		if _, _, err := agent.WriteFile(context.Background(), testBranch, "kb/second.md", "# Second\n", "second", "learn"); err != nil {
			t.Fatal(err)
		}

		// Push should succeed via force push fallback.
		result, err := agent.Push(context.Background(), testBranch)
		if err != nil {
			t.Fatalf("expected force push to succeed, got: %v", err)
		}
		if !result.Pushed {
			t.Fatal("expected Pushed=true after force push")
		}
	})
}
