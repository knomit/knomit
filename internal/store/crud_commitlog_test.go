package store

import (
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

func TestLastCommitForPath_ReturnsLatestNonDeleted(t *testing.T) {
	idx := newTestIndexInternal(t)
	db := idx.db

	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation) VALUES ('aaa', 'kb/test.md', 'added', 100, 'add', 'learn')`)
	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation) VALUES ('bbb', 'kb/test.md', 'modified', 200, 'update', 'learn')`)

	hash, ok := idx.LastCommitForPath("kb/test.md")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if hash != "bbb" {
		t.Fatalf("expected bbb, got %s", hash)
	}
}

func TestLastCommitForPath_ExcludesDeleted(t *testing.T) {
	idx := newTestIndexInternal(t)
	db := idx.db

	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation) VALUES ('aaa', 'kb/gone.md', 'added', 100, 'add', 'learn')`)
	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation) VALUES ('bbb', 'kb/gone.md', 'deleted', 200, 'delete', 'learn')`)

	_, ok := idx.LastCommitForPath("kb/gone.md")
	if ok {
		t.Fatal("expected ok=false when last action is deleted")
	}
}

func TestLastCommitForPath_NotFound(t *testing.T) {
	idx := newTestIndexInternal(t)

	_, ok := idx.LastCommitForPath("kb/nope.md")
	if ok {
		t.Fatal("expected ok=false for missing path")
	}
}

func TestCommitTimestamp_Found(t *testing.T) {
	idx := newTestIndexInternal(t)
	db := idx.db

	db.Exec(`INSERT INTO commit_log (commit_hash, path, action, committed_at, message, operation) VALUES ('abc123', 'kb/test.md', 'added', 1700000000, 'add', 'learn')`)

	ts, ok := idx.CommitTimestamp("abc123")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if ts != 1700000000 {
		t.Fatalf("expected 1700000000, got %d", ts)
	}
}

func TestCommitTimestamp_NotFound(t *testing.T) {
	idx := newTestIndexInternal(t)

	_, ok := idx.CommitTimestamp("nonexistent")
	if ok {
		t.Fatal("expected ok=false for missing hash")
	}
}
