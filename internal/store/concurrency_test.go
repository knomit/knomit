package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	)

func TestSync_ConcurrentCAS(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "sync-race.db")
	idx, err := New(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ctx := context.Background()

	branch := "agent/sync-race"
	content := "---\ndomain: [test]\nconfidence: 0.9\nsources: 1\nentities: [Go]\nrefs: []\n---\n# Fact\n\nBody.\n"
	insertBlob(t, idx.db, "blob_kb/fact.md", content)

	const headHash = "abc123def456"
	git := &mockGitReader{
		files: map[string]string{"kb/fact.md": content},
		head:  headHash,
	}

	// Two concurrent Sync calls on the same branch.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = idx.Sync(ctx, git, branch) }()
	go func() { defer wg.Done(); errs[1] = idx.Sync(ctx, git, branch) }()
	wg.Wait()

	// Both should succeed (one wins CAS, other is harmless no-op).
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// Last commit should be set.
	last, _ := idx.GetLastCommit(ctx, branch)
	if last != headHash {
		t.Errorf("expected last commit %s, got %s", headHash, last)
	}

	// Exactly one fact should exist (COW dedup handles duplicate upserts).
	var count int
	idx.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE path = 'kb/fact.md'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 fact, got %d", count)
	}
}

func TestCreatePipelineSession_ConcurrentAbandon(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "pipeline-race.db")
	idx, err := New(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ctx := context.Background()

	tool := "review"
	branch := "agent/pipeline-race"

	// Two concurrent CreatePipelineSession calls.
	var wg sync.WaitGroup
	sessions := make([]*PipelineSession, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); sessions[0], errs[0] = idx.CreatePipelineSession(ctx, tool, branch) }()
	go func() { defer wg.Done(); sessions[1], errs[1] = idx.CreatePipelineSession(ctx, tool, branch) }()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// Exactly one active session should remain.
	var activeCount int
	idx.db.QueryRow(
		`SELECT COUNT(*) FROM pipeline_sessions WHERE tool = ? AND branch = ? AND status = 'active'`,
		tool, branch,
	).Scan(&activeCount)
	if activeCount != 1 {
		t.Errorf("expected 1 active session, got %d", activeCount)
	}
}

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

func TestConcurrent_WriteAndSync(t *testing.T) {
	// Use a real Service + GitStore to test the full write pipeline.
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ctx := context.Background()

	branch := "agent/e2e-concurrent"
	err = svc.InitRepo(nil, branch)
	if err != nil {
		t.Fatal(err)
	}

	idx := svc.Index()

	// 5 concurrent goroutines each write a distinct fact then sync.
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			path := fmt.Sprintf("kb/fact-%d.md", n)
			content := fmt.Sprintf("---\ntype: observation\ndomain: [test]\nconfidence: 0.9\nsources: 1\nentities: [item%d]\nrefs: []\n---\n# Fact %d\n\nBody %d.\n", n, n, n)
			if _, err := svc.WriteFact(ctx, branch, path, content, fmt.Sprintf("add fact %d", n), "learn"); err != nil {
				errs[n] = fmt.Errorf("WriteFact: %w", err)
				return
			}
			if err := idx.Sync(ctx, svc, branch); err != nil {
				errs[n] = fmt.Errorf("Sync: %w", err)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// All 5 facts should be indexed (Sync may need a final call to catch up).
	if err := idx.Sync(ctx, svc, branch); err != nil {
		t.Fatal(err)
	}

	branchID, err := idx.EnsureBranch(ctx, branch, "refs/heads/"+branch)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	idx.TestDB().QueryRow(`SELECT COUNT(*) FROM branch_facts WHERE branch_id = ?`, branchID).Scan(&count)
	if count != 5 {
		t.Errorf("expected 5 indexed facts, got %d", count)
	}
}

func TestConcurrent_MultiBranchUpsert(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "multibranch.db")
	idx, err := New(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ctx := context.Background()

	branches := []string{"agent/branch-1", "agent/branch-2", "agent/branch-3"}

	// 3 branches, 5 goroutines per branch upserting distinct facts.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allErrs []error

	for _, branch := range branches {
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(b string, n int) {
				defer wg.Done()
				rec := FactRecord{
					Path:     fmt.Sprintf("kb/%s-fact-%d.md", b, n),
					BlobHash: fmt.Sprintf("bh_%s_%d", b, n),
					Title:    fmt.Sprintf("Fact %d on %s", n, b),
					Domain:   []string{"test"},
					Entities: []string{fmt.Sprintf("Entity%d", n)},
				}
				if err := idx.Upsert(ctx, b, "commit1", rec); err != nil {
					mu.Lock()
					allErrs = append(allErrs, fmt.Errorf("%s goroutine %d: %w", b, n, err))
					mu.Unlock()
				}
			}(branch, i)
		}
	}
	wg.Wait()

	for _, err := range allErrs {
		t.Error(err)
	}

	// Each branch should have exactly 5 facts, no cross-contamination.
	for _, branch := range branches {
		branchID, err := idx.EnsureBranch(ctx, branch, "refs/heads/"+branch)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		idx.TestDB().QueryRow(`SELECT COUNT(*) FROM branch_facts WHERE branch_id = ?`, branchID).Scan(&count)
		if count != 5 {
			t.Errorf("branch %s: expected 5 facts, got %d", branch, count)
		}
	}

	// Total facts should be 15 (5 per branch, all distinct blob_hashes).
	var total int
	idx.TestDB().QueryRow(`SELECT COUNT(*) FROM facts`).Scan(&total)
	if total != 15 {
		t.Errorf("expected 15 total facts, got %d", total)
	}
}
