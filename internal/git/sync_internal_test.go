// Internal tests for sync — uses package git to access unexported methods.
package git

import (
	"testing"
)

func TestSyncMergeCommitAuthor(t *testing.T) {
	// Create origin with shared content.
	origin, err := Init(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	if _, _, err := origin.WriteFile("kb/shared.md", "# Shared\n", "origin: add shared", "learn"); err != nil {
		t.Fatal(err)
	}

	// Create agent store (simulating a separate agent).
	agent, err := Init(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()

	// Write divergent content on both sides.
	if _, _, err := origin.WriteFile("kb/origin-only.md", "# Origin\n", "origin: diverge", "learn"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.WriteFile("kb/agent-only.md", "# Agent\n", "agent: diverge", "learn"); err != nil {
		t.Fatal(err)
	}

	// Test the signature methods directly — these are what the merge commit uses.
	agentID := agent.AgentID()
	authorSig := agent.authorSig("sync")
	committerSig := agent.committerSig()

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
