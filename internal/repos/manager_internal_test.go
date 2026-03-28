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
	idx, ok := ri.idx.(*store.Index)
	if !ok {
		t.Fatalf("%s: ri.idx is not *store.Index", msg)
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

func newEmptyManager() *Manager {
	return New(context.Background(), Deps{})
}

func openTestDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	svc, err := store.Open(path)
	if err != nil {
		t.Fatalf("openTestDB: store.Open: %v", err)
	}
	if _, err := git.InitWithStorer(svc.GitStorer(), nil, ""); err != nil {
		svc.Close()
		t.Fatalf("openTestDB: git.InitWithStorer: %v", err)
	}
	svc.Close()
	return path
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
	if ri.name != "knomit" {
		t.Errorf("ri.name = %q, want %q", ri.name, "knomit")
	}
	if ri.gs == nil {
		t.Error("ri.gs is nil")
	}
	if ri.svc == nil {
		t.Error("ri.svc is nil")
	}
	if ri.idx == nil {
		t.Error("ri.idx is nil")
	}
	if ri.hub == nil {
		t.Error("ri.hub is nil")
	}
	if _, ok := ri.gs.(*git.Store); !ok {
		t.Fatal("ri.gs is not *git.Store")
	}
	if ri.agentBranch != agentBranch {
		t.Errorf("agentBranch = %q, want %q", ri.agentBranch, agentBranch)
	}
	if ri.mcpHandlers != nil {
		t.Error("mcpHandlers should be nil before SetupMCP")
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

	if ri.mcpHandlers == nil {
		t.Fatal("mcpHandlers should not be nil after SetupMCP")
	}
	if len(ri.mcpHandlers) != 3 {
		t.Errorf("mcpHandlers has %d profiles, want 3", len(ri.mcpHandlers))
	}
	for _, profile := range []string{"code", "chat", "generic"} {
		if ri.mcpHandlers[profile] == nil {
			t.Errorf("mcpHandlers[%q] is nil", profile)
		}
	}
	if ri.synthDeps != nil {
		t.Error("synthDeps should be nil without LLM adapter")
	}
}

func TestShutdown_CallsClose(t *testing.T) {
	m := newEmptyManager()
	closed := make(chan string, 2)
	for _, name := range []string{"a", "b"} {
		n := name
		ri := NewTestInstance(n)
		ri.closeFn = func() { closed <- n }
		m.Set(n, ri)
	}
	m.Shutdown()
	close(closed)
	got := map[string]bool{}
	for n := range closed {
		got[n] = true
	}
	if !got["a"] || !got["b"] {
		t.Fatalf("Shutdown did not call Close on all repos: %v", got)
	}
}

func TestSwapStore_FileSwap(t *testing.T) {
	m := newEmptyManager()
	realDB := openTestDB(t)
	svc, err := store.Open(realDB)
	if err != nil {
		t.Fatalf("open real DB: %v", err)
	}
	ri := NewTestInstance("knomit")
	ri.dbPath = realDB
	ri.svc = svc

	tempDB := openTestDB(t)
	if err := m.SwapStore(ri, tempDB); err != nil {
		t.Fatalf("SwapStore: %v", err)
	}
	if ri.svc == nil {
		t.Fatal("expected ri.svc to be set after file swap")
	}
	if ri.gs == nil {
		t.Fatal("expected ri.gs to be set after file swap")
	}
	if ri.idx == nil {
		t.Fatal("expected ri.idx to be set after file swap")
	}
	if _, err := os.Stat(realDB + ".bak"); !os.IsNotExist(err) {
		t.Fatal("expected backup to be removed after successful swap")
	}
}

func TestSwapStore_InvalidTempPath_ReturnsError(t *testing.T) {
	m := newEmptyManager()
	realDB := openTestDB(t)
	svc, err := store.Open(realDB)
	if err != nil {
		t.Fatalf("reopen real DB: %v", err)
	}
	ri := NewTestInstance("knomit")
	ri.dbPath = realDB
	ri.svc = svc

	err = m.SwapStore(ri, "/nonexistent/path/to/temp.db")
	if err == nil {
		t.Fatal("expected error for invalid temp path, got nil")
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
