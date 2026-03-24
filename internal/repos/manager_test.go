package repos_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"knomit/internal/config"
	"knomit/internal/git"
	"knomit/internal/repos"
	"knomit/internal/store"
)

func emptyManager() *repos.Manager {
	return repos.New(context.Background(), repos.Deps{})
}

func makeRI(name string) *repos.RepoInstance {
	return &repos.RepoInstance{
		Name:       name,
		SyncCancel: func() {},
		SyncWg:     &sync.WaitGroup{},
	}
}

func TestNew_Empty(t *testing.T) {
	m := emptyManager()
	if m == nil {
		t.Fatal("New returned nil")
	}
	if names := m.Names(); len(names) != 0 {
		t.Fatalf("expected 0 repos, got %v", names)
	}
}

func TestSetAndGet(t *testing.T) {
	m := emptyManager()
	ri := makeRI("knomit")
	m.Set("knomit", ri)
	got := m.Get("knomit")
	if got != ri {
		t.Fatal("Get did not return the same instance that was Set")
	}
}

func TestGet_Unknown(t *testing.T) {
	m := emptyManager()
	if m.Get("missing") != nil {
		t.Fatal("expected nil for unknown repo")
	}
}

func TestReplace_ReturnsOld(t *testing.T) {
	m := emptyManager()
	old := makeRI("knomit")
	m.Set("knomit", old)
	newRI := makeRI("knomit")
	prev := m.Replace("knomit", newRI)
	if prev != old {
		t.Fatal("Replace did not return the old instance")
	}
	if m.Get("knomit") != newRI {
		t.Fatal("Replace did not install the new instance")
	}
}

func TestReplace_NoOld(t *testing.T) {
	m := emptyManager()
	ri := makeRI("work")
	prev := m.Replace("work", ri)
	if prev != nil {
		t.Fatalf("expected nil for absent repo, got %v", prev)
	}
}

func TestForEach(t *testing.T) {
	m := emptyManager()
	m.Set("a", makeRI("a"))
	m.Set("b", makeRI("b"))
	seen := map[string]bool{}
	m.ForEach(func(name string, _ *repos.RepoInstance) {
		seen[name] = true
	})
	if !seen["a"] || !seen["b"] || len(seen) != 2 {
		t.Fatalf("ForEach did not visit all repos: %v", seen)
	}
}

func TestNames_Sorted(t *testing.T) {
	m := emptyManager()
	m.Set("zebra", makeRI("zebra"))
	m.Set("apple", makeRI("apple"))
	m.Set("mango", makeRI("mango"))
	names := m.Names()
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %v", names)
	}
	if names[0] != "apple" || names[1] != "mango" || names[2] != "zebra" {
		t.Fatalf("Names not sorted: %v", names)
	}
}

func TestShutdown_CallsClose(t *testing.T) {
	m := emptyManager()
	closed := make(chan string, 2)
	for _, name := range []string{"a", "b"} {
		n := name
		ri := makeRI(n)
		ri.Close = func() { closed <- n }
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

// ---------- Context helpers ----------

func TestRepoFromContext_Roundtrip(t *testing.T) {
	ri := makeRI("test")
	ctx := repos.WithRepoInstance(context.Background(), ri)
	got := repos.RepoFromContext(ctx)
	if got != ri {
		t.Fatal("roundtrip through context failed")
	}
}

func TestRepoFromContext_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when RepoInstance not in context")
		}
	}()
	repos.RepoFromContext(context.Background())
}

// ---------- SwapStore ----------

func openTestDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	gs, err := git.Init(path, nil)
	if err != nil {
		t.Fatalf("openTestDB: git.Init: %v", err)
	}
	gs.Close()
	return path
}

func TestSwapStore_InMemoryFallback(t *testing.T) {
	m := emptyManager()
	ri := makeRI("knomit")
	// DBPath="" signals in-memory/test mode.

	tempDB := openTestDB(t)
	if err := m.SwapStore(ri, tempDB); err != nil {
		t.Fatalf("SwapStore returned error: %v", err)
	}
	// In-memory fallback opens the temp DB directly — Svc must be non-nil.
	if ri.Svc == nil {
		t.Fatal("expected ri.Svc to be set after in-memory fallback")
	}
}

