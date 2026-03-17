// Internal tests for commit_log population, append, and SQL fallback.
// Package git (not git_test) to access unexported fields: db, commitLog.
package git

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	storegit "knomit/internal/store/git"
)

// TestActivitySQLTimeBuckets verifies that the CASE WHEN cutoff expressions in
// activitySQL produce correct 7d/30d/90d counts. Rows are injected directly
// into commit_log so timestamps are fully controlled.
func TestActivitySQLTimeBuckets(t *testing.T) {
	dir := t.TempDir()
	store, err := Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if !store.commitLog {
		t.Skip("commit_log not available")
	}

	now := time.Now().Unix()
	injected := []struct {
		hash string
		path string
		ts   int64
		msg  string
	}{
		{"aa000001aa000001aa000001aa000001aa000001", "kb/r3d.md", now - 3*86400, "3d ago"},
		{"aa000002aa000002aa000002aa000002aa000002", "kb/r15d.md", now - 15*86400, "15d ago"},
		{"aa000003aa000003aa000003aa000003aa000003", "kb/r60d.md", now - 60*86400, "60d ago"},
		{"aa000004aa000004aa000004aa000004aa000004", "kb/r120d.md", now - 120*86400, "120d ago"},
	}
	for _, r := range injected {
		if _, err := store.db.Exec(
			`INSERT OR IGNORE INTO commit_log (commit_hash, path, committed_at, message) VALUES (?, ?, ?, ?)`,
			r.hash, r.path, r.ts, r.msg,
		); err != nil {
			t.Fatal(err)
		}
	}

	// Activity("kb") uses GLOB 'kb/*' — matches all 4 injected rows, not the
	// init commit's kb.md (root-level, doesn't match 'kb/*').
	a, err := store.activitySQL("kb")
	if err != nil {
		t.Fatal(err)
	}
	if a.Total != 4 {
		t.Errorf("Total = %d, want 4", a.Total)
	}
	if a.Changes7d != 1 {
		t.Errorf("Changes7d = %d, want 1 (only 3-day-old commit)", a.Changes7d)
	}
	if a.Changes30d != 2 {
		t.Errorf("Changes30d = %d, want 2 (3d + 15d)", a.Changes30d)
	}
	if a.Changes90d != 3 {
		t.Errorf("Changes90d = %d, want 3 (3d + 15d + 60d)", a.Changes90d)
	}
}

// TestCommitLogIncrementalAppend verifies that appendCommitLog writes the
// correct commit hash and path for each WriteFile/DeleteFile call, and that
// rowid order reflects write order (newer commit = higher rowid).
func TestCommitLogIncrementalAppend(t *testing.T) {
	dir := t.TempDir()
	store, err := Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if !store.commitLog {
		t.Skip("commit_log not available")
	}

	var countBefore int
	store.db.QueryRow(`SELECT COUNT(*) FROM commit_log`).Scan(&countBefore)

	h1, _, err := store.WriteFile("kb/a.md", "# A\n", "add a")
	if err != nil {
		t.Fatal(err)
	}
	h2, _, err := store.WriteFile("kb/b.md", "# B\n", "add b")
	if err != nil {
		t.Fatal(err)
	}

	var countAfter int
	store.db.QueryRow(`SELECT COUNT(*) FROM commit_log`).Scan(&countAfter)
	if countAfter != countBefore+2 {
		t.Errorf("commit_log rows: got %d, want %d", countAfter, countBefore+2)
	}

	// Each row must reference the correct commit hash.
	var gotH1, gotH2 string
	store.db.QueryRow(`SELECT commit_hash FROM commit_log WHERE path = 'kb/a.md'`).Scan(&gotH1)
	store.db.QueryRow(`SELECT commit_hash FROM commit_log WHERE path = 'kb/b.md'`).Scan(&gotH2)
	if gotH1 != h1 {
		t.Errorf("kb/a.md commit_hash = %q, want %q", gotH1, h1)
	}
	if gotH2 != h2 {
		t.Errorf("kb/b.md commit_hash = %q, want %q", gotH2, h2)
	}

	// rowid(a) < rowid(b) — b was written after a, so it must have higher rowid.
	var rowA, rowB int64
	store.db.QueryRow(`SELECT rowid FROM commit_log WHERE path = 'kb/a.md'`).Scan(&rowA)
	store.db.QueryRow(`SELECT rowid FROM commit_log WHERE path = 'kb/b.md'`).Scan(&rowB)
	if rowA >= rowB {
		t.Errorf("rowid ordering: rowid(a)=%d >= rowid(b)=%d; newer commit must have higher rowid", rowA, rowB)
	}
}

