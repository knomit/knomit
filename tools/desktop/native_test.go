//go:build desktop

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFile_AbsoluteOnly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "note.txt")

	if err := writeFile(target, "hello"); err != nil {
		t.Fatalf("absolute write failed: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "hello" {
		t.Fatalf("file not written: %v %q", err, got)
	}

	if err := writeFile("relative.txt", "x"); err == nil {
		t.Error("relative path must be rejected")
	}
}