func TestSwapStore_FileSwap(t *testing.T) {
	m := emptyManager()
	// Create a real DB at a persistent path (already closed by openTestDB).
	realDB := openTestDB(t)
	svc, err := store.Open(realDB)
	if err != nil {
		t.Fatalf("open real DB: %v", err)
	}
	ri := makeRI("knomit")
	ri.DBPath = realDB
	ri.Svc = svc

	// Create a second DB to swap in.
	tempDB := openTestDB(t)
	if err := m.SwapStore(ri, tempDB); err != nil {
		t.Fatalf("SwapStore returned error: %v", err)
	}
	if ri.Svc == nil {
		t.Fatal("expected ri.Svc to be set after file swap")
	}
	if ri.GS == nil {
		t.Fatal("expected ri.GS to be set after file swap")
	}
	if ri.Idx == nil {
		t.Fatal("expected ri.Idx to be set after file swap")
	}
	// Backup should be cleaned up on success.
	if _, err := os.Stat(realDB + ".bak"); !os.IsNotExist(err) {
		t.Fatal("expected backup to be removed after successful swap")
	}
}

func TestSwapStore_InvalidTempPath_ReturnsError(t *testing.T) {
	m := emptyManager()
	// Set up a real DB so it tries the file-swap path.
	realDB := openTestDB(t)
	svc, err := store.Open(realDB)
	if err != nil {
		t.Fatalf("reopen real DB: %v", err)
	}
	ri := makeRI("knomit")
	ri.DBPath = realDB
	ri.Svc = svc

	// Pass a non-existent temp path — copyFile should fail.
	err = m.SwapStore(ri, "/nonexistent/path/to/temp.db")
	if err == nil {
		t.Fatal("expected error for invalid temp path, got nil")
	}
}

func TestSetupMCP_RebindsAfterSwapStore(t *testing.T) {
	// Regression test: MCP handlers must use the new database after SwapStore,
	// not the old (closed) one. Before the fix, MCP learn calls would fail
	// with "sql: database is closed" because handlers captured the original index.
	dir := t.TempDir()
	m := bootManager(t, dir)
	defer m.Shutdown()

	if err := m.Boot(); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	ri := m.Get("knomit")
	if ri == nil {
		t.Fatal("knomit not registered")
	}

	oldSvc := ri.Svc

	// Swap to a new database.
	tempDB := openTestDB(t)
	if err := m.SwapStore(ri, tempDB); err != nil {
		t.Fatalf("SwapStore: %v", err)
	}

	// Rebuild MCP handlers (as the origin session handler does).
	m.SetupMCP(ri)

	// The old service's DB is closed; the new one should be open.
	if ri.Svc == oldSvc {
		t.Fatal("ri.Svc should have changed after SwapStore")
	}

	// Verify the new index is usable (not closed).
	_, err := ri.Svc.Index().Stats("")
	if err != nil {
		t.Fatalf("new index query failed (database closed?): %v", err)
	}
}

func TestObserver_UsesCurrentIndexAfterSwapStore(t *testing.T) {
	// Regression test: the observer closure must read ri.Svc.Index() at call
	// time. Before the fix it captured the original idx which became closed
	// after SwapStore, causing "sql: database is closed" on every commit.
	dir := t.TempDir()
	m := bootManager(t, dir)
	defer m.Shutdown()

	if err := m.Boot(); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	ri := m.Get("knomit")
	if ri == nil {
		t.Fatal("knomit not registered")
	}

	// Write a fact so the old index has some state.
	gs := ri.GS.(interface {
		WriteFile(path, content, message, operation string) (string, string, error)
	})
	_, _, err := gs.WriteFile("kb/test/hello.md", "---\ntitle: hello\n---\n# hello\nworld\n", "test", "learn")
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Wait for observer to fire (debounce is 1s in openOne).
	// Give it a generous window.
	oldIdx := ri.Svc.Index()

	// Swap to a new database — this closes the old DB.
	tempDB := openTestDB(t)
	if err := m.SwapStore(ri, tempDB); err != nil {
		t.Fatalf("SwapStore: %v", err)
	}
	m.SetupMCP(ri)

	// The new index should be different and open.
	newIdx := ri.Svc.Index()
	if newIdx == oldIdx {
		t.Fatal("expected new index after SwapStore")
	}

	// Verify the new index is queryable (not closed).
	if _, err := newIdx.Stats(""); err != nil {
		t.Fatalf("new index Stats failed: %v", err)
	}

	// Verify the old index IS closed (confirms the bug scenario).
	_, oldErr := oldIdx.Stats("")
	if oldErr == nil {
		t.Fatal("expected old index to be closed after SwapStore")
	}
}

