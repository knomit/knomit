package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

const pluginDir = ".agents/plugins/knomit"

func TestRunInit_ScaffoldsWholePluginTree(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "testproj"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	want := []string{
		"plugin.json",
		"mcp_config.json",
		"hooks.json",
		"rules/AGENTS.md",
		"skills/knomit-recall/SKILL.md",
		"skills/knomit-remember/SKILL.md",
		"skills/knomit-decided/SKILL.md",
		"skills/knomit-why/SKILL.md",
		"skills/knomit-update/SKILL.md",
		"skills/knomit-retract/SKILL.md",
		"skills/knomit-review/SKILL.md",
		"skills/knomit-harden/SKILL.md",
		"skills/knomit-hypothesize/SKILL.md",
		"skills/knomit-principle/SKILL.md",
	}
	for _, f := range want {
		if _, err := os.Stat(filepath.Join(dir, pluginDir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
}

// The scaffold must be self-contained: nothing outside the plugin directory.
func TestRunInit_TouchesNothingOutsidePluginDir(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "testproj"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	for _, forbidden := range []string{"AGENTS.md", "GEMINI.md", ".mcp.json", ".agents/mcp_config.json", ".agents/hooks.json"} {
		if _, err := os.Stat(filepath.Join(dir, forbidden)); err == nil {
			t.Errorf("%s should not be written", forbidden)
		}
	}
}

// The hook reads the mcp_config.json next to its hooks.json, so init must
// produce a file that binds to exactly the requested repo.
func TestRunInit_ScaffoldedConfigBindsHook(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "testproj"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, pluginDir, "mcp_config.json"))
	if err != nil {
		t.Fatalf("read mcp_config.json: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("mcp_config.json is not valid JSON: %v\n%s", err, b)
	}
	srv, ok := cfg.MCPServers["knomit-repo-testproj"]
	if !ok {
		t.Fatalf("no knomit-repo-testproj key; got %v", cfg.MCPServers)
	}
	if srv.Command != "knomit-bridge" {
		t.Errorf("command = %q, want knomit-bridge", srv.Command)
	}
	if len(srv.Args) != 2 || srv.Args[0] != "--repo" || srv.Args[1] != "testproj" {
		t.Errorf("args = %v, want [--repo testproj]", srv.Args)
	}
}

func TestRunInit_LensMode_WritesLensConfig(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--lens", "eng"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	b, _ := os.ReadFile(filepath.Join(dir, pluginDir, "mcp_config.json"))
	if !strings.Contains(string(b), `"knomit-lens-eng"`) {
		t.Errorf("missing lens server key; got:\n%s", b)
	}
	if !strings.Contains(string(b), `"--lens"`) {
		t.Errorf("missing --lens arg; got:\n%s", b)
	}
}

// Every file is owned: a second run overwrites, never drops a companion.
func TestRunInit_Rerun_OverwritesAndLeavesNoCompanions(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "testproj"}); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	hooks := filepath.Join(dir, pluginDir, "hooks.json")
	if err := os.WriteFile(hooks, []byte("{}"), 0o644); err != nil {
		t.Fatalf("clobber: %v", err)
	}
	if err := runInit([]string{"--repo", "testproj"}); err != nil {
		t.Fatalf("second runInit: %v", err)
	}

	b, _ := os.ReadFile(hooks)
	if !strings.Contains(string(b), "pre-invocation") {
		t.Errorf("hooks.json not restored on re-run; got %s", b)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, pluginDir))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".knomit") {
			t.Errorf("companion file %s written; this host merges nothing", e.Name())
		}
	}
}

func TestRunInit_InvalidNames_RejectBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bad repo", []string{"--repo", "Not Valid"}},
		{"bad lens", []string{"--lens", `"quoted"`}},
		{"both flags", []string{"--repo", "a", "--lens", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			chdir(t, dir)
			if err := runInit(tc.args); err == nil {
				t.Fatal("expected error")
			}
			if _, err := os.Stat(filepath.Join(dir, ".agents")); err == nil {
				t.Error(".agents/ created despite validation failure")
			}
		})
	}
}

// init must tell the user about the registration requirement, since a headless
// run without --add-dir silently loads no knomit context at all.
func TestRunInit_PrintsRegistrationNotice(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := runInit([]string{"--repo", "testproj"})
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	got := string(buf[:n])

	if !strings.Contains(got, "--add-dir") {
		t.Errorf("output must mention --add-dir; got:\n%s", got)
	}
}

// REGRESSION: init used to enforce knomitapi.MaxServerKeyLen, a Claude Code
// tool-name budget, so a flagless `agy init` in any directory of 28+ characters
// aborted with an error about a host that was not even involved. Antigravity
// exposes bare tool names, so the bound does not apply here.
func TestRunInit_LongScopeName_Accepted(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	long := strings.Repeat("a", 40)
	if err := runInit([]string{"--repo", long}); err != nil {
		t.Fatalf("runInit rejected a valid %d-char repo name: %v", len(long), err)
	}
	b, err := os.ReadFile(filepath.Join(dir, pluginDir, "mcp_config.json"))
	if err != nil {
		t.Fatalf("read mcp_config.json: %v", err)
	}
	if !strings.Contains(string(b), "knomit-repo-"+long) {
		t.Errorf("config missing the long scope key:\n%s", b)
	}
}

// REGRESSION: every scaffolded file used to go through text/template, so a
// literal `{{` anywhere in a SKILL.md aborted init for BOTH hosts after some
// files had already been written. Only *.tmpl is templated now.
func TestRunInit_LiteralBracesInSkillProseDoNotBreakScaffolding(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "proj"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	// Every shipped skill must have landed, including any carrying brace-heavy
	// prose. A template parse error would have aborted before writing them all.
	for _, name := range []string{"knomit-review", "knomit-recall", "knomit-principle"} {
		p := filepath.Join(dir, pluginDir, "skills", name, "SKILL.md")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}
