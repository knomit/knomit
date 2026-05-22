package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInit_EmptyDirectory_DropsAllFiles(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "testproj"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	wantFiles := []string{
		".mcp.json",
		".claude/settings.json",
		".claude/hooks/_knomit-helpers.sh",
		".claude/hooks/knomit-session-start.sh",
		".claude/hooks/knomit-post-commit.sh",
		".claude/hooks/knomit-pre-compact.sh",
		".claude/hooks/knomit-stop.sh",
		".claude/hooks/knomit-session-end.sh",
		".claude/skills/recall.md",
		".claude/skills/remember.md",
		".claude/skills/why.md",
		".claude/skills/decided.md",
		".claude/skills/kickoff-area.md",
		".claude/skills/review.md",
		"CLAUDE.md",
	}
	for _, f := range wantFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}

	// .mcp.json must have the repo name interpolated
	mcp, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(mcp), `"testproj"`) {
		t.Errorf(".mcp.json missing repo name; got:\n%s", mcp)
	}

	// Hooks must be executable
	info, _ := os.Stat(filepath.Join(dir, ".claude/hooks/knomit-session-start.sh"))
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("session-start hook not executable; mode=%v", info.Mode())
	}
}

func TestRunInit_ExistingMcpJson_DropsCompanion(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	companion := filepath.Join(dir, ".mcp.json.knomit")
	if _, err := os.Stat(companion); err != nil {
		t.Errorf("expected companion file at %s: %v", companion, err)
	}

	// Original .mcp.json should be untouched
	orig, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(orig), `"mcpServers":{}`) {
		t.Errorf("original .mcp.json was modified; got:\n%s", orig)
	}
}

func TestRunInit_ExistingClaudeMd_DropsBlockCompanion(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	companion := filepath.Join(dir, "CLAUDE.md.knomit-block")
	if _, err := os.Stat(companion); err != nil {
		t.Errorf("expected companion at %s: %v", companion, err)
	}
}

func TestRunInit_AlreadyIntegrated_ReportsAndSkips(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	// First run: full scaffold
	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit #1: %v", err)
	}
	// Modify a hook
	hookPath := filepath.Join(dir, ".claude/hooks/knomit-session-start.sh")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\necho modified\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Second run: should detect the marker and skip
	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit #2: %v", err)
	}

	// Modified hook should NOT have been overwritten
	got, _ := os.ReadFile(hookPath)
	if !strings.Contains(string(got), "modified") {
		t.Errorf("hook was overwritten on re-run; want skip")
	}
}

func TestRunInit_ProfileOverride_RendersIntoMcpJson(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x", "--profile", "chat"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	mcp, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(mcp), `"chat"`) {
		t.Errorf(".mcp.json missing chat profile; got:\n%s", mcp)
	}
}

func TestRunInit_InvalidProfile_Errors(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	err := runInit([]string{"--repo", "x", "--profile", "bogus"})
	if err == nil {
		t.Fatal("runInit with invalid profile = nil, want error")
	}
	if !strings.Contains(err.Error(), "invalid profile") {
		t.Errorf("error %q does not mention invalid profile", err)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
}
