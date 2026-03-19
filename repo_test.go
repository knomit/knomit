package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/git"
)

func defaultTestConfig() config.Config {
	return config.Config{}
}

func TestIsValidRepoName(t *testing.T) {
	valid := []string{"knomit", "work", "my-kb", "test_123", "a"}
	for _, name := range valid {
		if !isValidRepoName(name) {
			t.Errorf("isValidRepoName(%q) = false, want true", name)
		}
	}

	invalid := []string{"", "My-KB", "work.db", "../evil", "a b", "UPPER", "hello!"}
	for _, name := range invalid {
		if isValidRepoName(name) {
			t.Errorf("isValidRepoName(%q) = true, want false", name)
		}
	}
}

func TestOpenRepo_DefaultCreatesNewRepo(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "knomit.db")

	keyPath := filepath.Join(dir, "id_ed25519")
	signer, fp, err := git.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	agentBranch := git.AgentBranch(fp)

	ctx := context.Background()
	result, err := openRepo(ctx, "knomit", dbPath, true, signer, agentBranch, nil, nil, "kb", nil, defaultTestConfig(), keyPath)
	if err != nil {
		t.Fatalf("openRepo: %v", err)
	}
	defer result.cleanup()

	// Verify the db file was created
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db file not created: %v", err)
	}

	// Verify RepoInstance fields
	if result.ri.Name != "knomit" {
		t.Errorf("ri.Name = %q, want %q", result.ri.Name, "knomit")
	}
	if result.ri.GS == nil {
		t.Error("ri.GS is nil")
	}
	if result.ri.Svc == nil {
		t.Error("ri.Svc is nil")
	}
	if result.ri.Idx == nil {
		t.Error("ri.Idx is nil")
	}
	if result.ri.Hub == nil {
		t.Error("ri.Hub is nil")
	}
	if result.gs.Branch() != agentBranch {
		t.Errorf("branch = %q, want %q", result.gs.Branch(), agentBranch)
	}

	// MCP handlers should be nil (no ontology passed)
	if result.ri.MCPHandlers != nil {
		t.Error("MCPHandlers should be nil when ontology is nil")
	}
}

func TestOpenRepo_NonDefaultFailsWithoutGit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "empty.db")

	// Create an empty file — not a valid knomit db
	os.WriteFile(dbPath, []byte{}, 0o644)

	keyPath := filepath.Join(dir, "id_ed25519")
	signer, fp, err := git.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	agentBranch := git.AgentBranch(fp)

	ctx := context.Background()
	_, err = openRepo(ctx, "empty", dbPath, false, signer, agentBranch, nil, nil, "kb", nil, defaultTestConfig(), keyPath)
	if err == nil {
		t.Fatal("expected error for non-default repo without git data")
	}
}

func TestOpenRepo_WithOntologyCreatesMCP(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	keyPath := filepath.Join(dir, "id_ed25519")
	signer, fp, err := git.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	agentBranch := git.AgentBranch(fp)

	ctx := context.Background()
	ontology := fact.DefaultOntology()

	result, err := openRepo(ctx, "test", dbPath, true, signer, agentBranch, nil, nil, "kb", ontology, defaultTestConfig(), keyPath)
	if err != nil {
		t.Fatalf("openRepo: %v", err)
	}
	defer result.cleanup()

	if result.ri.MCPHandlers == nil {
		t.Fatal("MCPHandlers should not be nil when ontology is provided")
	}
	if len(result.ri.MCPHandlers) != 3 {
		t.Errorf("MCPHandlers has %d profiles, want 3", len(result.ri.MCPHandlers))
	}
	for _, profile := range []string{"code", "chat", "generic"} {
		if result.ri.MCPHandlers[profile] == nil {
			t.Errorf("MCPHandlers[%q] is nil", profile)
		}
	}
}

func TestSetRepoMCP(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	keyPath := filepath.Join(dir, "id_ed25519")
	signer, fp, err := git.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	agentBranch := git.AgentBranch(fp)

	ctx := context.Background()
	// Create repo without ontology first
	result, err := openRepo(ctx, "test", dbPath, true, signer, agentBranch, nil, nil, "kb", nil, defaultTestConfig(), keyPath)
	if err != nil {
		t.Fatalf("openRepo: %v", err)
	}
	defer result.cleanup()

	if result.ri.MCPHandlers != nil {
		t.Fatal("MCPHandlers should be nil before setRepoMCP")
	}

	// Now set MCP
	ontology := fact.DefaultOntology()
	setRepoMCP(result, "kb", ontology, nil, nil)

	if result.ri.MCPHandlers == nil {
		t.Fatal("MCPHandlers should not be nil after setRepoMCP")
	}
	if len(result.ri.MCPHandlers) != 3 {
		t.Errorf("MCPHandlers has %d profiles, want 3", len(result.ri.MCPHandlers))
	}

	// SynthDeps should be nil (no LLM adapter)
	if result.ri.SynthDeps != nil {
		t.Error("SynthDeps should be nil without LLM adapter")
	}
}
