package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/test/testenv"
)

// Agent ids are minted lower case, the hostname part included: a macOS
// default hostname (mixed case) yields a lower-case host label and id, which
// pki.ValidAgentID accepts. SABOTAGE: drop the ToLower in sanitizeHostname →
// red.
func TestNewIdentity_MintsALowerCaseID(t *testing.T) {
	if got := sanitizeHostname("Pauls-MacBook-Pro.local"); got != "pauls-macbook-pro.local" {
		t.Fatalf("sanitizeHostname = %q, want pauls-macbook-pro.local", got)
	}
	host := sanitizeHostname("Büro PC+1")
	if host != "büro-pc-1" {
		t.Fatalf("sanitizeHostname = %q, want büro-pc-1", host)
	}
	id := newIdentity("ab12cd34")
	if err := pki.ValidAgentID(id.AgentID); err != nil {
		t.Fatalf("minted id %q: %v", id.AgentID, err)
	}
	if strings.ToLower(id.AgentID) != id.AgentID || strings.ToLower(id.Host) != id.Host {
		t.Fatalf("minted identity is not lower case: %+v", id)
	}
}

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
	firstID := a.AgentID()
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
	if a2.AgentID() != firstID || firstID == "" {
		t.Fatalf("author id changed: %q -> %q", firstID, a2.AgentID())
	}
	// And the rotated key really is different (otherwise the test proves nothing).
	_, fp, err := ensureKeyPair(a2.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if newIdentity(fp).Branch == first {
		t.Fatal("fixture: the regenerated key derives the same branch; the test cannot tell persisted from re-derived")
	}
}
