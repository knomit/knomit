package repos_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
