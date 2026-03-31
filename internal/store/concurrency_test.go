package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDelete_ConcurrentRefcount(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "concurrent_del.db")
	idx, err := New(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	defer os.Remove(tmp)
	ctx := context.Background()

	branchA := "agent/del-a"
	branchB := "agent/del-b"

	// Same fact on two branches (COW).
	rec := FactRecord{
		Path: "kb/shared.md", BlobHash: "bh_shared", Title: "Shared",
		Domain: []string{"test"},
	}
	idx.Upsert(ctx, branchA, "c1", rec)
	idx.Upsert(ctx, branchB, "c1", rec)

	// Delete from both branches concurrently.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = idx.Delete(ctx, branchA, "kb/shared.md") }()
	go func() { defer wg.Done(); errs[1] = idx.Delete(ctx, branchB, "kb/shared.md") }()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// Fact should be fully deleted (no branch_facts refs remain).
	var bfCount int
	idx.db.QueryRow(`SELECT COUNT(*) FROM branch_facts WHERE path = 'kb/shared.md'`).Scan(&bfCount)
	if bfCount != 0 {
		t.Errorf("expected 0 branch_facts rows, got %d", bfCount)
	}

	// Fact row should be gone (GC'd by last deleter).
	var factCount int
	idx.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE path = 'kb/shared.md'`).Scan(&factCount)
	if factCount != 0 {
		t.Errorf("expected 0 facts rows, got %d", factCount)
	}
}

func TestUpsert_ConcurrentCOW(t *testing.T) {
	// Use a temp file DB so all connections share the same database.
	// :memory: with MaxOpenConns>1 gives each conn its own database.
	tmp := filepath.Join(t.TempDir(), "concurrent.db")
	idx, err := New(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	defer os.Remove(tmp)
	ctx := context.Background()

	branch := "agent/concurrent"
	rec := FactRecord{
		Path: "kb/race.md", BlobHash: "bh_race", Title: "Race",
		Domain: []string{"test"}, Entities: []string{"Go"},
	}

	// 10 goroutines upsert the same (path, blob_hash) simultaneously.
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			errs[n] = idx.Upsert(ctx, branch, "commit1", rec)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// Exactly one facts row should exist.
	var count int
	idx.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE path = ? AND blob_hash = ?`,
		rec.Path, rec.BlobHash).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 fact row, got %d", count)
	}
}
