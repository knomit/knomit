package antigravity

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// factsServer stands up a knomit stub serving agent_branch plus one global
// principle.
func factsServer(t *testing.T) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/facts") {
			w.Header().Set("Content-Type", "application/hal+json")
			w.Write([]byte(`{"_embedded":{"facts":[
				{"path":"kb/principles/philosophy/review-is-a-gate/x.md","title":"Review is a gate, not a loop","domain":["global"],"entities":["designer"]}
			]}}`))
			return
		}
		w.Write([]byte(`{"agent_branch":"machine/test"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)
}

// isolateCache redirects the marker cache root. BOTH HOME and XDG_CACHE_HOME
// are set because os.UserCacheDir honours XDG_CACHE_HOME on Linux but returns
// $HOME/Library/Caches on darwin and ignores it — redirecting only one leaves
// the tests asserting about a directory the code never writes to.
func isolateCache(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg"))
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("UserCacheDir: %v", err)
	}
	return cache
}

// boundPlugin creates a real plugin tree bound to repo "proj", chdirs into the
// plugin directory (where agy runs the hook), and isolates the marker cache.
func boundPlugin(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	dir := filepath.Join(ws, PluginDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--repo","proj"]}}}`)
	chdir(t, dir)
	isolateCache(t)
	return dir
}

func run(t *testing.T, payload map[string]any) string {
	t.Helper()
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPreInvocation(bytes.NewReader(data), &out); err != nil {
		t.Fatalf("hookPreInvocation returned an error; hooks must never fail: %v", err)
	}
	return out.String()
}

func runRaw(t *testing.T, body string) string {
	t.Helper()
	var out bytes.Buffer
	if err := hookPreInvocation(strings.NewReader(body), &out); err != nil {
		t.Fatalf("hookPreInvocation returned an error; hooks must never fail: %v", err)
	}
	return out.String()
}

// assertEmpty requires the exact no-op envelope: a bare JSON object.
func assertEmpty(t *testing.T, got string) {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("output is not valid JSON: %v (%q)", err, got)
	}
	if len(v) != 0 {
		t.Errorf("output = %q, want an empty object", got)
	}
}

