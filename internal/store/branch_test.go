package store

import (
	"testing"
)

func TestEnsureBranch_CreatesNew(t *testing.T) {
	idx := newTestIndexInternal(t)

	id, err := idx.EnsureBranch("main", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}
}

func TestEnsureBranch_Idempotent(t *testing.T) {
	idx := newTestIndexInternal(t)

	id1, err := idx.EnsureBranch("main", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := idx.EnsureBranch("main", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("expected same ID, got %d and %d", id1, id2)
	}
}

func TestBranchID_NotFound(t *testing.T) {
	idx := newTestIndexInternal(t)

	_, err := idx.BranchID("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent branch")
	}
}

func TestBranchID_Cached(t *testing.T) {
	idx := newTestIndexInternal(t)

	id1, err := idx.EnsureBranch("main", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}

	id2, err := idx.BranchID("main")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("expected same ID, got %d and %d", id1, id2)
	}
}

func TestListBranches(t *testing.T) {
	idx := newTestIndexInternal(t)

	idx.EnsureBranch("main", "refs/heads/main")
	idx.EnsureBranch("agent/dev", "refs/heads/agent/dev")

	branches, err := idx.ListBranches()
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(branches))
	}
}
