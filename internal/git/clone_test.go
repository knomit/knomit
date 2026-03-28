package git_test

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"

	git "knomit/internal/git"
)

func TestDefaultBranch_ResolvesFromHEAD(t *testing.T) {
	store := newTestStore(t)

	branch, err := store.DefaultBranch()
	if err != nil {
		t.Fatal(err)
	}

	// Init sets HEAD → testBranch, so DefaultBranch should return that.
	if branch != testBranch {
		t.Fatalf("DefaultBranch() = %q, want %q", branch, testBranch)
	}
}

func TestCloneInto_ClonesRemoteToTempStorer(t *testing.T) {
	// Create an origin store with content.
	originSto := newTestStorer(t)
	origin, err := git.InitWithStorer(originSto, nil, testBranch)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := origin.WriteFile(testBranch, "kb/hello.md", "# Hello\n", "add hello", "learn"); err != nil {
		t.Fatal(err)
	}
	// Advance main to HEAD.
	head, err := origin.HeadCommit(testBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := originSto.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(head)),
	); err != nil {
		t.Fatal(err)
	}

	// Register in-process transport.
	loader := server.MapLoader{"inmem:///clone-origin": originSto}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	// Clone into a fresh storer.
	destStorer := newTestStorer(t)
	cloned, err := git.CloneInto(destStorer, "inmem:///clone-origin", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Should be able to read the file.
	// CloneInto sets HEAD to the default branch of the remote, which may differ
	// from testBranch. We use DefaultBranch to discover the branch.
	clonedBranch, err := cloned.DefaultBranch()
	if err != nil {
		t.Fatal(err)
	}
	content, err := cloned.ReadFile(clonedBranch, "kb/hello.md")
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Hello\n" {
		t.Fatalf("unexpected content: %q", content)
	}

	// DefaultBranch should resolve.
	if clonedBranch == "" {
		t.Fatal("expected non-empty default branch")
	}
}

func TestHasSharedHistory_DisjointRepos(t *testing.T) {
	// InitWithStorer always creates an identical root commit (kb.md, same
	// author). If two repos are created within the same second, the root
	// hash is identical and HasSharedHistory correctly returns true.
	// To get truly disjoint histories, we need the root commits to differ.
	// We achieve this by adding unique initFiles — the second commit (which
	// writes the initFile) will differ, but the root is still shared.
	//
	// To guarantee disjoint roots we sleep 1s so the timestamps differ.
	s1 := newTestStorer(t)
	disjoint1, err := git.InitWithStorer(s1, nil, "agent/alpha")
	if err != nil {
		t.Fatal(err)
	}
	// Write unique content so HEAD moves past the potentially-shared root.
	if _, _, err := disjoint1.WriteFile("agent/alpha", "kb/only-a.md", "# A unique\n", "add a", "learn"); err != nil {
		t.Fatal(err)
	}

	// Sleep to ensure the second repo's root commit has a different timestamp.
	time.Sleep(1100 * time.Millisecond)

	s2 := newTestStorer(t)
	disjoint2, err := git.InitWithStorer(s2, nil, "agent/beta")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := disjoint2.WriteFile("agent/beta", "kb/only-b.md", "# B unique\n", "add b", "learn"); err != nil {
		t.Fatal(err)
	}

	shared, err := disjoint1.HasSharedHistory("agent/alpha", disjoint2, "agent/beta")
	if err != nil {
		t.Fatal(err)
	}
	if shared {
		t.Fatal("expected no shared history between independently created repos")
	}
}

func TestHasSharedHistory_SharedOrigin(t *testing.T) {
	// Create an origin and clone it.
	originSto := newTestStorer(t)
	origin, err := git.InitWithStorer(originSto, nil, testBranch)
	if err != nil {
		t.Fatal(err)
	}

	// Advance main.
	head, err := origin.HeadCommit(testBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := originSto.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(head)),
	); err != nil {
		t.Fatal(err)
	}

	// Register in-process transport.
	loader := server.MapLoader{"inmem:///shared-origin": originSto}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	// Clone into a fresh storer.
	destStorer := newTestStorer(t)
	cloned, err := git.CloneInto(destStorer, "inmem:///shared-origin", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	clonedBranch, err := cloned.DefaultBranch()
	if err != nil {
		t.Fatal(err)
	}

	shared, err := origin.HasSharedHistory(testBranch, cloned, clonedBranch)
	if err != nil {
		t.Fatal(err)
	}
	if !shared {
		t.Fatal("expected shared history between origin and clone")
	}
}