// TestPopulateCommitLogIsIncremental verifies that reopening a store with
// existing commit_log entries does not duplicate rows (the early-stop on
// already-indexed commits works correctly).
func TestPopulateCommitLogIsIncremental(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	store, err := Init(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/a.md", "# A\n", "add a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteFile("kb/b.md", "# B\n", "add b"); err != nil {
		t.Fatal(err)
	}

	var countFirst int
	store.db.QueryRow(`SELECT COUNT(*) FROM commit_log`).Scan(&countFirst)
	store.Close()

	// Reopen — populateCommitLog runs again.
	store2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	var countSecond int
	store2.db.QueryRow(`SELECT COUNT(*) FROM commit_log`).Scan(&countSecond)

	if countSecond != countFirst {
		t.Errorf("commit_log rows after re-open: got %d, want %d (duplicate insert?)", countSecond, countFirst)
	}
	if !store2.commitLog {
		t.Error("commitLog flag must be true after re-open")
	}
}

// TestAppendCommitLogDelete verifies that DeleteFile commits appear in
// commit_log with the deleted file's path.
func TestAppendCommitLogDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if !store.commitLog {
		t.Skip("commit_log not available")
	}

	h1, _, err := store.WriteFile("kb/del.md", "# Del\n", "add del")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := store.DeleteFile("kb/del.md", "delete del")
	if err != nil {
		t.Fatal(err)
	}

	rows, err := store.db.Query(
		`SELECT commit_hash FROM commit_log WHERE path = 'kb/del.md' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var hashes []string
	for rows.Next() {
		var h string
		rows.Scan(&h)
		hashes = append(hashes, h)
	}
	if len(hashes) != 2 {
		t.Fatalf("expected 2 commit_log rows for kb/del.md, got %d", len(hashes))
	}
	if hashes[0] != h1 || hashes[1] != h2 {
		t.Errorf("commit_log hashes = %v, want [%s, %s]", hashes, h1[:8], h2[:8])
	}
}

// TestActivityFallbackNoTable verifies that Activity and WalkChangedFiles fall
// back to go-git correctly when the commit_log table is absent.
func TestActivityFallbackNoTable(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Minimal schema — no commit_log table.
	const minSchema = `
CREATE TABLE IF NOT EXISTS objects (hash TEXT NOT NULL, type INTEGER NOT NULL, size INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY (hash, type));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target TEXT NOT NULL, is_symbolic INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value BLOB NOT NULL);`
	if _, err := db.Exec(minSchema); err != nil {
		t.Fatal(err)
	}

	s := storegit.NewStorer(db)
	store, err := InitWithStorer(s, nil, "agent/test")
	if err != nil {
		t.Fatal(err)
	}

	if store.commitLog {
		t.Error("commitLog should be false when commit_log table is absent")
	}

	if _, _, err := store.WriteFile("kb/a.md", "# A\n", "add a"); err != nil {
		t.Fatal(err)
	}

	// Activity must fall back to go-git and return correct count.
	a, err := store.Activity("kb")
	if err != nil {
		t.Fatal(err)
	}
	if a.Total != 1 {
		t.Errorf("go-git fallback Activity(\"kb\").Total = %d, want 1", a.Total)
	}

	// WalkChangedFiles must also fall back to go-git.
	files, _, err := store.WalkChangedFiles("", "kb", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Error("go-git fallback WalkChangedFiles should return results")
	}
}
