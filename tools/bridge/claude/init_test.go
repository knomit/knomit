package claude

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

	// No .claude/hooks/ directory should be created
	if _, err := os.Stat(filepath.Join(dir, ".claude/hooks")); err == nil {
		t.Error(".claude/hooks/ should NOT be created; hooks are now in knomit-bridge binary")
	}
}

func TestRunInit_NoHooksDirectory(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	hooksDir := filepath.Join(dir, ".claude", "hooks")
	if _, err := os.Stat(hooksDir); err == nil {
		t.Errorf(".claude/hooks/ directory should not exist; got one at %s", hooksDir)
	}
}

func TestRunInit_SettingsJsonReferencesGoHooks(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	s, err := os.ReadFile(filepath.Join(dir, ".claude/settings.json"))
	if err != nil {
		t.Fatalf("cannot read settings.json: %v", err)
	}
	content := string(s)

	// Must reference the new Go-based hook commands
	if !strings.Contains(content, "knomit-bridge claude hook session-start") {
		t.Errorf("settings.json missing 'knomit-bridge claude hook session-start'; got:\n%s", content)
	}
	if !strings.Contains(content, "knomit-bridge claude hook post-commit") {
		t.Errorf("settings.json missing 'knomit-bridge claude hook post-commit'; got:\n%s", content)
	}
	if !strings.Contains(content, "knomit-bridge claude hook pre-compact") {
		t.Errorf("settings.json missing 'knomit-bridge claude hook pre-compact'; got:\n%s", content)
	}
	if !strings.Contains(content, "knomit-bridge claude hook stop") {
		t.Errorf("settings.json missing 'knomit-bridge claude hook stop'; got:\n%s", content)
	}

	// Must NOT reference old .sh paths
	if strings.Contains(content, ".sh") {
		t.Errorf("settings.json should not reference .sh files; got:\n%s", content)
	}
	if strings.Contains(content, "$CLAUDE_PROJECT_DIR") {
		t.Errorf("settings.json should not reference $CLAUDE_PROJECT_DIR; got:\n%s", content)
	}
	if strings.Contains(content, "SessionEnd") {
		t.Errorf("settings.json must not register SessionEnd; got:\n%s", content)
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

func TestRunInit_SkillFrontmatterMatchesDir(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	skills := map[string]string{
		".claude/skills/knomit-recall/SKILL.md":       "name: knomit-recall",
		".claude/skills/knomit-remember/SKILL.md":     "name: knomit-remember",
		".claude/skills/knomit-why/SKILL.md":          "name: knomit-why",
		".claude/skills/knomit-decided/SKILL.md":      "name: knomit-decided",
		".claude/skills/knomit-kickoff-area/SKILL.md": "name: knomit-kickoff-area",
		".claude/skills/knomit-review/SKILL.md":       "name: knomit-review",
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
