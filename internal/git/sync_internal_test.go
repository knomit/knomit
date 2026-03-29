// Internal tests — uses package git to access unexported methods.
package git

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestDeriveAgentID(t *testing.T) {
	tests := []struct {
		branch string
		want   string
	}{
		{"agent/laptop-abc", "laptop-abc"},
		{"agent/dev-a1b2c3", "dev-a1b2c3"},
		{"main", "main"},
		{"feature/foo", "feature/foo"},
		{"agent/", ""},
	}
	for _, tt := range tests {
		got := deriveAgentID(tt.branch)
		if got != tt.want {
			t.Errorf("deriveAgentID(%q) = %q, want %q", tt.branch, got, tt.want)
		}
	}
}

func TestCommitAuthorCommitter(t *testing.T) {
	store := newInternalTestStore(t)

	agentID := deriveAgentID(testBranch)

	// WriteFile with "learn" operation.
	hash, _, err := store.WriteFile(testBranch, "kb/test.md", "# Test\n", "add test", "learn")
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.repo.CommitObject(plumbing.NewHash(hash))
	if err != nil {
		t.Fatal(err)
	}
	wantAuthor := agentID + "+learn@agents.knomit.io"
	if c.Author.Email != wantAuthor {
		t.Errorf("WriteFile author email = %q, want %q", c.Author.Email, wantAuthor)
	}
	if c.Author.Name != agentID {
		t.Errorf("WriteFile author name = %q, want %q", c.Author.Name, agentID)
	}
	wantCommitter := agentID + "@agents.knomit.io"
	if c.Committer.Email != wantCommitter {
		t.Errorf("WriteFile committer email = %q, want %q", c.Committer.Email, wantCommitter)
	}

	// DeleteFile with "retract" operation.
	delHash, err := store.DeleteFile(testBranch, "kb/test.md", "retract test", "retract")
	if err != nil {
		t.Fatal(err)
	}
	dc, err := store.repo.CommitObject(plumbing.NewHash(delHash))
	if err != nil {
		t.Fatal(err)
	}
	wantDelAuthor := agentID + "+retract@agents.knomit.io"
	if dc.Author.Email != wantDelAuthor {
		t.Errorf("DeleteFile author email = %q, want %q", dc.Author.Email, wantDelAuthor)
	}
	if dc.Committer.Email != wantCommitter {
		t.Errorf("DeleteFile committer email = %q, want %q", dc.Committer.Email, wantCommitter)
	}

	// BatchWrite with "subsume" operation.
	batchHash, _, err := store.BatchWrite(testBranch, map[string]string{
		"kb/x.md": "# X\n",
		"kb/y.md": "# Y\n",
	}, "batch add", "subsume")
	if err != nil {
		t.Fatal(err)
	}
	bc, err := store.repo.CommitObject(plumbing.NewHash(batchHash))
	if err != nil {
		t.Fatal(err)
	}
	wantBatchAuthor := agentID + "+subsume@agents.knomit.io"
	if bc.Author.Email != wantBatchAuthor {
		t.Errorf("BatchWrite author email = %q, want %q", bc.Author.Email, wantBatchAuthor)
	}
	if bc.Committer.Email != wantCommitter {
		t.Errorf("BatchWrite committer email = %q, want %q", bc.Committer.Email, wantCommitter)
	}
}

func TestSyncMergeCommitAuthor(t *testing.T) {
	// Create origin with shared content.
	origin := newInternalTestStore(t)
	if _, _, err := origin.WriteFile(testBranch, "kb/shared.md", "# Shared\n", "origin: add shared", "learn"); err != nil {
		t.Fatal(err)
	}

	// Create agent store (simulating a separate agent).
	agent := newInternalTestStore(t)

	// Write divergent content on both sides.
	if _, _, err := origin.WriteFile(testBranch, "kb/origin-only.md", "# Origin\n", "origin: diverge", "learn"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.WriteFile(testBranch, "kb/agent-only.md", "# Agent\n", "agent: diverge", "learn"); err != nil {
		t.Fatal(err)
	}

	// Test the signature methods directly — these are what the merge commit uses.
	agentID := deriveAgentID(testBranch)
	authorSig := agent.authorSig(testBranch, "sync")
	committerSig := agent.committerSig(testBranch)

	wantAuthorEmail := agentID + "+sync@agents.knomit.io"
	if authorSig.Email != wantAuthorEmail {
		t.Errorf("authorSig email = %q, want %q", authorSig.Email, wantAuthorEmail)
	}
	if authorSig.Name != agentID {
		t.Errorf("authorSig name = %q, want %q", authorSig.Name, agentID)
	}

	wantCommitterEmail := agentID + "@agents.knomit.io"
	if committerSig.Email != wantCommitterEmail {
		t.Errorf("committerSig email = %q, want %q", committerSig.Email, wantCommitterEmail)
	}
}