func injected(t *testing.T, got string) string {
	t.Helper()
	var out struct {
		InjectSteps []struct {
			EphemeralMessage string `json:"ephemeralMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("bad JSON: %v (%q)", err, got)
	}
	if len(out.InjectSteps) != 1 {
		t.Fatalf("got %d injectSteps, want 1: %s", len(out.InjectSteps), got)
	}
	return out.InjectSteps[0].EphemeralMessage
}

func TestPreInvocation_FirstInvocation_Injects(t *testing.T) {
	boundPlugin(t)
	factsServer(t)

	msg := injected(t, run(t, map[string]any{"invocationNum": 0, "conversationId": "c1"}))
	if !strings.Contains(msg, "Known facts from knomit") {
		t.Errorf("message missing header: %q", msg)
	}
	if !strings.Contains(msg, "Review is a gate, not a loop") {
		t.Errorf("message missing the principle: %q", msg)
	}
}

func TestPreInvocation_LaterInvocation_Empty(t *testing.T) {
	boundPlugin(t)
	factsServer(t)
	assertEmpty(t, run(t, map[string]any{"invocationNum": 1, "conversationId": "c1"}))
}

// REGRESSION: invocationNum absent (a renamed, restructured or snake_case
// field) used to decode to 0 and read as "first invocation". Combined with an
// absent conversationId it failed OPEN — the corpus was injected on EVERY call.
// Both guards must now fail closed.
func TestPreInvocation_MissingInvocationNum_FailsClosed(t *testing.T) {
	boundPlugin(t)
	factsServer(t)
	for _, body := range []string{
		`{"conversationId":"c1"}`,
		`{"invocation_num":7,"conversation_id":"c9"}`,
		`{}`,
	} {
		t.Run(body, func(t *testing.T) {
			assertEmpty(t, runRaw(t, body))
			// And again — a fail-open bug shows up as repeated injection.
			assertEmpty(t, runRaw(t, body))
		})
	}
}

// REGRESSION: an absent conversationId used to make markerPath return "",
// which skipped the marker check entirely rather than failing closed. With
// nothing to key a marker on, the guard cannot function and must stay silent.
func TestPreInvocation_MissingConversationID_FailsClosed(t *testing.T) {
	boundPlugin(t)
	factsServer(t)
	assertEmpty(t, run(t, map[string]any{"invocationNum": 0}))
	assertEmpty(t, run(t, map[string]any{"invocationNum": 0, "conversationId": ""}))
}

// REGRESSION: the marker filename used to be the raw id, restricted to
// [A-Za-z0-9_-], and anything else was a hard skip. The id format is an
// unverified beta-API detail, so a timestamped or dotted id would have
// disabled the greeting for every conversation. Hashing greets each of these
// exactly once instead.
func TestPreInvocation_ExoticConversationID_GreetsOnce(t *testing.T) {
	boundPlugin(t)
	factsServer(t)
	for _, id := range []string{
		"conv_2026-08-18T10:30:00Z",
		"../../escape",
		"a/b",
		"CON",
		"café",
		strings.Repeat("x", 200),
	} {
		t.Run(id, func(t *testing.T) {
			if !strings.Contains(run(t, map[string]any{"invocationNum": 0, "conversationId": id}), "injectSteps") {
				t.Error("first invocation should be greeted")
			}
			assertEmpty(t, run(t, map[string]any{"invocationNum": 0, "conversationId": id}))
		})
	}
}

func TestPreInvocation_SecondCallSameConversation_Empty(t *testing.T) {
	boundPlugin(t)
	factsServer(t)

	if !strings.Contains(run(t, map[string]any{"invocationNum": 0, "conversationId": "c1"}), "injectSteps") {
		t.Fatal("first call should inject")
	}
	assertEmpty(t, run(t, map[string]any{"invocationNum": 0, "conversationId": "c1"}))
}

func TestPreInvocation_DifferentConversation_Injects(t *testing.T) {
	boundPlugin(t)
	factsServer(t)

	run(t, map[string]any{"invocationNum": 0, "conversationId": "c1"})
	if !strings.Contains(run(t, map[string]any{"invocationNum": 0, "conversationId": "c2"}), "injectSteps") {
		t.Error("a new conversation should inject")
	}
}

func TestPreInvocation_MalformedStdin_Empty(t *testing.T) {
	boundPlugin(t)
	assertEmpty(t, runRaw(t, "{not json"))
}

func TestPreInvocation_ServerDown_Empty(t *testing.T) {
	boundPlugin(t)
	t.Setenv("KNOMIT_BASE_URL", "http://127.0.0.1:1")
	assertEmpty(t, run(t, map[string]any{"invocationNum": 0, "conversationId": "c1"}))
}

func TestPreInvocation_NoFacts_Empty(t *testing.T) {
	boundPlugin(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/facts") {
			w.Write([]byte(`{"_embedded":{"facts":[]}}`))
			return
		}
		w.Write([]byte(`{"agent_branch":"machine/test"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)

	assertEmpty(t, run(t, map[string]any{"invocationNum": 0, "conversationId": "c1"}))
}

// REGRESSION: the never-resolving misconfigurations used to be completely
// silent — `{}` and a line in a log file the user never reads.
func TestPreInvocation_Misconfiguration_SaysSoOutLoud(t *testing.T) {
	for _, tc := range []struct{ name, config, want string }{
		{"ambiguous", `{"mcpServers":{
			"knomit-repo-alpha":{"command":"knomit-bridge","args":["--repo","alpha"]},
			"knomit-repo-beta":{"command":"knomit-bridge","args":["--repo","beta"]}}}`, "more than one"},
		{"degenerate lens", `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--lens"]}}}`, "--lens flag with no value"},
		{"invalid scope", `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--repo","../evil"]}}}`, "not a valid knomit name"},
		{"no knomit server", `{"mcpServers":{"other":{"command":"something-else"}}}`, "no usable knomit server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			dir := filepath.Join(ws, PluginDir)
			os.MkdirAll(dir, 0o755)
			writeConfig(t, dir, tc.config)
			chdir(t, dir)
			isolateCache(t)
			factsServer(t)

			msg := injected(t, run(t, map[string]any{"invocationNum": 0, "conversationId": "c1"}))
			if !strings.Contains(msg, tc.want) {
				t.Errorf("notice %q does not mention %q", msg, tc.want)
			}
		})
	}
}

// REGRESSION: the hook used to trust os.Getwd() unconditionally, so being run
// from anywhere but the plugin dir went permanently and silently dark.
func TestPreInvocation_RunFromWorkspaceRoot_StillBinds(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, PluginDir)
	os.MkdirAll(dir, 0o755)
	writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--repo","proj"]}}}`)
	chdir(t, ws) // workspace root, NOT the plugin dir
	isolateCache(t)
	factsServer(t)

	if !strings.Contains(run(t, map[string]any{"invocationNum": 0, "conversationId": "c1"}), "Known facts from knomit") {
		t.Error("hook should locate the plugin dir from the workspace root")
	}
}

