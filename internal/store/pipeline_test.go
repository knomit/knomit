package store_test

import (
	"context"
	"testing"

	"knomit/internal/store"
)

func newTestIndex(t *testing.T) *store.Index {
	t.Helper()
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

// ── Watermark tests ──────────────────────────────────────────────────────────

func TestGetPipelineWatermark_Empty(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	hash, err := idx.GetPipelineWatermark(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "" {
		t.Fatalf("expected empty string for unset watermark, got %q", hash)
	}
}

func TestSetGetPipelineWatermark(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	if err := idx.SetPipelineWatermark(ctx, "review", "machine/test", "abc123"); err != nil {
		t.Fatal(err)
	}

	hash, err := idx.GetPipelineWatermark(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "abc123" {
		t.Fatalf("expected abc123, got %q", hash)
	}

	// Overwrite
	if err := idx.SetPipelineWatermark(ctx, "review", "machine/test", "def456"); err != nil {
		t.Fatal(err)
	}

	hash, err = idx.GetPipelineWatermark(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "def456" {
		t.Fatalf("expected def456, got %q", hash)
	}
}

func TestPipelineWatermark_IndependentPerBranch(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	if err := idx.SetPipelineWatermark(ctx, "review", "branch-a", "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetPipelineWatermark(ctx, "review", "branch-b", "bbb"); err != nil {
		t.Fatal(err)
	}

	ha, err := idx.GetPipelineWatermark(ctx, "review", "branch-a")
	if err != nil {
		t.Fatal(err)
	}
	hb, err := idx.GetPipelineWatermark(ctx, "review", "branch-b")
	if err != nil {
		t.Fatal(err)
	}
	if ha != "aaa" || hb != "bbb" {
		t.Fatalf("expected aaa/bbb, got %q/%q", ha, hb)
	}
}

// ── Session lifecycle tests ──────────────────────────────────────────────────

func TestCreateAndGetPipelineSession(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreatePipelineSession(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		t.Fatal("expected non-empty session ID")
	}
	if s.Branch != "machine/test" {
		t.Fatalf("expected branch machine/test, got %q", s.Branch)
	}
	if s.Status != "active" {
		t.Fatalf("expected active status, got %q", s.Status)
	}

	got, err := idx.GetPipelineSession(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected session, got nil")
	}
	if got.ID != s.ID {
		t.Fatalf("expected ID %q, got %q", s.ID, got.ID)
	}
}

func TestGetPipelineSession_NotFound(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	got, err := idx.GetPipelineSession(ctx, "nonexistent-id")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("expected nil for nonexistent session")
	}
}

func TestCompletePipelineSession(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreatePipelineSession(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}

	if err := idx.CompletePipelineSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetPipelineSession(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" {
		t.Fatalf("expected completed, got %q", got.Status)
	}
}

func TestCreatePipelineSession_AbandonsStale(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s1, err := idx.CreatePipelineSession(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}

	// Creating a second session on the same tool+branch should abandon the first.
	s2, err := idx.CreatePipelineSession(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}
	if s2.ID == s1.ID {
		t.Fatal("expected different session IDs")
	}

	got, err := idx.GetPipelineSession(ctx, s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "abandoned" {
		t.Fatalf("expected abandoned, got %q", got.Status)
	}

	got2, err := idx.GetPipelineSession(ctx, s2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != "active" {
		t.Fatalf("expected active, got %q", got2.Status)
	}
}

func TestCreatePipelineSession_DoesNotAbandonOtherBranch(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s1, err := idx.CreatePipelineSession(ctx, "review", "branch-a")
	if err != nil {
		t.Fatal(err)
	}

	// Creating on a different branch should not touch branch-a's session.
	_, err = idx.CreatePipelineSession(ctx, "review", "branch-b")
	if err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetPipelineSession(ctx, s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" {
		t.Fatalf("expected active (different branch), got %q", got.Status)
	}
}

// ── Work item tests ──────────────────────────────────────────────────────────

func TestInsertAndNextPipelineWorkItem(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreatePipelineSession(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}

	// Insert two items with different priorities.
	if err := idx.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  s.ID,
		StepType:   "prune",
		ClusterKey: "cluster-low",
		FactsJSON:  `["a.md"]`,
		Priority:   1.0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  s.ID,
		StepType:   "distill",
		ClusterKey: "cluster-high",
		FactsJSON:  `["b.md","c.md"]`,
		Priority:   10.0,
	}); err != nil {
		t.Fatal(err)
	}

	// Next should return the highest priority item.
	item, err := idx.NextPipelineWorkItem(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item == nil {
		t.Fatal("expected work item, got nil")
	}
	if item.ClusterKey != "cluster-high" {
		t.Fatalf("expected cluster-high (priority 10), got %q", item.ClusterKey)
	}
	if item.StepType != "distill" {
		t.Fatalf("expected distill, got %q", item.StepType)
	}
	if item.Response != nil {
		t.Fatal("expected nil response for unanswered item")
	}
}

func TestSetPipelineWorkItemResponse(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreatePipelineSession(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}

	if err := idx.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  s.ID,
		StepType:   "prune",
		ClusterKey: "cluster-a",
		FactsJSON:  `["a.md"]`,
		Priority:   5.0,
	}); err != nil {
		t.Fatal(err)
	}

	item, err := idx.NextPipelineWorkItem(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := idx.SetPipelineWorkItemResponse(ctx, item.ID, "keep all"); err != nil {
		t.Fatal(err)
	}

	// After responding, NextPipelineWorkItem should return nil (no unanswered items).
	next, err := idx.NextPipelineWorkItem(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatal("expected nil after all items answered")
	}
}

func TestNextPipelineWorkItem_SkipsAnswered(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreatePipelineSession(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}

	// Insert two items: high priority and low priority.
	if err := idx.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID: s.ID, StepType: "prune", ClusterKey: "high",
		FactsJSON: `["a.md"]`, Priority: 10.0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID: s.ID, StepType: "prune", ClusterKey: "low",
		FactsJSON: `["b.md"]`, Priority: 1.0,
	}); err != nil {
		t.Fatal(err)
	}

	// Answer the high-priority item.
	high, err := idx.NextPipelineWorkItem(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.SetPipelineWorkItemResponse(ctx, high.ID, "done"); err != nil {
		t.Fatal(err)
	}

	// Next should now return the low-priority item.
	next, err := idx.NextPipelineWorkItem(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next == nil {
		t.Fatal("expected low-priority item")
	}
	if next.ClusterKey != "low" {
		t.Fatalf("expected low, got %q", next.ClusterKey)
	}
}

func TestNextPipelineWorkItem_EmptySession(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreatePipelineSession(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}

	item, err := idx.NextPipelineWorkItem(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item != nil {
		t.Fatal("expected nil for session with no items")
	}
}

func TestPipelineWorkItemDepth(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreatePipelineSession(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}

	// Insert a work item with Depth=2.
	if err := idx.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  s.ID,
		StepType:   "distill",
		ClusterKey: "deep-cluster",
		FactsJSON:  `["a.md","b.md"]`,
		Priority:   5.0,
		Depth:      2,
	}); err != nil {
		t.Fatal(err)
	}

	item, err := idx.NextPipelineWorkItem(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item == nil {
		t.Fatal("expected work item, got nil")
	}
	if item.Depth != 2 {
		t.Fatalf("expected depth 2, got %d", item.Depth)
	}

	// Verify default depth=0 for items inserted without explicit depth.
	if err := idx.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  s.ID,
		StepType:   "prune",
		ClusterKey: "shallow-cluster",
		FactsJSON:  `["c.md"]`,
		Priority:   1.0,
	}); err != nil {
		t.Fatal(err)
	}

	// Answer the first item so NextPipelineWorkItem returns the second.
	if err := idx.SetPipelineWorkItemResponse(ctx, item.ID, "ok"); err != nil {
		t.Fatal(err)
	}

	item2, err := idx.NextPipelineWorkItem(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item2 == nil {
		t.Fatal("expected second work item, got nil")
	}
	if item2.Depth != 0 {
		t.Fatalf("expected default depth 0, got %d", item2.Depth)
	}
}

// ── PipelineWorkItemStats tests ──────────────────────────────────────────────

func TestPipelineWorkItemStats(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	s, err := idx.CreatePipelineSession(ctx, "review", "machine/test")
	if err != nil {
		t.Fatal(err)
	}

	// Initially: 0 completed, 0 remaining.
	c, r, err := idx.PipelineWorkItemStats(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c != 0 || r != 0 {
		t.Fatalf("expected 0/0, got %d/%d", c, r)
	}

	// Insert 3 items.
	for i, key := range []string{"a", "b", "c"} {
		if err := idx.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
			SessionID: s.ID, StepType: "prune", ClusterKey: key,
			FactsJSON: `["x.md"]`, Priority: float64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	c, r, err = idx.PipelineWorkItemStats(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c != 0 || r != 3 {
		t.Fatalf("expected 0/3, got %d/%d", c, r)
	}

	// Answer one.
	item, _ := idx.NextPipelineWorkItem(ctx, s.ID)
	if err := idx.SetPipelineWorkItemResponse(ctx, item.ID, "ok"); err != nil {
		t.Fatal(err)
	}

	c, r, err = idx.PipelineWorkItemStats(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c != 1 || r != 2 {
		t.Fatalf("expected 1/2, got %d/%d", c, r)
	}
}

// ── GC tests ─────────────────────────────────────────────────────────────────

func TestGCPipelineSessions(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	// Create 5 sessions on the same branch.
	var ids []string
	for i := 0; i < 5; i++ {
		s, err := idx.CreatePipelineSession(ctx, "review", "machine/test")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}

	// Only the last one should be active; the rest abandoned.
	// Add a work item to session 3 (index 2) to test cascade.
	if err := idx.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID: ids[2], StepType: "prune", ClusterKey: "gc-test",
		FactsJSON: `["z.md"]`, Priority: 1.0,
	}); err != nil {
		t.Fatal(err)
	}

	// Keep only 2 most recent sessions.
	if err := idx.GCPipelineSessions(ctx, "review", "machine/test", 2); err != nil {
		t.Fatal(err)
	}

	// Sessions 0, 1, 2 should be deleted; 3 and 4 should remain.
	for _, id := range ids[:3] {
		got, err := idx.GetPipelineSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Fatalf("expected session %s to be deleted by GC", id)
		}
	}
	for _, id := range ids[3:] {
		got, err := idx.GetPipelineSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatalf("expected session %s to survive GC", id)
		}
	}

	// Work items for deleted session should also be gone (cascade).
	var count int
	if err := idx.TestDB().QueryRow(
		`SELECT COUNT(*) FROM pipeline_work_items WHERE session_id = ?`, ids[2],
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 work items after GC cascade, got %d", count)
	}
}

func TestGCPipelineSessions_IndependentPerBranch(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	// Create sessions on two branches.
	sA, err := idx.CreatePipelineSession(ctx, "review", "branch-a")
	if err != nil {
		t.Fatal(err)
	}
	sB, err := idx.CreatePipelineSession(ctx, "review", "branch-b")
	if err != nil {
		t.Fatal(err)
	}

	// GC branch-a keeping 0 should not affect branch-b.
	if err := idx.GCPipelineSessions(ctx, "review", "branch-a", 0); err != nil {
		t.Fatal(err)
	}

	gotA, err := idx.GetPipelineSession(ctx, sA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotA != nil {
		t.Fatal("expected branch-a session to be deleted")
	}

	gotB, err := idx.GetPipelineSession(ctx, sB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotB == nil {
		t.Fatal("expected branch-b session to survive")
	}
}
