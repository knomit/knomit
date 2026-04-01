package store_test

import (
	"context"
	"testing"
)

func TestGC_CleansOldSessions(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	// Create 7 tool sessions for the same tool+branch (GC keeps 5).
	for i := 0; i < 7; i++ {
		if _, err := idx.CreateToolSession(ctx, "explore", "machine/test", ""); err != nil {
			t.Fatal(err)
		}
	}

	// Create 7 pipeline sessions for the same tool+branch.
	for i := 0; i < 7; i++ {
		if _, err := idx.CreatePipelineSession(ctx, "review", "machine/test"); err != nil {
			t.Fatal(err)
		}
	}

	if err := idx.GC(ctx); err != nil {
		t.Fatal(err)
	}

	// Should have 5 tool sessions remaining.
	var toolCount int
	if err := idx.TestDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tool_sessions WHERE tool = 'explore' AND branch = 'machine/test'`,
	).Scan(&toolCount); err != nil {
		t.Fatal(err)
	}
	if toolCount != 5 {
		t.Fatalf("expected 5 tool sessions after GC, got %d", toolCount)
	}

	// Should have 5 pipeline sessions remaining.
	var pipelineCount int
	if err := idx.TestDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipeline_sessions WHERE tool = 'review' AND branch = 'machine/test'`,
	).Scan(&pipelineCount); err != nil {
		t.Fatal(err)
	}
	if pipelineCount != 5 {
		t.Fatalf("expected 5 pipeline sessions after GC, got %d", pipelineCount)
	}
}

func TestGC_SessionGCIndependentPerToolBranch(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)

	// Create 7 sessions on branch-a and 2 on branch-b.
	for i := 0; i < 7; i++ {
		if _, err := idx.CreateToolSession(ctx, "explore", "branch-a", ""); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := idx.CreateToolSession(ctx, "explore", "branch-b", ""); err != nil {
			t.Fatal(err)
		}
	}

	if err := idx.GC(ctx); err != nil {
		t.Fatal(err)
	}

	var countA, countB int
	if err := idx.TestDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tool_sessions WHERE tool = 'explore' AND branch = 'branch-a'`,
	).Scan(&countA); err != nil {
		t.Fatal(err)
	}
	if err := idx.TestDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tool_sessions WHERE tool = 'explore' AND branch = 'branch-b'`,
	).Scan(&countB); err != nil {
		t.Fatal(err)
	}
	if countA != 5 {
		t.Fatalf("expected 5 sessions on branch-a after GC, got %d", countA)
	}
	if countB != 2 {
		t.Fatalf("expected 2 sessions on branch-b after GC (untouched), got %d", countB)
	}
}
