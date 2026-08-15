package claude

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---- repoFromMCP ----

func TestRepoFromMCP_NoFile_FallsBackToBasename(t *testing.T) {
	dir := t.TempDir()
	got := repoFromMCP(dir)
	want := filepath.Base(dir)
	if got != want {
		t.Errorf("repoFromMCP(%q) = %q, want %q", dir, got, want)
	}
}

func TestRepoFromMCP_ValidMcpJson_ReturnsRepoArg(t *testing.T) {
	dir := t.TempDir()
	mcp := `{
		"mcpServers": {
			"knomit": {
				"command": "knomit-bridge",
				"args": ["--repo", "myproject", "--profile", "code"]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	got := repoFromMCP(dir)
	if got != "myproject" {
		t.Errorf("repoFromMCP = %q, want %q", got, "myproject")
	}
}

func TestRepoFromMCP_NoKnomitServer_FallsBack(t *testing.T) {
	dir := t.TempDir()
	mcp := `{"mcpServers": {"other": {"command": "foo", "args": []}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	got := repoFromMCP(dir)
	want := filepath.Base(dir)
	if got != want {
		t.Errorf("repoFromMCP = %q, want basename %q", got, want)
	}
}

func TestRepoFromMCP_SingleDashRepo(t *testing.T) {
	dir := t.TempDir()
	mcp := `{
		"mcpServers": {
			"knomit": {
				"command": "knomit-bridge",
				"args": ["-repo", "singleproject"]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	got := repoFromMCP(dir)
	if got != "singleproject" {
		t.Errorf("repoFromMCP = %q, want %q", got, "singleproject")
	}
}

// ---- mcpBinding ----

func TestMcpBinding_RepoConfigured_RepoMode(t *testing.T) {
	dir := t.TempDir()
	mcp := `{
		"mcpServers": {
			"knomit": {
				"command": "knomit-bridge",
				"args": ["--repo", "myproject", "--source", "git", "--profile", "code"]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, lens, _ := mcpBinding(dir)
	if repo != "myproject" || lens != "" {
		t.Errorf("mcpBinding = (%q, %q), want (%q, %q)", repo, lens, "myproject", "")
	}
}

func TestMcpBinding_LensConfigured_LensMode(t *testing.T) {
	dir := t.TempDir()
	// Mirror the mcp.json.lens.tmpl shape: only --lens, no --repo.
	mcp := `{
		"mcpServers": {
			"knomit": {
				"command": "knomit-bridge",
				"args": ["--lens", "mylens"]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, lens, _ := mcpBinding(dir)
	if lens != "mylens" || repo != "" {
		t.Errorf("mcpBinding = (%q, %q), want (%q, %q)", repo, lens, "", "mylens")
	}
	// A lens-configured file must NEVER report the basename as a repo.
	if repo == filepath.Base(dir) {
		t.Errorf("lens config leaked basename %q as repo", repo)
	}
}

func TestMcpBinding_MissingFile_BasenameRepoMode(t *testing.T) {
	dir := t.TempDir()
	repo, lens, _ := mcpBinding(dir)
	if repo != filepath.Base(dir) || lens != "" {
		t.Errorf("mcpBinding = (%q, %q), want (%q, %q)", repo, lens, filepath.Base(dir), "")
	}
}

// TestMcpBinding_BothFlags_LensWins documents the defensive precedence rule for
// a hand-edited .mcp.json that `claude init` would refuse to produce: when both
// --lens and --repo appear, --lens wins regardless of order, so the session is
// never demoted to a raw repo scope.
func TestMcpBinding_BothFlags_LensWins(t *testing.T) {
	cases := map[string]string{
		"lens first": `["--lens", "mylens", "--repo", "myproject"]`,
		"repo first": `["--repo", "myproject", "--lens", "mylens"]`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			mcp := `{"mcpServers": {"knomit": {"command": "knomit-bridge", "args": ` + args + `}}}`
			if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcp), 0o644); err != nil {
				t.Fatal(err)
			}
			repo, lens, _ := mcpBinding(dir)
			if lens != "mylens" || repo != "" {
				t.Errorf("mcpBinding = (%q, %q), want lens to win (%q, %q)", repo, lens, "", "mylens")
			}
		})
	}
}

// TestMcpBinding_LensNoValue_LensModeEmptyName covers a hand-mangled config
// where --lens is present but carries no value. It must classify as lens mode
// (empty name) and must NOT leak the basename as a repo — otherwise the exact
// wrong-repo hazard B.6 removes would reappear.
func TestMcpBinding_LensNoValue_LensModeEmptyName(t *testing.T) {
	dir := t.TempDir()
	mcp := `{"mcpServers": {"knomit": {"command": "knomit-bridge", "args": ["--lens"]}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, lens, _ := mcpBinding(dir)
	if repo != "" || lens != "" {
		t.Errorf("mcpBinding = (%q, %q), want (%q, %q)", repo, lens, "", "")
	}
	if repo == filepath.Base(dir) {
		t.Errorf("degenerate --lens leaked basename %q as repo", repo)
	}
}

// TestMcpBinding_EqualsForms covers the Go-flag combined `--flag=value` forms,
// which the token-only classifier previously ignored — silently demoting a
// lens-configured session to basename repo mode. The `=` forms must behave
// identically to the space-separated token forms: any --lens=/-lens= means lens
// mode wins immediately (value after '=' may be empty → empty lens name), and
// --repo=/-repo= sets the repo (first occurrence wins).
func TestMcpBinding_EqualsForms(t *testing.T) {
	cases := []struct {
		name     string
		args     string // JSON array literal
		wantRepo string
		wantLens string
	}{
		{"--lens=eng", `["--lens=eng"]`, "", "eng"},
		{"-lens=eng", `["-lens=eng"]`, "", "eng"},
		{"--lens= empty value", `["--lens="]`, "", ""},
		{"--repo=work", `["--repo=work"]`, "work", ""},
		{"-repo=work", `["-repo=work"]`, "work", ""},
		{"repo= then lens= (lens wins)", `["--repo=x", "--lens=eng"]`, "", "eng"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mcp := `{"mcpServers": {"knomit": {"command": "knomit-bridge", "args": ` + tc.args + `}}}`
			if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcp), 0o644); err != nil {
				t.Fatal(err)
			}
			repo, lens, _ := mcpBinding(dir)
			if repo != tc.wantRepo || lens != tc.wantLens {
				t.Errorf("mcpBinding = (%q, %q), want (%q, %q)", repo, lens, tc.wantRepo, tc.wantLens)
			}
			// A lens-configured file must NEVER report the basename as a repo.
			if tc.wantRepo == "" && repo == filepath.Base(dir) {
				t.Errorf("lens config leaked basename %q as repo", repo)
			}
		})
	}
}

// ---- resolveWriteRepo ----

// TestResolveWriteRepo_LensNoValue_SkipsUnresolved confirms the degenerate
// --lens config fails safe (clean skip), never a basename fallback.
func TestResolveWriteRepo_LensNoValue_SkipsUnresolved(t *testing.T) {
	dir := t.TempDir()
	mcp := `{"mcpServers": {"knomit": {"command": "knomit-bridge", "args": ["--lens"]}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	closedKnomit(t)

	repo, skip := resolveWriteRepo(dir)
	if repo != "" || skip != "lens_unresolved" {
		t.Errorf("resolveWriteRepo = (%q, %q), want (%q, %q)", repo, skip, "", "lens_unresolved")
	}
}

func TestResolveWriteRepo_RepoMode_NoServerNeeded(t *testing.T) {
	dir := t.TempDir()
	mcp := `{"mcpServers": {"knomit": {"command": "knomit-bridge", "args": ["--repo", "myproject"]}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, skip := resolveWriteRepo(dir)
	if repo != "myproject" || skip != "" {
		t.Errorf("resolveWriteRepo = (%q, %q), want (%q, %q)", repo, skip, "myproject", "")
	}
}

func TestResolveWriteRepo_LensMode_ResolvesWriteRepo(t *testing.T) {
	dir := t.TempDir()
	writeLensMCP(t, dir, "mylens")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/lenses/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "mylens") {
			t.Errorf("lens GET path = %q, want it to contain lens name", r.URL.Path)
		}
		w.Write([]byte(`{"name":"mylens","write":{"uid":"uid-writerepo","name":"writerepo"},"reads":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)

	repo, skip := resolveWriteRepo(dir)
	if repo != "writerepo" || skip != "" {
		t.Errorf("resolveWriteRepo = (%q, %q), want (%q, %q)", repo, skip, "writerepo", "")
	}
}

func TestResolveWriteRepo_LensMode_404_SkipsUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeLensMCP(t, dir, "mylens")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)

	repo, skip := resolveWriteRepo(dir)
	if repo != "" || skip != "lens_unresolved" {
		t.Errorf("resolveWriteRepo = (%q, %q), want (%q, %q)", repo, skip, "", "lens_unresolved")
	}
}

func TestResolveWriteRepo_LensMode_ServerDown_SkipsUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeLensMCP(t, dir, "mylens")
	closedKnomit(t)

	repo, skip := resolveWriteRepo(dir)
	if repo != "" || skip != "lens_unresolved" {
		t.Errorf("resolveWriteRepo = (%q, %q), want (%q, %q)", repo, skip, "", "lens_unresolved")
	}
}

// writeLensMCP writes a lens-configured .mcp.json (only --lens) into dir,
// matching the mcp.json.lens.tmpl shape.
func writeLensMCP(t *testing.T, dir, lens string) {
	t.Helper()
	mcp := `{"mcpServers": {"knomit": {"command": "knomit-bridge", "args": ["--lens", "` + lens + `"]}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- emitAdditionalContext ----

func TestEmitAdditionalContext_Empty_NoOutput(t *testing.T) {
	var out bytes.Buffer
	if err := emitAdditionalContext(&out, "PostToolUse", ""); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for empty ctx; got %q", out.String())
	}
}

func TestEmitAdditionalContext_NonEmpty_ValidJSON(t *testing.T) {
	var out bytes.Buffer
	if err := emitAdditionalContext(&out, "Stop", "hello world"); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\ngot: %s", err, out.String())
	}
	if resp.HookSpecificOutput.AdditionalContext != "hello world" {
		t.Errorf("additionalContext = %q, want %q", resp.HookSpecificOutput.AdditionalContext, "hello world")
	}
	if resp.HookSpecificOutput.HookEventName != "Stop" {
		t.Errorf("hookEventName = %q, want %q", resp.HookSpecificOutput.HookEventName, "Stop")
	}
}

// ---- wiredEvent ----

func TestWiredEvent(t *testing.T) {
	for _, tc := range []struct{ name, fromInput, fallback, want string }{
		{"stdin wins", "PostToolBatch", "PostToolUse", "PostToolBatch"},
		{"stdin wins even when it equals the fallback", "PostToolUse", "PostToolUse", "PostToolUse"},
		{"fallback when stdin omitted the field", "", "PostToolUse", "PostToolUse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wiredEvent(tc.fromInput, tc.fallback); got != tc.want {
				t.Errorf("wiredEvent(%q, %q) = %q, want %q", tc.fromInput, tc.fallback, got, tc.want)
			}
		})
	}
}

// TestIsKnomitServer covers the two ways an entry is recognised. The KEY arm is
// the compatibility path: configs predating the derived key, or using a wrapper
// script / renamed symlink / `go run`, leave a command this cannot identify, and
// failing to match there does NOT fail safe — it falls through to the basename
// fallback, i.e. the wrong-repo hazard.
func TestIsKnomitServer(t *testing.T) {
	for _, tc := range []struct {
		name, key, command string
		want               bool
	}{
		{"plain command", "anything", "knomit-bridge", true},
		{"absolute command", "anything", "/usr/local/bin/knomit-bridge", true},
		// filepath.Base is OS-specific, so a backslash path only splits on
		// Windows. The portable assertion is the .exe suffix trimming itself,
		// which is what the GOOS=windows build needs.
		{"windows exe", "anything", "knomit-bridge.exe", true},
		{"windows exe with slash path", "anything", "C:/tools/knomit-bridge.exe", true},
		{"legacy constant key", "knomit", "/opt/wrappers/kb", true},
		{"derived key, wrapper command", "knomit-eng", "~/bin/kb-wrapper.sh", true},
		{"unrelated server", "postgres", "/usr/bin/pg-mcp", false},
		{"knomit-ish command but not the bridge", "db", "knomit-bridgehead", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isKnomitServer(tc.key, tc.command); got != tc.want {
				t.Errorf("isKnomitServer(%q, %q) = %v, want %v", tc.key, tc.command, got, tc.want)
			}
		})
	}
}

// TestMcpBinding_LegacyConfigStillBinds is the compatibility regression: a
// pre-existing config keyed "knomit" whose command is a wrapper must keep
// resolving to lens mode. Selecting on command alone silently demoted it to a
// basename repo — the hazard mcpBinding's contract forbids.
func TestMcpBinding_LegacyConfigStillBinds(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"mcpServers":{"knomit":{"command":"/opt/wrappers/kb","args":["--lens","eng"]}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, lens, ambiguous := mcpBinding(dir)
	if ambiguous {
		t.Fatal("single server reported ambiguous")
	}
	if lens != "eng" {
		t.Errorf("lens = %q, want %q", lens, "eng")
	}
	if repo != "" {
		t.Errorf("repo = %q, want empty — lens config must never fall back to a basename", repo)
	}
}

// TestHookSessionStart_MultipleServersTellsTheUser pins that the one skip the
// user cannot otherwise see is spoken aloud. Every other skip reason is
// transient; this one never resolves on its own.
func TestHookSessionStart_MultipleServersTellsTheUser(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"mcpServers":{
		"knomit-a":{"command":"knomit-bridge","args":["--repo","a"]},
		"knomit-b":{"command":"knomit-bridge","args":["--repo","b"]}
	}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(`{"cwd":` + strconv.Quote(dir) + `}`)
	var out bytes.Buffer
	if err := hookSessionStart(in, &out); err != nil {
		t.Fatalf("hookSessionStart: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "DISABLED") {
		t.Errorf("session-start stayed silent about disabled hooks; got %q", got)
	}
	if !strings.Contains(got, ".mcp.json") {
		t.Errorf("notice does not say where to fix it; got %q", got)
	}
}
