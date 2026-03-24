package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/git"
	"knomit/internal/store"
)

// stubEmbedder satisfies repos.Embedder (and store.Embedder) without ONNX model files.
type stubEmbedder struct{ calls int }

func (s *stubEmbedder) Embed(_ string) ([]float32, error) {
	s.calls++
	return []float32{1, 0, 0, 0}, nil
}
func (s *stubEmbedder) EmbedBatch(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// assertEmbedderSet fails the test if the concrete *store.Index inside ri does
// not have an embedder attached. Uses EmbedderSet() added for this purpose.
func assertEmbedderSet(t *testing.T, ri *RepoInstance, msg string) {
	t.Helper()
	idx, ok := ri.Idx.(*store.Index)
	if !ok {
		t.Fatalf("%s: ri.Idx is not *store.Index", msg)
	}
	if !idx.EmbedderSet() {
		t.Errorf("%s: embedder not set on index after swap", msg)
	}
}

func defaultTestDeps(t *testing.T, dir string) (Deps, string) {
	t.Helper()
	keyPath := filepath.Join(dir, "id_ed25519")
	signer, fp, err := git.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	agentBranch := git.AgentBranch(fp)
	deps := Deps{
		Cfg:         config.Config{},
		Signer:      signer,
		AgentBranch: agentBranch,
		KeyPath:     keyPath,
	}
	return deps, agentBranch
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

func TestOpenOne_DefaultCreatesNewRepo(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "knomit.db")
	deps, agentBranch := defaultTestDeps(t, dir)

	m := New(context.Background(), deps)
	ri, err := m.openOne("knomit", dbPath, true)
	if err != nil {
		t.Fatalf("openOne: %v", err)
	}
	defer ri.Close()

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db file not created: %v", err)
	}
	if ri.Name != "knomit" {
		t.Errorf("ri.Name = %q, want %q", ri.Name, "knomit")
	}
	if ri.GS == nil {
		t.Error("ri.GS is nil")
	}
	if ri.Svc == nil {
		t.Error("ri.Svc is nil")
	}
	if ri.Idx == nil {
		t.Error("ri.Idx is nil")
	}
	if ri.Hub == nil {
		t.Error("ri.Hub is nil")
	}
	gs, ok := ri.GS.(*git.Store)
	if !ok {
		t.Fatal("ri.GS is not *git.Store")
	}
	if gs.Branch() != agentBranch {
		t.Errorf("branch = %q, want %q", gs.Branch(), agentBranch)
	}
	// MCP handlers should be nil — SetupMCP not called yet
	if ri.MCPHandlers != nil {
		t.Error("MCPHandlers should be nil before SetupMCP")
	}
}

func TestOpenOne_NonDefaultFailsWithoutGit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "empty.db")
	os.WriteFile(dbPath, []byte{}, 0o644)

	deps, _ := defaultTestDeps(t, dir)
	m := New(context.Background(), deps)

	_, err := m.openOne("empty", dbPath, false)
	if err == nil {
		t.Fatal("expected error for non-default repo without git data")
	}
}

func TestSetupMCP_NoLLM(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	deps, _ := defaultTestDeps(t, dir)

	m := New(context.Background(), deps)
	m.ontology = fact.DefaultOntology()

	ri, err := m.openOne("test", dbPath, true)
	if err != nil {
		t.Fatalf("openOne: %v", err)
	}
	defer ri.Close()

	m.SetupMCP(ri)

	if ri.MCPHandlers == nil {
		t.Fatal("MCPHandlers should not be nil after setupMCP")
	}
	if len(ri.MCPHandlers) != 3 {
		t.Errorf("MCPHandlers has %d profiles, want 3", len(ri.MCPHandlers))
	}
	for _, profile := range []string{"code", "chat", "generic"} {
		if ri.MCPHandlers[profile] == nil {
			t.Errorf("MCPHandlers[%q] is nil", profile)
		}
	}
	if ri.SynthDeps != nil {
		t.Error("SynthDeps should be nil without LLM adapter")
	}
}

func TestSwapStoreRestoresEmbedder(t *testing.T) {
	// Regression: SwapStore opened a new store.Index without calling
	// SetEmbedder, silently dropping the embedder after every sync. This
	// caused rebuild to report "embedded=0" even though an embedder was
	// configured at startup.
	dir := t.TempDir()
	deps, _ := defaultTestDeps(t, dir)
	emb := &stubEmbedder{}
	deps.Embedder = emb

	m := New(context.Background(), deps)
	dbPath := filepath.Join(dir, "knomit.db")
	ri, err := m.openOne("knomit", dbPath, true)
	if err != nil {
		t.Fatalf("openOne: %v", err)
	}
	defer ri.Close()

	// Sanity-check: embedder set immediately after openOne.
	assertEmbedderSet(t, ri, "after openOne")

	// Build a second fully-initialized DB in a separate directory to use as the
	// "incoming" store, matching what sync produces before calling SwapStore.
	dir2 := t.TempDir()
	deps2, _ := defaultTestDeps(t, dir2)
	m2 := New(context.Background(), deps2)
	ri2, err := m2.openOne("knomit", filepath.Join(dir2, "knomit.db"), true)
	if err != nil {
		t.Fatalf("openOne (second): %v", err)
	}
	tempDB := filepath.Join(dir2, "knomit.db")
	ri2.Close()

	if err := m.SwapStore(ri, tempDB); err != nil {
		t.Fatalf("SwapStore: %v", err)
	}

	// After the swap the new index must have the embedder attached.
	assertEmbedderSet(t, ri, "after SwapStore")
}
