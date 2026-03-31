package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

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
