package store_test

import (
	"context"
	"testing"

	"knomit/internal/store"
)

func TestCreateToolSession(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreateToolSession(ctx, "explore", "machine/test", "concepts/")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		t.Fatal("expected non-empty session ID")
	}
	if s.Tool != "explore" {
		t.Fatalf("expected tool explore, got %q", s.Tool)
	}
	if s.Branch != "machine/test" {
		t.Fatalf("expected branch machine/test, got %q", s.Branch)
	}
	if s.PathPrefix != "concepts/" {
		t.Fatalf("expected path_prefix concepts/, got %q", s.PathPrefix)
	}
	if s.LastCommit != "" {
		t.Fatalf("expected empty last_commit, got %q", s.LastCommit)
	}
	if s.Status != "active" {
		t.Fatalf("expected active status, got %q", s.Status)
	}
	if s.CreatedAt == "" {
		t.Fatal("expected non-empty created_at")
	}
	if s.UpdatedAt == "" {
		t.Fatal("expected non-empty updated_at")
	}
}

func TestGetToolSession(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreateToolSession(ctx, "explore", "machine/test", "")
	if err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetToolSession(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected session, got nil")
	}
	if got.ID != s.ID {
		t.Fatalf("expected ID %q, got %q", s.ID, got.ID)
	}
	if got.Tool != "explore" {
		t.Fatalf("expected tool explore, got %q", got.Tool)
	}
	if got.Branch != s.Branch {
		t.Fatalf("expected branch %q, got %q", s.Branch, got.Branch)
	}
	if got.PathPrefix != s.PathPrefix {
		t.Fatalf("expected path_prefix %q, got %q", s.PathPrefix, got.PathPrefix)
	}
	if got.Status != "active" {
		t.Fatalf("expected active, got %q", got.Status)
	}

	// Nonexistent returns nil, no error.
	got2, err := idx.GetToolSession(ctx, "nonexistent-id")
	if err != nil {
		t.Fatal(err)
	}
	if got2 != nil {
		t.Fatal("expected nil for nonexistent session")
	}
}

func TestUpdateToolSession(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreateToolSession(ctx, "explore", "machine/test", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := idx.UpdateToolSession(ctx, s.ID, "abc123", "completed"); err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetToolSession(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastCommit != "abc123" {
		t.Fatalf("expected last_commit abc123, got %q", got.LastCommit)
	}
	if got.Status != "completed" {
		t.Fatalf("expected completed, got %q", got.Status)
	}
	if got.UpdatedAt == "" {
		t.Fatal("expected non-empty updated_at after update")
	}
}

func TestToolSeenPaths(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreateToolSession(ctx, "explore", "machine/test", "")
	if err != nil {
		t.Fatal(err)
	}

	// Initially empty.
	seen, err := idx.GetSeenPaths(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Fatalf("expected 0 seen paths, got %d", len(seen))
	}

	// Add some paths.
	if err := idx.AddSeenPaths(ctx, s.ID, []string{"a.md", "b.md", "c.md"}); err != nil {
		t.Fatal(err)
	}

	seen, err = idx.GetSeenPaths(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 seen paths, got %d", len(seen))
	}
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		if !seen[p] {
			t.Fatalf("expected %q in seen paths", p)
		}
	}

	// Add duplicates — should not grow beyond 4.
	if err := idx.AddSeenPaths(ctx, s.ID, []string{"b.md", "c.md", "d.md"}); err != nil {
		t.Fatal(err)
	}

	seen, err = idx.GetSeenPaths(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 4 {
		t.Fatalf("expected 4 seen paths after adding 1 new + 2 duplicates, got %d", len(seen))
	}
}

func TestEnqueueAndDequeuePaths(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreateToolSession(ctx, "explore", "machine/test", "")
	if err != nil {
		t.Fatal(err)
	}

	items := []store.QueueItem{
		{Path: "a.md", CommitHash: "aaa", Depth: 0},
		{Path: "b.md", CommitHash: "bbb", Depth: 1},
		{Path: "c.md", CommitHash: "ccc", Depth: 2},
	}
	if err := idx.EnqueuePaths(ctx, s.ID, items); err != nil {
		t.Fatal(err)
	}

	// Dequeue 2 — should get depth-0 and depth-1 items (breadth-first).
	got, err := idx.DequeuePaths(ctx, s.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 dequeued items, got %d", len(got))
	}
	if got[0].Path != "a.md" || got[0].Depth != 0 {
		t.Fatalf("expected first item a.md@depth=0, got %s@depth=%d", got[0].Path, got[0].Depth)
	}
	if got[1].Path != "b.md" || got[1].Depth != 1 {
		t.Fatalf("expected second item b.md@depth=1, got %s@depth=%d", got[1].Path, got[1].Depth)
	}

	// Verify remaining count.
	size, err := idx.QueueSize(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if size != 1 {
		t.Fatalf("expected queue size 1 after dequeue, got %d", size)
	}
}

func TestDequeuePathsEmpty(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreateToolSession(ctx, "explore", "machine/test", "")
	if err != nil {
		t.Fatal(err)
	}

	got, err := idx.DequeuePaths(ctx, s.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil from empty dequeue, got %d items", len(got))
	}
}

func TestQueueSize(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreateToolSession(ctx, "explore", "machine/test", "")
	if err != nil {
		t.Fatal(err)
	}

	// Initially zero.
	size, err := idx.QueueSize(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		t.Fatalf("expected queue size 0, got %d", size)
	}

	// Enqueue some items.
	items := []store.QueueItem{
		{Path: "a.md", CommitHash: "aaa", Depth: 0},
		{Path: "b.md", CommitHash: "bbb", Depth: 0},
	}
	if err := idx.EnqueuePaths(ctx, s.ID, items); err != nil {
		t.Fatal(err)
	}

	size, err = idx.QueueSize(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if size != 2 {
		t.Fatalf("expected queue size 2, got %d", size)
	}

	// Dequeue all.
	if _, err := idx.DequeuePaths(ctx, s.ID, 10); err != nil {
		t.Fatal(err)
	}

	size, err = idx.QueueSize(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		t.Fatalf("expected queue size 0 after dequeue, got %d", size)
	}
}
