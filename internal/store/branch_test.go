package store

import (
	"context"
	"testing"
)

func TestEnsureBranch_CreatesNew(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)

	id, err := idx.EnsureBranch(ctx, "main", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}
}

func TestEnsureBranch_Idempotent(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)

	id1, err := idx.EnsureBranch(ctx, "main", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := idx.EnsureBranch(ctx, "main", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("expected same ID, got %d and %d", id1, id2)
	}
}

func TestBranchID_NotFound(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)

	_, err := idx.branchID(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent branch")
	}
}

func TestBranchID_Cached(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)

	id1, err := idx.EnsureBranch(ctx, "main", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}

	id2, err := idx.branchID(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("expected same ID, got %d and %d", id1, id2)
	}
}

func TestListBranches(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)

	idx.EnsureBranch(ctx, "main", "refs/heads/main")
	idx.EnsureBranch(ctx, "agent/dev", "refs/heads/agent/dev")

	branches, err := idx.ListBranches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(branches))
	}
}
