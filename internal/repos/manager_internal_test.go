package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/identity"
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
	signer, fp, err := identity.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	agentBranch := identity.AgentBranch(fp)
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
	if err := svc.InitRepo(nil, ""); err != nil {
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
	if ri.svc == nil {
		t.Error("ri.svc is nil")
	}
	if ri.idx == nil {
		t.Error("ri.idx is nil")
	}
	if ri.hub == nil {
		t.Error("ri.hub is nil")
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

// ---------- remoteAuthFromRecord ----------

func TestRemoteAuthFromRecord_UsesRecordFields(t *testing.T) {
	fallback := identity.RemoteAuthConfig{Token: "global-tok", AuthMethod: "token"}
	remote := &store.Remote{AuthMethod: "basic", AuthToken: "alice:s3cret"}

	got := remoteAuthFromRecord(remote, fallback)
	if got.AuthMethod != "basic" {
		t.Errorf("AuthMethod = %q, want %q", got.AuthMethod, "basic")
	}
	if got.User != "alice" || got.Password != "s3cret" {
		t.Errorf("User:Password = %q:%q, want alice:s3cret", got.User, got.Password)
	}
}

func TestRemoteAuthFromRecord_TokenFallback(t *testing.T) {
	fallback := identity.RemoteAuthConfig{Token: "global-tok", AuthMethod: "token"}
	remote := &store.Remote{AuthToken: "override-tok"}

	got := remoteAuthFromRecord(remote, fallback)
	if got.Token != "override-tok" {
		t.Errorf("Token = %q, want %q", got.Token, "override-tok")
	}
	if got.AuthMethod != "token" {
		t.Errorf("AuthMethod = %q, want %q (from fallback)", got.AuthMethod, "token")
	}
}

func TestRemoteAuthFromRecord_EmptyRecordUsesFallback(t *testing.T) {
	fallback := identity.RemoteAuthConfig{Token: "global", AuthMethod: "token", SSHKey: "/path/key"}
	remote := &store.Remote{}

	got := remoteAuthFromRecord(remote, fallback)
	if got != fallback {
		t.Errorf("expected fallback config unchanged, got %+v", got)
	}
}

// ---------- openOne with in-memory remote ----------

// setupOrigin creates an in-memory git store with content on main, registers
// the inmem:// transport, and returns the origin store URL for use as
// cfg.Git.Origin.
func setupOrigin(t *testing.T) (originURL string) {
	t.Helper()
	originSvc, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { originSvc.Close() })
	if err := originSvc.InitRepo(map[string]string{
		"kb/seed.md": "---\ntitle: seed\n---\nhello\n",
	}, "main"); err != nil {
		t.Fatal(err)
	}

	loader := server.MapLoader{"inmem:///origin": originSvc.GitStorer()}
	client.InstallProtocol("inmem", server.NewClient(loader))
	t.Cleanup(func() { client.InstallProtocol("inmem", nil) })

	return "inmem:///origin"
}

func TestOpenOne_WithRemote_StartsSyncLoops(t *testing.T) {
	originURL := setupOrigin(t)
	dir := t.TempDir()
	deps, _ := defaultTestDeps(t, dir)
	deps.Cfg.Git.Origin = originURL

	m := New(context.Background(), deps)
	dbPath := filepath.Join(dir, "knomit.db")

	ri, err := m.openOne("knomit", dbPath, true)
	if err != nil {
		t.Fatalf("openOne: %v", err)
	}
	defer func() {
		if ri.syncCancel != nil {
			ri.syncCancel()
		}
		if ri.syncWg != nil {
			ri.syncWg.Wait()
		}
		ri.Close()
	}()

	// The sync loops do an immediate sync/push on start. Give them a moment.
	time.Sleep(200 * time.Millisecond)

	// Verify the remote record was seeded.
	remote, err := ri.svc.GetRemote("origin")
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if remote == nil {
		t.Fatal("origin remote not seeded")
	}
	if remote.URL != originURL {
		t.Errorf("remote URL = %q, want %q", remote.URL, originURL)
	}

	// Verify sync ran (status should be set by the loop).
	if remote.LastStatus == nil {
		t.Error("expected LastStatus to be set after sync loop ran")
	}
}

func TestStartSync_Closure(t *testing.T) {
	originURL := setupOrigin(t)
	dir := t.TempDir()
	deps, _ := defaultTestDeps(t, dir)
	deps.Cfg.Git.Origin = originURL

	m := New(context.Background(), deps)
	dbPath := filepath.Join(dir, "knomit.db")

	ri, err := m.openOne("knomit", dbPath, true)
	if err != nil {
		t.Fatalf("openOne: %v", err)
	}
	// Stop initial sync loops so we can test startSync fresh.
	ri.syncCancel()
	ri.syncWg.Wait()

	defer func() {
		if ri.syncCancel != nil {
			ri.syncCancel()
		}
		if ri.syncWg != nil {
			ri.syncWg.Wait()
		}
		ri.Close()
	}()

	// Call startSync — should restart loops.
	if err := ri.startSync(originURL); err != nil {
		t.Fatalf("startSync: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Verify push status was updated by the new loops.
	remote, err := ri.svc.GetRemote("origin")
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if remote.LastPushStatus == nil {
		t.Error("expected LastPushStatus to be set after startSync re-launched loops")
	}
}
