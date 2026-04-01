package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestServiceOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	svc, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	if svc.Index() == nil {
		t.Fatal("expected non-nil Index")
	}
	if svc.Storer() == nil {
		t.Fatal("expected non-nil Storer")
	}

	// DB file should exist
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatal("database file not created")
	}
}

func TestServiceOpenIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	svc1, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	svc1.Close()

	// Reopen same DB
	svc2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	svc2.Close()
}

func TestServiceOpenMemory(t *testing.T) {
	svc, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	if svc.db == nil {
		t.Fatal("expected non-nil DB")
	}
}

// TestMigration_ReviewWorkItemsDepth verifies that opening an existing database
// whose review_work_items table lacks the depth column gets it added via migration.
func TestMigration_ReviewWorkItemsDepth(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")

	// Step 1: Create a DB with the OLD schema (no depth column).
	svc, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Drop and recreate review_work_items WITHOUT the depth column,
	// simulating an old database.
	db := svc.db
	if _, err := db.Exec(`DROP TABLE IF EXISTS review_work_items`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE review_work_items (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id  TEXT NOT NULL,
		step_type   TEXT NOT NULL,
		cluster_key TEXT NOT NULL,
		facts_json  TEXT NOT NULL,
		response    TEXT,
		priority    REAL NOT NULL,
		created_at  TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	svc.Close()

	// Step 2: Reopen — migration should add the depth column.
	svc2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open after migration failed: %v", err)
	}
	defer svc2.Close()

	// Step 3: Verify we can insert and read a work item with depth.
	idx := svc2.Index()
	sess, err := idx.CreatePipelineSession(ctx, "review", "main")
	if err != nil {
		t.Fatal(err)
	}
	err = idx.InsertPipelineWorkItem(ctx, PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   "distill",
		ClusterKey: "raptor-d2",
		FactsJSON:  "[]",
		Priority:   -2,
		Depth:      2,
	})
	if err != nil {
		t.Fatalf("InsertPipelineWorkItem with depth after migration: %v", err)
	}

	item, err := idx.NextPipelineWorkItem(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item == nil {
		t.Fatal("expected work item, got nil")
	}
	if item.Depth != 2 {
		t.Errorf("Depth = %d, want 2", item.Depth)
	}
}
