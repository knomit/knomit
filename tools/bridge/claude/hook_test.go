package claude

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// ---- hookPostCommit ----

func TestHookPostCommit_MalformedStdin_Clean(t *testing.T) {
	in := strings.NewReader(`not json`)
	var out bytes.Buffer
	if err := hookPostCommit(in, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for malformed stdin; got %q", out.String())
	}
}

func TestHookPostCommit_NonCommitCommand_Quiet(t *testing.T) {
	in := strings.NewReader(`{"tool_input":{"command":"ls -la"},"tool_output":{"stdout":"...","exit_code":0}}`)
	var out bytes.Buffer
	if err := hookPostCommit(in, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for non-commit; got %q", out.String())
	}
}

func TestHookPostCommit_ShortNondescriptCommit_Quiet(t *testing.T) {
	payload := map[string]interface{}{
		"tool_input":  map[string]interface{}{"command": "git commit -m 'wip'"},
		"tool_output": map[string]interface{}{"stdout": "wip", "exit_code": 0},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostCommit(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for short nondescript commit; got %q", out.String())
	}
}

func TestHookPostCommit_SubstantiveCommit_EmitsAdditionalContext(t *testing.T) {
	body := `fix: long enough commit message that exceeds sixty characters of substance`
	payload := map[string]interface{}{
		"tool_input":  map[string]interface{}{"command": "git commit -m '" + body + "'"},
		"tool_output": map[string]interface{}{"stdout": body, "exit_code": 0},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostCommit(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\ngot: %s", err, out.String())
	}
	if !strings.Contains(resp.HookSpecificOutput.AdditionalContext, "/knomit-remember") {
		t.Errorf("additionalContext missing /knomit-remember reference: %q", resp.HookSpecificOutput.AdditionalContext)
	}
}

func TestHookPostCommit_CommitWithMarker_EmitsEvenIfShort(t *testing.T) {
	// "fix:" marker triggers even if stdout is under 60 chars
	payload := map[string]interface{}{
		"tool_input":  map[string]interface{}{"command": "git commit -m 'fix: typo'"},
		"tool_output": map[string]interface{}{"stdout": "fix: typo", "exit_code": 0},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostCommit(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Error("expected output for commit with fix: marker even if short")
	}
}

// ---- hookSessionStart ----

func TestHookSessionStart_MalformedStdin_Clean(t *testing.T) {
	in := strings.NewReader(`not json at all`)
	var out bytes.Buffer
	if err := hookSessionStart(in, &out); err != nil {
		t.Fatal(err)
	}
	// Should exit cleanly with no output (knomit not running)
}

func TestHookSessionStart_EmptyInput_Clean(t *testing.T) {
	in := strings.NewReader(`{}`)
	var out bytes.Buffer
	if err := hookSessionStart(in, &out); err != nil {
		t.Fatal(err)
	}
	// No error — knomit may not be running; agentBranch returns ""
}

// ---- hookPreCompact ----

func TestHookPreCompact_MalformedStdin_Clean(t *testing.T) {
	in := strings.NewReader(`not json`)
	var out bytes.Buffer
	if err := hookPreCompact(in, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output; got %q", out.String())
	}
}

func TestHookPreCompact_MissingTranscript_Clean(t *testing.T) {
	payload := map[string]interface{}{
		"transcript_path": "/nonexistent/path/to/transcript.jsonl",
		"cwd":             "/tmp",
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPreCompact(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for missing transcript; got %q", out.String())
	}
}

// ---- hookStop ----

func TestHookStop_MalformedStdin_Clean(t *testing.T) {
	in := strings.NewReader(`not json`)
	var out bytes.Buffer
	if err := hookStop(in, &out); err != nil {
		t.Fatal(err)
	}
}

func TestHookStop_MissingTranscript_Clean(t *testing.T) {
	// Reset rate-limit counter so rateLimitFire() may return true
	resetRateLimit(t)

	payload := map[string]interface{}{
		"transcript_path": "/nonexistent/path/transcript.jsonl",
		"cwd":             "/tmp",
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookStop(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for missing transcript; got %q", out.String())
	}
}

// ---- helpers ----

// resetRateLimit forces the counter file to stopRateLimit-1 so the next call
// to rateLimitFire() fires (returns true) rather than skipping.
func resetRateLimit(t *testing.T) {
	t.Helper()
	import_os_path := "/tmp/knomit-stop-rate"
	// Write stopRateLimit-1 so next increment == stopRateLimit → fires
	_ = writeFile(import_os_path, []byte("4"), 0o644) // stopRateLimit=5, 4+1=5 → fires
}

// ---- runHook dispatch ----

func TestRunHook_UnknownEvent_Errors(t *testing.T) {
	err := runHook([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown event")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q should mention the unknown event name", err)
	}
}

func TestRunHook_NoArgs_Errors(t *testing.T) {
	err := runHook([]string{})
	if err == nil {
		t.Fatal("expected error for no args")
	}
}
