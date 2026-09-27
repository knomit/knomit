package app

import (
	"context"
	"os"
	"testing"

	"knomit/internal/config"
	"knomit/internal/store"
	"knomit/test/testenv"
)

// TestBoot_AgentIdentityIsPersisted (F09 PR 5, D1): the agent branch is
// computed once and read from control.db on every later boot. A new key (the
// old one deleted, so ensureKeyPair generates another) must NOT change the
// branch, nor therefore the commit author id derived from it.
func TestBoot_AgentIdentityIsPersisted(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Home = t.TempDir()

	a, err := New(ctx, cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	first := a.AgentBranch()
	firstKey := a.KeyPath()
	a.Close()
	if first == "" {
		t.Fatal("no agent branch after the first boot")
	}

	// A different key: the fp8 a re-derivation would produce changes.
	if err := os.Remove(firstKey); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(firstKey + ".pub")
	a2, err := New(ctx, cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	defer a2.Close()
	if a2.AgentBranch() != first {
		t.Fatalf("agent branch changed with the key: %q -> %q", first, a2.AgentBranch())
	}
	if got, want := store.AgentIDOf(a2.AgentBranch()), store.AgentIDOf(first); got != want {
		t.Fatalf("author id changed: %q -> %q", want, got)
	}
	// And the rotated key really is different (otherwise the test proves nothing).
	_, fp, err := ensureKeyPair(a2.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if agentBranch(fp) == first {
		t.Fatal("fixture: the regenerated key derives the same branch; the test cannot tell persisted from re-derived")
	}
}
