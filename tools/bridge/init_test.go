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
		".claude/skills/knomit-recall/SKILL.md",
		".claude/skills/knomit-remember/SKILL.md",
		".claude/skills/knomit-why/SKILL.md",
		".claude/skills/knomit-decided/SKILL.md",
		".claude/skills/knomit-kickoff-area/SKILL.md",
		".claude/skills/knomit-review/SKILL.md",
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

func TestRunInit_HooksDeleted_GetRestored(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit #1: %v", err)
	}
	hookPath := filepath.Join(dir, ".claude/hooks/knomit-session-start.sh")

	// User deletes the hook
	if err := os.Remove(hookPath); err != nil {
		t.Fatal(err)
	}

	// Re-running init must restore it
	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit #2: %v", err)
	}

	if _, err := os.Stat(hookPath); err != nil {
		t.Errorf("hook was not restored after re-run: %v", err)
	}
}

func TestRunInit_SkillsDeleted_GetRestored(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit #1: %v", err)
	}
	skillPath := filepath.Join(dir, ".claude/skills/knomit-recall/SKILL.md")

	// User deletes the skill
	if err := os.Remove(skillPath); err != nil {
		t.Fatal(err)
	}

	// Re-running init must restore it
	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit #2: %v", err)
	}

	if _, err := os.Stat(skillPath); err != nil {
		t.Errorf("skill was not restored after re-run: %v", err)
	}
}

func TestRunInit_DropsNoSessionEndHook(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	p := filepath.Join(dir, ".claude/hooks/knomit-session-end.sh")
	if _, err := os.Stat(p); err == nil {
		t.Errorf("session-end hook should NOT be created; SessionEnd is fire-and-forget per CC docs")
	}

	s, _ := os.ReadFile(filepath.Join(dir, ".claude/settings.json"))
	if strings.Contains(string(s), "SessionEnd") {
		t.Errorf("settings.json must not register SessionEnd; got:\n%s", s)
	}
}

func TestRunInit_SkillFrontmatterMatchesDir(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	skills := map[string]string{
		".claude/skills/knomit-recall/SKILL.md":        "name: knomit-recall",
		".claude/skills/knomit-remember/SKILL.md":      "name: knomit-remember",
		".claude/skills/knomit-why/SKILL.md":           "name: knomit-why",
		".claude/skills/knomit-decided/SKILL.md":       "name: knomit-decided",
		".claude/skills/knomit-kickoff-area/SKILL.md":  "name: knomit-kickoff-area",
		".claude/skills/knomit-review/SKILL.md":        "name: knomit-review",
	}
	for path, wantFrontmatter := range skills {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Errorf("cannot read %s: %v", path, err)
			continue
		}
		if !strings.Contains(string(data), wantFrontmatter) {
			t.Errorf("%s: frontmatter missing %q; got:\n%s", path, wantFrontmatter, data)
		}
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
