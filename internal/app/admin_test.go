package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/fact"
)

func TestInitRepo_OntologyPreset(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{Home: dir}
	if err := InitRepo(cfg, "test", "", "code"); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "repos", "test.db")); err != nil {
		t.Fatalf("expected DB at repos/test.db: %v", err)
	}
	// Verify the code preset serializes to contain "id: source-code"
	yaml, err := fact.CodeOntology().Serialize()
	if err != nil {
		t.Fatalf("CodeOntology().Serialize(): %v", err)
	}
	if !strings.Contains(string(yaml), "id: source-code") {
		t.Errorf("CodeOntology().Serialize() does not contain 'id: source-code'; got:\n%s", yaml)
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
