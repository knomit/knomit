package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

func TestInitRepo_OntologyPreset(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{Home: dir}
	if err := InitRepo(cfg, "test", "", "code"); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	dbPath := filepath.Join(dir, "repos", "test.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected DB at repos/test.db: %v", err)
	}
	// Read back the persisted ontology from git to verify InitRepo wrote the
	// correct preset, not the default.
	svc, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer svc.Close()
	if err := svc.OpenRepo(); err != nil {
		t.Fatalf("OpenRepo: %v", err)
	}
	ctx := context.Background()
	branch, err := svc.Branches().DefaultBranch(ctx)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	result, err := svc.Facts().ReadFact(ctx, branch, "domains/ontology.yaml", nil)
	if err != nil {
		t.Fatalf("ReadFact domains/ontology.yaml: %v", err)
	}
	onto, err := fact.ParseOntology([]byte(result.Content))
	if err != nil {
		t.Fatalf("ParseOntology: %v", err)
	}
	if onto.ID != "source-code" {
		t.Errorf("persisted ontology ID = %q, want %q", onto.ID, "source-code")
	}
}

func TestInitRepo_RejectsBothPathAndPreset(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{Home: dir}
	tmpfile := filepath.Join(dir, "x.yaml")
	if err := os.WriteFile(tmpfile, []byte("id: x\nname: x\ntopics:\n  a: {description: a}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := InitRepo(cfg, "test", tmpfile, "code")
	if err == nil {
		t.Fatal("InitRepo with both path and preset = nil, want error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q does not mention mutual exclusion", err)
	}
}
