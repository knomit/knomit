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
	return repos.NewTestInstance(name)
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

func TestSwapStore_InMemoryFallback(t *testing.T) {
	m := emptyManager()
	ri := makeRI("knomit")
	// DBPath="" signals in-memory/test mode.

	tempDB := openTestDB(t)
	if err := m.SwapStore(ri, tempDB); err != nil {
		t.Fatalf("SwapStore returned error: %v", err)
	}
	// In-memory fallback opens the temp DB directly — Svc must be non-nil.
	var svc *store.Service
	ri.WithRead(func(d repos.StoreDeps) { svc = d.Svc })
	if svc == nil {
		t.Fatal("expected Svc to be set after in-memory fallback")
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

	var oldSvc *store.Service
	ri.WithRead(func(d repos.StoreDeps) { oldSvc = d.Svc })

	// Swap to a new database.
	tempDB := openTestDB(t)
	if err := m.SwapStore(ri, tempDB); err != nil {
		t.Fatalf("SwapStore: %v", err)
	}

	// Rebuild MCP handlers (as the origin session handler does).
	m.SetupMCP(ri)

	// The old service's DB is closed; the new one should be open.
	var newSvc *store.Service
	ri.WithRead(func(d repos.StoreDeps) { newSvc = d.Svc })
	if newSvc == oldSvc {
		t.Fatal("ri.Svc should have changed after SwapStore")
	}

	// Verify the new index is usable (not closed).
	_, err := newSvc.Index().GetLastCommit(context.Background(), "_check")
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
	var gs repos.GitStore
	ri.WithRead(func(d repos.StoreDeps) { gs = d.GS })
	writer := gs.(interface {
		WriteFile(ctx context.Context, branch, path, content, message, operation string) (string, string, error)
	})
	_, _, err := writer.WriteFile(context.Background(), ri.AgentBranch(), "kb/test/hello.md", "---\ntitle: hello\n---\n# hello\nworld\n", "test", "learn")
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var oldSvc *store.Service
	ri.WithRead(func(d repos.StoreDeps) { oldSvc = d.Svc })
	oldIdx := oldSvc.Index()

	// Swap to a new database — this closes the old DB.
	tempDB := openTestDB(t)
	if err := m.SwapStore(ri, tempDB); err != nil {
		t.Fatalf("SwapStore: %v", err)
	}
	m.SetupMCP(ri)

	// The new index should be different and open.
	var newSvc *store.Service
	ri.WithRead(func(d repos.StoreDeps) { newSvc = d.Svc })
	newIdx := newSvc.Index()
	if newIdx == oldIdx {
		t.Fatal("expected new index after SwapStore")
	}

	// Verify the new index is queryable (not closed).
	if _, err := newIdx.GetLastCommit(context.Background(), "_check"); err != nil {
		t.Fatalf("new index Stats failed: %v", err)
	}

	// Verify the old index IS closed (confirms the bug scenario).
	_, oldErr := oldIdx.GetLastCommit(context.Background(), "_check")
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
	var newSvc *store.Service
	ri.WithRead(func(d repos.StoreDeps) { newSvc = d.Svc })

	// Close should close the new service.
	ri.Close()

	// The new service's DB should now be closed.
	_, err := newSvc.Index().GetLastCommit(context.Background(), "_check")
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

	// Start readers that continuously snapshot GS/Svc/Idx under WithRead.
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
					ri.WithRead(func(d repos.StoreDeps) {
						_ = d.GS
						_ = d.Svc
						_ = d.Idx
					})
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
