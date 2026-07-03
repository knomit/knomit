package claw

import (
	"os"
	"path/filepath"
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
		profile: "code",
		scope:   "project",
		snapshot: func() ([]byte, string, error) {
			return []byte(`[{"name":"knomit_query","description":"q","inputSchema":{"type":"object"}}]`),
				"Operate the knomit knowledge base carefully.", nil
		},
	}
	if err := runInitWith(opts); err != nil {
		t.Fatalf("runInitWith: %v", err)
	}

	mustExist := []string{
		".agents/skills/knomit-recall/SKILL.md",
		".agents/skills/knomit-guidance/SKILL.md",
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
	// Guidance skill got the instructions substituted.
	b, _ := os.ReadFile(filepath.Join(dir, ".agents/skills/knomit-guidance/SKILL.md"))
	if !contains(string(b), "knowledge base carefully") {
		t.Errorf("guidance skill missing instructions: %s", b)
	}
	// Manifest landed next to the plugin.
	m, _ := os.ReadFile(filepath.Join(dir, "openclaw-plugins/knomit/knomit-tools.json"))
	if !contains(string(m), "knomit_query") {
		t.Errorf("manifest missing tool: %s", m)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
