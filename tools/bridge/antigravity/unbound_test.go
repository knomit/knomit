package antigravity

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/tools/bridge/knomitapi"
)

// ---------------------------------------------------------------------------
// Flagless init scaffolds an UNBOUND server (#341). The directory name may
// appear in the KEY, never in args, and the hook binds from args alone.
// ---------------------------------------------------------------------------

func namedWorkspace(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readServers(t *testing.T, ws string) map[string]struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
} {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, pluginDir, "mcp_config.json"))
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
	return cfg.MCPServers
}

func TestRunInit_NoFlags_WritesUnboundEntry(t *testing.T) {
	ws := namedWorkspace(t, "ingestion")
	chdir(t, ws)
	if err := runInit(nil); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	srv, ok := readServers(t, ws)["knomit-repo-ingestion"]
	if !ok {
		t.Fatalf("no knomit-repo-ingestion key; got %v", readServers(t, ws))
	}
	if srv.Args == nil || len(srv.Args) != 0 {
		t.Errorf("args = %#v, want [] — the directory name must never reach args", srv.Args)
	}

	// Producer/consumer: the hook reads init's own output as unbound.
	if repo, lens, skip := pluginBinding(filepath.Join(ws, pluginDir)); skip != skipUnbound {
		t.Errorf("pluginBinding = (%q, %q, %q), want skip %q", repo, lens, skip, skipUnbound)
	}
}

func TestRunInit_NoFlags_NeverFailsOnTheDirectoryName(t *testing.T) {
	for _, name := range []string{"My Project", strings.Repeat("d", 60)} {
		t.Run(name, func(t *testing.T) {
			ws := namedWorkspace(t, name)
			chdir(t, ws)
			if err := runInit(nil); err != nil {
				t.Fatalf("runInit in %q: %v", name, err)
			}
			if _, ok := readServers(t, ws)[knomitapi.UnboundServerKey(name)]; !ok {
				t.Errorf("mcp_config.json has no %q entry", knomitapi.UnboundServerKey(name))
			}
		})
	}
}

// A flagless re-init over a bound plugin keeps its scope: unbound applies only
// to a fresh scaffold.
func TestRunInit_NoFlags_ExistingScope_Kept(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first []string
		key   string
		args  []string
		scope string
	}{
		{"repo", []string{"--repo", "ingestion"}, "knomit-repo-ingestion", []string{"--repo", "ingestion"}, "repo ingestion"},
		{"lens", []string{"--lens", "eng"}, "knomit-lens-eng", []string{"--lens", "eng"}, "lens eng"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := namedWorkspace(t, "ingestion")
			chdir(t, ws)
			if err := runInit(tc.first); err != nil {
				t.Fatalf("first runInit: %v", err)
			}
			out := captureStdout(t, func() {
				if err := runInit(nil); err != nil {
					t.Fatalf("flagless re-init: %v", err)
				}
			})
			servers := readServers(t, ws)
			if len(servers) != 1 {
				t.Fatalf("servers = %v, want exactly %q", servers, tc.key)
			}
			if got := servers[tc.key].Args; strings.Join(got, " ") != strings.Join(tc.args, " ") {
				t.Errorf("args = %v, want %v — a flagless re-init unbound an existing scope", got, tc.args)
			}
			if !strings.Contains(out, "kept") || !strings.Contains(out, tc.scope) {
				t.Errorf("summary does not say the %s scope was kept:\n%s", tc.scope, out)
			}
		})
	}
}

// ---- hook: the binding comes from ARGS only ----

func TestPluginBinding_KeyLooksBound_ArgsUnbound(t *testing.T) {
	for _, cfg := range []string{
		`{"mcpServers":{"knomit-repo-ingestion":{"command":"kb","args":[]}}}`,
		`{"mcpServers":{"knomit-repo-ingestion":{"command":"kb"}}}`,
		`{"mcpServers":{"knomit-repo-ingestion":{"command":"kb","args":["http://127.0.0.1:8080"]}}}`,
	} {
		dir := filepath.Join(t.TempDir(), "ingestion")
		os.MkdirAll(dir, 0o755)
		writeConfig(t, dir, cfg)
		if repo, lens, skip := pluginBinding(dir); repo != "" || lens != "" || skip != skipUnbound {
			t.Errorf("pluginBinding(%s) = (%q, %q, %q), want unbound", cfg, repo, lens, skip)
		}
	}
}

func TestPluginBinding_UnboundBesideBound_IsAmbiguous(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{
		"knomit-repo-a":{"command":"kb","args":[]},
		"knomit-repo-b":{"command":"kb","args":["--repo","b"]}}}`)
	if _, _, skip := pluginBinding(dir); skip != skipAmbiguousBinding {
		t.Errorf("skip = %q, want %q", skip, skipAmbiguousBinding)
	}
}

// The hook injects the bind note, never the "Re-run init" notice.
func TestPreInvocation_Unbound_InjectsBindNote(t *testing.T) {
	ws := namedWorkspace(t, "ingestion")
	dir := filepath.Join(ws, PluginDir)
	os.MkdirAll(dir, 0o755)
	writeConfig(t, dir, `{"mcpServers":{"knomit-repo-ingestion":{"command":"kb","args":[]}}}`)
	chdir(t, dir)
	isolateCache(t)
	factsServer(t)

	msg := injected(t, run(t, map[string]any{"invocationNum": 0, "conversationId": "c1"}))
	if msg != knomitapi.UnboundNote {
		t.Errorf("notice = %q, want %q", msg, knomitapi.UnboundNote)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	return <-done
}
