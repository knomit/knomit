package store

import (
	"context"
	"testing"
)

func newTestIndexInternal(t *testing.T) *Index {
	t.Helper()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

// setupBranch creates a branch and returns its ID.
func setupBranch(t *testing.T, idx *Index, name string) int64 {
	ctx := context.Background()
	t.Helper()
	id, err := idx.EnsureBranch(ctx, name, "refs/heads/"+name)
	if err != nil {
		t.Fatalf("EnsureBranch(%q): %v", name, err)
	}
	return id
}

func TestLastCommitForPath_ReturnsLatestNonDeleted(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)
	db := idx.db
	branchID := setupBranch(t, idx, "test-branch")

	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation, branch_id) VALUES ('aaa', 'kb/test.md', 'added', 100, 'add', 'learn', ?)`, branchID)
	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation, branch_id) VALUES ('bbb', 'kb/test.md', 'modified', 200, 'update', 'learn', ?)`, branchID)

	hash, ok := idx.LastCommitForPath(ctx, "test-branch", "kb/test.md")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if hash != "bbb" {
		t.Fatalf("expected bbb, got %s", hash)
	}
}

func TestLastCommitForPath_ExcludesDeleted(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)
	db := idx.db
	branchID := setupBranch(t, idx, "test-branch")

	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation, branch_id) VALUES ('aaa', 'kb/gone.md', 'added', 100, 'add', 'learn', ?)`, branchID)
	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation, branch_id) VALUES ('bbb', 'kb/gone.md', 'deleted', 200, 'delete', 'learn', ?)`, branchID)

	_, ok := idx.LastCommitForPath(ctx, "test-branch", "kb/gone.md")
	if ok {
		t.Fatal("expected ok=false when last action is deleted")
	}
}

func TestLastCommitForPath_NotFound(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)
	setupBranch(t, idx, "test-branch")

	_, ok := idx.LastCommitForPath(ctx, "test-branch", "kb/nope.md")
	if ok {
		t.Fatal("expected ok=false for missing path")
	}
}

func TestLastCommitForPath_BranchScoped(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)
	db := idx.db
	branchA := setupBranch(t, idx, "branch-a")
	branchB := setupBranch(t, idx, "branch-b")

	// Insert on branch A first (lower rowid).
	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation, branch_id) VALUES ('aaa', 'kb/shared.md', 'added', 100, 'add on A', 'learn', ?)`, branchA)
	// Insert on branch B second (higher rowid, more recent).
	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation, branch_id) VALUES ('bbb', 'kb/shared.md', 'added', 200, 'add on B', 'learn', ?)`, branchB)

	// Querying branch A should return A's commit, not B's.
	hash, ok := idx.LastCommitForPath(ctx, "branch-a", "kb/shared.md")
	if !ok {
		t.Fatal("expected ok=true for branch-a")
	}
	if hash != "aaa" {
		t.Fatalf("expected aaa for branch-a, got %s", hash)
	}

	// Querying branch B should return B's commit.
	hash, ok = idx.LastCommitForPath(ctx, "branch-b", "kb/shared.md")
	if !ok {
		t.Fatal("expected ok=true for branch-b")
	}
	if hash != "bbb" {
		t.Fatalf("expected bbb for branch-b, got %s", hash)
	}
}

func TestLastCommitForPath_FallbackToNullBranchID(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)
	db := idx.db
	setupBranch(t, idx, "my-branch")

	// Insert a legacy row with NULL branch_id.
	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation, branch_id) VALUES ('legacy111', 'kb/old.md', 'added', 50, 'legacy add', 'learn', NULL)`)

	// Should fall back to the legacy row since no branch-scoped row exists.
	hash, ok := idx.LastCommitForPath(ctx, "my-branch", "kb/old.md")
	if !ok {
		t.Fatal("expected ok=true via fallback for legacy NULL branch_id row")
	}
	if hash != "legacy111" {
		t.Fatalf("expected legacy111, got %s", hash)
	}
}

func TestCommitTimestamp_Found(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)
	db := idx.db

	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation) VALUES ('abc123', 'kb/test.md', 'added', 1700000000, 'add', 'learn')`)

	ts, ok := idx.CommitTimestamp(ctx, "abc123")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if ts != 1700000000 {
		t.Fatalf("expected 1700000000, got %d", ts)
	}
}

func TestCommitTimestamp_NotFound(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndexInternal(t)

	_, ok := idx.CommitTimestamp(ctx, "nonexistent")
	if ok {
		t.Fatal("expected ok=false for missing hash")
	}
}
