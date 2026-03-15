package store

import (
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
	if svc.GitStorer() == nil {
		t.Fatal("expected non-nil GitStorer")
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

	if svc.DB() == nil {
		t.Fatal("expected non-nil DB")
	}
}
