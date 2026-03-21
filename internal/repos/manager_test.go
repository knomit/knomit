package repos_test

import (
	"context"
	"sync"
	"testing"

	"knomit/internal/repos"
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
