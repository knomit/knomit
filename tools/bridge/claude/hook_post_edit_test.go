package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// closedKnomit points KNOMIT_BASE_URL at an immediately-closed httptest server
// so every hook HTTP call fails fast with "connection refused", making the
// test deterministic regardless of whether a real knomit happens to be running
// on localhost.
func closedKnomit(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	t.Setenv("KNOMIT_BASE_URL", srv.URL)
}

func TestHookPostEdit_MalformedStdin_Clean(t *testing.T) {
	in := strings.NewReader(`not json`)
	var out bytes.Buffer
	if err := hookPostEdit(in, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for malformed stdin; got %q", out.String())
	}
}

func TestHookPostEdit_NonEditTool_Quiet(t *testing.T) {
	payload := map[string]interface{}{
		"tool_name": "Bash",
		"cwd":       "/Users/knomit/data/mine/knomit",
		"tool_input": map[string]interface{}{
			"file_path": "/Users/knomit/data/mine/knomit/internal/synthesize/weight.go",
		},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostEdit(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for non-edit tool; got %q", out.String())
	}
}

func TestHookPostEdit_PathOutsideCwd_Quiet(t *testing.T) {
	payload := map[string]interface{}{
		"tool_name":  "Edit",
		"cwd":        "/Users/knomit/data/mine/knomit",
		"tool_input": map[string]interface{}{"file_path": "/etc/hosts"},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostEdit(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for path outside cwd; got %q", out.String())
	}
}

func TestHookPostEdit_EmptyInputs_Quiet(t *testing.T) {
	payload := map[string]interface{}{
		"tool_name":  "Edit",
		"cwd":        "",
		"tool_input": map[string]interface{}{"file_path": ""},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostEdit(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for empty inputs; got %q", out.String())
	}
}

func TestHookPostEdit_ValidEditNoKnomit_Quiet(t *testing.T) {
	// With knomit unreachable, agentBranch returns "" and the hook exits
	// silently — same defensive pattern as hookSessionStart.
	closedKnomit(t)
	dir := t.TempDir()
	payload := map[string]interface{}{
		"tool_name": "Edit",
		"cwd":       dir,
		"tool_input": map[string]interface{}{
			"file_path": filepath.Join(dir, "internal/synthesize/weight.go"),
		},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostEdit(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output when knomit unreachable; got %q", out.String())
	}
}

// postEditHappyPath runs hookPostEdit against a stub knomit that returns one
// fact whose entities exact-match the edited file, and returns the raw stdout.
func postEditHappyPath(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	rel := "internal/store/foo.go"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/search"):
			// Search returns one candidate; entities exact-match rel.
			fmt.Fprintf(w, `{"_embedded":{"results":[
				{"path":"kb/invariants/store/foo.md","title":"Foo invariant","entities":[%q]}
			]}}`, rel)
		case strings.Contains(r.URL.Path, "/branches/"):
			// shouldn't be hit
			http.NotFound(w, r)
		default:
			// agentBranch lookup: GET /api/v1/repos/{repo}
			fmt.Fprint(w, `{"agent_branch":"machine/test"}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)

	payload := map[string]interface{}{
		"tool_name": "Edit",
		"cwd":       dir,
		"tool_input": map[string]interface{}{
			"file_path": filepath.Join(dir, rel),
		},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostEdit(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestHookPostEdit_HappyPath_EmitsNudge(t *testing.T) {
	out := postEditHappyPath(t)

	var resp struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\ngot: %s", err, out)
	}
	ctx := resp.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "kb/invariants/store/foo.md") {
		t.Errorf("nudge missing matched fact path: %q", ctx)
	}
	if !strings.Contains(ctx, "/knomit-update") {
		t.Errorf("nudge missing /knomit-update instruction: %q", ctx)
	}
}

// CC validates hookSpecificOutput and discards the whole payload — nudge and
// all — when hookEventName is absent, surfacing only "Hook JSON output
// validation failed". The name must match the event the hook is wired to.
func TestHookPostEdit_EmitsHookEventName(t *testing.T) {
	out := postEditHappyPath(t)

	var resp struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\ngot: %s", err, out)
	}
	if resp.HookSpecificOutput.HookEventName != "PostToolUse" {
		t.Errorf("hookEventName = %q, want %q", resp.HookSpecificOutput.HookEventName, "PostToolUse")
	}
}

func TestFilterByEntity(t *testing.T) {
	rel := "internal/store/foo.go"
	facts := []factSummary{
		{Path: "kb/a", Title: "bare exact", Entities: []string{rel}},
		{Path: "kb/b", Title: "src ref", Entities: []string{"src://knomit/" + rel}},
		{Path: "kb/c", Title: "src ref + commit", Entities: []string{"src://knomit/" + rel + "@abc123"}},
		{Path: "kb/d", Title: "symbol", Entities: []string{"Service.Verify"}},
		{Path: "kb/e", Title: "basename only", Entities: []string{"foo.go"}},
		{Path: "kb/f", Title: "different file", Entities: []string{"internal/store/bar.go"}},
		{Path: "kb/g", Title: "no-scheme false suffix", Entities: []string{"other/" + rel}},
		{Path: "kb/h", Title: "src diff path", Entities: []string{"src://knomit/internal/store/bar.go"}},
		{Path: "kb/i", Title: "multi-entity hit", Entities: []string{"Service.Verify", "src://knomit/" + rel}},
	}
	got := filterByEntity(facts, rel)
	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	want := []string{"kb/a", "kb/b", "kb/c", "kb/i"}
	if !equalStrings(paths, want) {
		t.Errorf("filterByEntity matched %v, want %v", paths, want)
	}
}

func TestRelPath_InsideCwd_ReturnsRelative(t *testing.T) {
	cwd := "/Users/knomit/data/mine/knomit"
	abs := "/Users/knomit/data/mine/knomit/internal/synthesize/weight.go"
	got := relPath(cwd, abs)
	want := "internal/synthesize/weight.go"
	if got != want {
		t.Errorf("relPath(%q, %q) = %q; want %q", cwd, abs, got, want)
	}
}

func TestRelPath_OutsideCwd_ReturnsEmpty(t *testing.T) {
	cwd := "/Users/knomit/data/mine/knomit"
	abs := "/etc/hosts"
	if got := relPath(cwd, abs); got != "" {
		t.Errorf("relPath outside cwd = %q; want empty", got)
	}
}

func TestRelPath_EmptyInputs_ReturnsEmpty(t *testing.T) {
	if got := relPath("", "/x"); got != "" {
		t.Errorf("relPath empty cwd = %q; want empty", got)
	}
	if got := relPath("/x", ""); got != "" {
		t.Errorf("relPath empty abs = %q; want empty", got)
	}
}

func TestRelPath_SameAsCwd_ReturnsDot(t *testing.T) {
	// filepath.Rel("/x", "/x") returns "." — we accept this as inside-cwd.
	cwd := "/Users/knomit/data/mine/knomit"
	if got := relPath(cwd, cwd); got != "." {
		t.Errorf("relPath of same paths = %q; want \".\"", got)
	}
}