// REGRESSION: locatePluginDir accepted any cwd merely NAMED "knomit" and
// returned early, so a workspace directory called knomit (this repo's own
// checkout is .../knomit/knomit) resolved to the workspace root, skipped the
// remaining probes, and greeted the user on every conversation with a
// misconfiguration notice no re-run could fix.
func TestPreInvocation_WorkspaceNamedKnomit_StillBinds(t *testing.T) {
	ws := filepath.Join(t.TempDir(), pluginDirName)
	dir := filepath.Join(ws, PluginDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--repo","proj"]}}}`)
	chdir(t, ws) // a workspace root that shares the plugin dir's name
	isolateCache(t)
	factsServer(t)

	got := run(t, map[string]any{"invocationNum": 0, "conversationId": "c1"})
	if !strings.Contains(got, "Known facts from knomit") {
		t.Errorf("a workspace named %q must not shadow its own plugin dir; got %q", pluginDirName, got)
	}
}

func TestPreInvocation_UsesWorkspacePathsWhenCwdIsUnrelated(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, PluginDir)
	os.MkdirAll(dir, 0o755)
	writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--repo","proj"]}}}`)
	chdir(t, t.TempDir()) // somewhere else entirely
	isolateCache(t)
	factsServer(t)

	got := run(t, map[string]any{"invocationNum": 0, "conversationId": "c1", "workspacePaths": []string{ws}})
	if !strings.Contains(got, "Known facts from knomit") {
		t.Errorf("hook should fall back to workspacePaths; got %q", got)
	}
}

func TestPreInvocation_NoPluginDirAnywhere_SaysSo(t *testing.T) {
	chdir(t, t.TempDir())
	isolateCache(t)
	msg := injected(t, run(t, map[string]any{"invocationNum": 0, "conversationId": "c1"}))
	if !strings.Contains(msg, "plugin directory") {
		t.Errorf("notice should name the problem; got %q", msg)
	}
}

// A conversation id is opaque agent-supplied text, so a traversal-shaped one
// must not steer the marker write outside the cache directory. It is hashed,
// so the filename is fixed-length hex no matter what the id contains.
func TestPreInvocation_TraversalConversationID_WritesNoMarkerOutside(t *testing.T) {
	boundPlugin(t)
	factsServer(t)
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("UserCacheDir: %v", err)
	}

	run(t, map[string]any{"invocationNum": 0, "conversationId": "../../escape"})

	// The path a raw-id implementation would produce: filepath.Join CLEANS the
	// result, so "../../escape" collapses to <cache>/escape.
	if _, err := os.Stat(filepath.Join(cache, "escape")); err == nil {
		t.Error("marker escaped into the cache root")
	}

	sessions := filepath.Join(cache, "knomit", "agy-sessions")
	got := markerPath("../../escape")
	if filepath.Dir(got) != sessions {
		t.Errorf("markerPath(%q) = %q, want a file directly under %q", "../../escape", got, sessions)
	}
	if name := filepath.Base(got); len(name) != 64 || strings.Trim(name, "0123456789abcdef") != "" {
		t.Errorf("marker filename %q is not a sha256 hex digest", name)
	}
}

// The once-per-conversation guard needs distinct ids to land on distinct
// markers; a hash collision here would greet one conversation and silence the
// other for its whole life.
func TestMarkerPath_DistinctIDsDistinctMarkers(t *testing.T) {
	if markerPath("c1") == markerPath("c2") {
		t.Error("distinct conversation ids must not share a marker")
	}
	if markerPath("") != "" {
		t.Error("an empty id has nothing to key on and must yield no marker")
	}
}
