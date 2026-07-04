package claw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInitScaffolds(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	_ = os.Chdir(dir)

	// Inject snapshot + instructions so the test needs no server.
	opts := initOptions{
		repo:    "myrepo",
		source:  "myrepo",
		profile: "generic",
		scope:   "project",
		snapshot: func() ([]byte, error) {
			return []byte(`[{"name":"knomit_query","description":"q","inputSchema":{"type":"object"}}]`), nil
		},
	}
	if err := runInitWith(opts); err != nil {
		t.Fatalf("runInitWith: %v", err)
	}

	mustExist := []string{
		".agents/skills/knomit-recall/SKILL.md",
		".agents/skills/knomit-remember/SKILL.md",
		"openclaw-plugins/knomit/index.mjs",
		"openclaw-plugins/knomit/register.mjs",
		"openclaw-plugins/knomit/knomit-tools.json",
		"openclaw-plugins/knomit/bridge-config.json",
		"openclaw-plugins/knomit/openclaw.plugin.json",
	}
	for _, p := range mustExist {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}
	// The invented knomit-guidance skill must NOT be scaffolded.
	if _, err := os.Stat(filepath.Join(dir, ".agents/skills/knomit-guidance/SKILL.md")); err == nil {
		t.Errorf("knomit-guidance skill should not exist (removed)")
	}
	// Manifest landed next to the plugin.
	m, _ := os.ReadFile(filepath.Join(dir, "openclaw-plugins/knomit/knomit-tools.json"))
	if !strings.Contains(string(m), "knomit_query") {
		t.Errorf("manifest missing tool: %s", m)
	}
}

func TestRunInitScaffoldsUserScope(t *testing.T) {
	projDir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	_ = os.Chdir(projDir)

	home := t.TempDir()
	t.Setenv("HOME", home)

	opts := initOptions{
		repo:    "myrepo",
		source:  "myrepo",
		profile: "generic",
		scope:   "user",
		snapshot: func() ([]byte, error) {
			return []byte(`[{"name":"knomit_query","description":"q","inputSchema":{"type":"object"}}]`), nil
		},
	}
	if err := runInitWith(opts); err != nil {
		t.Fatalf("runInitWith: %v", err)
	}

	mustExist := []string{
		filepath.Join(home, ".openclaw", "skills", "knomit-recall", "SKILL.md"),
		filepath.Join(home, ".openclaw", "extensions", "knomit", "index.mjs"),
		filepath.Join(home, ".openclaw", "extensions", "knomit", "bridge-config.json"),
		filepath.Join(home, ".openclaw", "openclaw.json"),
	}
	for _, p := range mustExist {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}

	// Must NOT have written to a bare ~/openclaw.json.
	if _, err := os.Stat(filepath.Join(home, "openclaw.json")); err == nil {
		t.Errorf("unexpected file at bare %s/openclaw.json; user-scope config must land under .openclaw/", home)
	}
}

func TestRunInitCompanionOnConflict(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	_ = os.Chdir(dir)

	sentinel := "SENTINEL: hand-edited config, do not overwrite"
	if err := os.WriteFile(filepath.Join(dir, "openclaw.json"), []byte(sentinel), 0o644); err != nil {
		t.Fatalf("pre-create openclaw.json: %v", err)
	}

	opts := initOptions{
		repo:    "myrepo",
		source:  "myrepo",
		profile: "generic",
		scope:   "project",
		snapshot: func() ([]byte, error) {
			return []byte(`[{"name":"knomit_query","description":"q","inputSchema":{"type":"object"}}]`), nil
		},
	}
	if err := runInitWith(opts); err != nil {
		t.Fatalf("runInitWith: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "openclaw.json"))
	if err != nil {
		t.Fatalf("read openclaw.json: %v", err)
	}
	if string(b) != sentinel {
		t.Errorf("original openclaw.json was modified; got %q, want %q", string(b), sentinel)
	}

	if _, err := os.Stat(filepath.Join(dir, "openclaw.json.knomit")); err != nil {
		t.Errorf("expected openclaw.json.knomit companion: %v", err)
	}
}
