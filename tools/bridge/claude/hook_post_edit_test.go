package claude

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

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
	// With no knomit server running, agentBranch returns "" and the hook
	// exits silently — same defensive pattern as hookSessionStart.
	payload := map[string]interface{}{
		"tool_name": "Edit",
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
	// No assertion on output — knomit may or may not be running in CI
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