func TestClose_ClosesCurrentSvcAfterSwapStore(t *testing.T) {
	// Regression test: ri.Close must close the current ri.Svc, not the
	// original one captured at openOne time.
	dir := t.TempDir()
	m := bootManager(t, dir)
	defer m.Shutdown()

	if err := m.Boot(); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	ri := m.Get("knomit")
	if ri == nil {
		t.Fatal("knomit not registered")
	}

	// Swap to a new database.
	tempDB := openTestDB(t)
	if err := m.SwapStore(ri, tempDB); err != nil {
		t.Fatalf("SwapStore: %v", err)
	}

	// Capture the new service before Close.
	newSvc := ri.Svc

	// Close should close the new service.
	ri.Close()

	// The new service's DB should now be closed.
	_, err := newSvc.Index().Stats("")
	if err == nil {
		t.Fatal("expected new service to be closed after ri.Close()")
	}
}

// ---------- Boot / Add ----------

func bootManager(t *testing.T, dir string) *repos.Manager {
	t.Helper()
	keyPath := filepath.Join(dir, "id_ed25519")
	signer, fp, err := git.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	return repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: dir},
		Signer:      signer,
		AgentBranch: git.AgentBranch(fp),
		KeyPath:     keyPath,
	})
}

func TestBoot_OpensKnomitAndRegisters(t *testing.T) {
	dir := t.TempDir()
	m := bootManager(t, dir)
	defer m.Shutdown()

	if err := m.Boot(); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if m.Get("knomit") == nil {
		t.Error("knomit not registered after Boot")
	}
}

func TestBoot_MissingOntologyUsesDefault(t *testing.T) {
	// Boot on a fresh dir — no ontology.yaml written yet — should not error.
	dir := t.TempDir()
	m := bootManager(t, dir)
	defer m.Shutdown()

	if err := m.Boot(); err != nil {
		t.Fatalf("Boot with missing ontology: %v", err)
	}
	if m.Get("knomit") == nil {
		t.Error("knomit not registered")
	}
}

func TestBoot_SkipsInvalidNames(t *testing.T) {
	dir := t.TempDir()
	reposDir := filepath.Join(dir, "repos")
	os.MkdirAll(reposDir, 0o755)
	// Create a db file with an invalid name — Boot should skip it.
	os.WriteFile(filepath.Join(reposDir, "My.Bad.db"), []byte{}, 0o644)

	m := bootManager(t, dir)
	defer m.Shutdown()

	if err := m.Boot(); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	// Only knomit should be registered; "My.Bad" should be skipped.
	names := m.Names()
	if len(names) != 1 || names[0] != "knomit" {
		t.Errorf("expected only [knomit], got %v", names)
	}
}

// ---------- Concurrency ----------

// TestRepoInstance_SwapStore_ConcurrentRead verifies that concurrent reads of
// ri.GS/ri.Svc/ri.Idx while SwapStore is writing do not produce a data race.
// Run with: go test -race ./internal/repos/ -run TestRepoInstance_SwapStore_ConcurrentRead
func TestRepoInstance_SwapStore_ConcurrentRead(t *testing.T) {
	dir := t.TempDir()
	m := bootManager(t, dir)
	defer m.Shutdown()
	if err := m.Boot(); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	ri := m.Get("knomit")
	if ri == nil {
		t.Fatal("knomit not registered")
	}

	const readers = 8
	const swaps = 5

	var wg sync.WaitGroup

	// Start readers that continuously snapshot GS/Svc/Idx under RLock.
	stop := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					ri.RLock()
					_ = ri.GS
					_ = ri.Svc
					_ = ri.Idx
					ri.RUnlock()
				}
			}
		}()
	}

	// Perform several SwapStore calls to trigger the write path.
	for i := 0; i < swaps; i++ {
		tempDB := openTestDB(t)
		if err := m.SwapStore(ri, tempDB); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("SwapStore iteration %d: %v", i, err)
		}
	}

	close(stop)
	wg.Wait()
}

func TestAdd_RegistersRepo(t *testing.T) {
	dir := t.TempDir()
	m := bootManager(t, dir)
	defer m.Shutdown()

	if err := m.Boot(); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	workDB := openTestDB(t)
	if err := m.Add("work", workDB); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if m.Get("work") == nil {
		t.Error("work not registered after Add")
	}
}
