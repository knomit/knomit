package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestHookPreCompact_EmitsQuotedCandidatesOnHit(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"type":"user","message":{"content":"Some prior question."}}`,
		`{"type":"assistant","message":{"content":"The root cause was a missing vtab registration."}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	payload := map[string]interface{}{
		"transcript_path": transcript,
		"cwd":             dir,
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPreCompact(bytes.NewReader(data), &out); err != nil {
		t.Fatalf("hookPreCompact: %v", err)
	}
	var resp struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\ngot: %s", err, out.String())
	}
	ctx := resp.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "fix-bug") {
		t.Errorf("additionalContext missing 'fix-bug' label: %q", ctx)
	}
	if !strings.Contains(ctx, "root cause") {
		t.Errorf("additionalContext missing quoted sentence: %q", ctx)
	}
	if !strings.Contains(ctx, "/knomit-remember") {
		t.Errorf("additionalContext missing /knomit-remember nudge: %q", ctx)
	}
}

func TestHookPreCompact_DedupesSameSentenceIntentHits(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	// "Be careful — this only works if X" matches BOTH gotcha:warn AND gotcha:silent.
	lines := []string{
		`{"type":"user","message":{"content":"how do I use it?"}}`,
		`{"type":"assistant","message":{"content":"Be careful — this only works if the agent branch is checked out."}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	payload := map[string]interface{}{
		"transcript_path": transcript,
		"cwd":             dir,
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPreCompact(bytes.NewReader(data), &out); err != nil {
		t.Fatalf("hookPreCompact: %v", err)
	}
	var resp struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\ngot: %s", err, out.String())
	}
	occurrences := strings.Count(resp.HookSpecificOutput.AdditionalContext, "Be careful")
	if occurrences != 1 {
		t.Errorf("expected exactly 1 occurrence of the quoted sentence, got %d:\n%s",
			occurrences, resp.HookSpecificOutput.AdditionalContext)
	}
}

func TestHookPreCompact_NoEmitWhenNoHits(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"type":"user","message":{"content":"Plain question."}}`,
		`{"type":"assistant","message":{"content":"Plain answer."}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	payload := map[string]interface{}{
		"transcript_path": transcript,
		"cwd":             dir,
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPreCompact(bytes.NewReader(data), &out); err != nil {
		t.Fatalf("hookPreCompact: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output when no intent matches; got %q", out.String())
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
	// Bypass rate limiter: write counter value that triggers a fire next call.
	counterPath := filepath.Join(os.TempDir(), "knomit-stop-rate")
	_ = os.WriteFile(counterPath, []byte("4"), 0o644)
	t.Cleanup(func() { _ = os.Remove(counterPath) })

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

func TestHookStop_EmitsQuotedCandidatesOnHit(t *testing.T) {
	// Bypass rate-limiter so the hook actually fires.
	counterPath := filepath.Join(os.TempDir(), "knomit-stop-rate")
	_ = os.WriteFile(counterPath, []byte("4"), 0o644)
	t.Cleanup(func() { _ = os.Remove(counterPath) })

	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"type":"assistant","message":{"content":"Some prior message."}}`,
		`{"type":"user","message":{"content":"No, that's not what I meant."}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	payload := map[string]interface{}{
		"transcript_path": transcript,
		"cwd":             dir,
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookStop(bytes.NewReader(data), &out); err != nil {
		t.Fatalf("hookStop: %v", err)
	}
	var resp struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\ngot: %s", err, out.String())
	}
	ctx := resp.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "correction") {
		t.Errorf("additionalContext missing 'correction' label: %q", ctx)
	}
	if !strings.Contains(ctx, "No, that's not what I meant") {
		t.Errorf("additionalContext missing quoted sentence: %q", ctx)
	}
	if !strings.Contains(ctx, "/knomit-remember") {
		t.Errorf("additionalContext missing /knomit-remember nudge: %q", ctx)
	}
}

func TestHookStop_DedupesSameSentenceIntentHits(t *testing.T) {
	// Bypass rate-limiter.
	counterPath := filepath.Join(os.TempDir(), "knomit-stop-rate")
	_ = os.WriteFile(counterPath, []byte("4"), 0o644)
	t.Cleanup(func() { _ = os.Remove(counterPath) })

	// "No, that's not what I meant." matches BOTH correction:start AND
	// correction:phrase. The output should contain ONE candidate line, not two.
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"type":"assistant","message":{"content":"Some prior message."}}`,
		`{"type":"user","message":{"content":"No, that's not what I meant."}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	payload := map[string]interface{}{
		"transcript_path": transcript,
		"cwd":             dir,
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookStop(bytes.NewReader(data), &out); err != nil {
		t.Fatalf("hookStop: %v", err)
	}
	var resp struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\ngot: %s", err, out.String())
	}
	occurrences := strings.Count(resp.HookSpecificOutput.AdditionalContext, "No, that's not what I meant.")
	if occurrences != 1 {
		t.Errorf("expected exactly 1 occurrence of the quoted sentence, got %d:\n%s",
			occurrences, resp.HookSpecificOutput.AdditionalContext)
	}
}

func TestHookStop_NoEmitWhenNoHits(t *testing.T) {
	counterPath := filepath.Join(os.TempDir(), "knomit-stop-rate")
	_ = os.WriteFile(counterPath, []byte("4"), 0o644)
	t.Cleanup(func() { _ = os.Remove(counterPath) })

	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"type":"user","message":{"content":"Plain question about the weather."}}`,
		`{"type":"assistant","message":{"content":"It is sunny."}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	payload := map[string]interface{}{
		"transcript_path": transcript,
		"cwd":             dir,
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookStop(bytes.NewReader(data), &out); err != nil {
		t.Fatalf("hookStop: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output when no intent matches; got %q", out.String())
	}
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
