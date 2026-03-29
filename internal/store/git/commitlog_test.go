package git_test

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	storegit "knomit/internal/store/git"
	storemigrate "knomit/internal/store/migrate"
)

// newCommitLogTestStorer opens an in-memory SQLite DB with the full Core schema
// (including branches and commit_log tables) and returns a Storer + DB.
func newCommitLogTestStorer(t *testing.T) (*storegit.Storer, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := storemigrate.Core(db); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return storegit.NewStorer(db), db
}

func TestCommitLogSync_BranchID_Set(t *testing.T) {
	s, db := newCommitLogTestStorer(t)

	if !s.CommitLogAvailable() {
		t.Fatal("commit_log table should be available after Core migration")
	}

	// Insert a branch row.
	if _, err := db.Exec(`INSERT INTO branches (name, git_ref) VALUES (?, ?)`, "agent/test", "refs/heads/agent/test"); err != nil {
		t.Fatal(err)
	}
	var wantBranchID int64
	if err := db.QueryRow(`SELECT id FROM branches WHERE name = ?`, "agent/test").Scan(&wantBranchID); err != nil {
		t.Fatal(err)
	}

	done := false
	err := s.CommitLogSync("agent/test", func() (string, []storegit.CommitLogEntry, error) {
		if done {
			return "", nil, nil
		}
		done = true
		return "aa00000000000000000000000000000000000001", []storegit.CommitLogEntry{{
			Hash:        "aa00000000000000000000000000000000000001",
			Path:        "kb/test.md",
			Message:     "test commit",
			Operation:   "learn",
			AuthorEmail: "agent+learn@agents.knomit.io",
			Action:      "added",
			CommittedAt: 1700000000,
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var gotBranchID sql.NullInt64
	if err := db.QueryRow(`SELECT branch_id FROM commit_log WHERE commit_hash = ?`, "aa00000000000000000000000000000000000001").Scan(&gotBranchID); err != nil {
		t.Fatal(err)
	}
	if !gotBranchID.Valid {
		t.Fatal("expected branch_id to be set, got NULL")
	}
	if gotBranchID.Int64 != wantBranchID {
		t.Errorf("branch_id = %d, want %d", gotBranchID.Int64, wantBranchID)
	}
}

func TestCommitLogSync_EmptyBranch_NullBranchID(t *testing.T) {
	s, db := newCommitLogTestStorer(t)

	if !s.CommitLogAvailable() {
		t.Fatal("commit_log table should be available after Core migration")
	}

	done := false
	err := s.CommitLogSync("", func() (string, []storegit.CommitLogEntry, error) {
		if done {
			return "", nil, nil
		}
		done = true
		return "bb00000000000000000000000000000000000001", []storegit.CommitLogEntry{{
			Hash:        "bb00000000000000000000000000000000000001",
			Path:        "kb/empty.md",
			Message:     "empty branch",
			Operation:   "learn",
			AuthorEmail: "agent+learn@agents.knomit.io",
			Action:      "added",
			CommittedAt: 1700000000,
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var gotBranchID sql.NullInt64
	if err := db.QueryRow(`SELECT branch_id FROM commit_log WHERE commit_hash = ?`, "bb00000000000000000000000000000000000001").Scan(&gotBranchID); err != nil {
		t.Fatal(err)
	}
	if gotBranchID.Valid {
		t.Errorf("expected NULL branch_id for empty branch name, got %d", gotBranchID.Int64)
	}
}

func TestCommitLogSync_UnknownBranch_NullBranchID(t *testing.T) {
	s, db := newCommitLogTestStorer(t)

	if !s.CommitLogAvailable() {
		t.Fatal("commit_log table should be available after Core migration")
	}

	done := false
	err := s.CommitLogSync("nonexistent-branch", func() (string, []storegit.CommitLogEntry, error) {
		if done {
			return "", nil, nil
		}
		done = true
		return "cc00000000000000000000000000000000000001", []storegit.CommitLogEntry{{
			Hash:        "cc00000000000000000000000000000000000001",
			Path:        "kb/unknown.md",
			Message:     "unknown branch",
			Operation:   "learn",
			AuthorEmail: "agent+learn@agents.knomit.io",
			Action:      "added",
			CommittedAt: 1700000000,
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var gotBranchID sql.NullInt64
	if err := db.QueryRow(`SELECT branch_id FROM commit_log WHERE commit_hash = ?`, "cc00000000000000000000000000000000000001").Scan(&gotBranchID); err != nil {
		t.Fatal(err)
	}
	if gotBranchID.Valid {
		t.Errorf("expected NULL branch_id for unknown branch, got %d", gotBranchID.Int64)
	}
}
