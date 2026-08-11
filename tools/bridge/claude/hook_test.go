package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- hookSessionStart ----

func TestHookSessionStart_MalformedStdin_Clean(t *testing.T) {
	// Malformed JSON returns before any HTTP call, so this case is
	// independent of whether knomit is reachable.
	in := strings.NewReader(`not json at all`)
	var out bytes.Buffer
	if err := hookSessionStart(in, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for malformed stdin; got %q", out.String())
	}
}

func TestHookSessionStart_EmptyInput_Clean(t *testing.T) {
	// Empty `{}` decodes fine and then hits HTTP; point at a closed server
	// so agentBranch fails deterministically.
	closedKnomit(t)
	in := strings.NewReader(`{}`)
	var out bytes.Buffer
	if err := hookSessionStart(in, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output when agent_branch unreachable; got %q", out.String())
	}
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
	ctx := out.String()
	if !strings.Contains(ctx, "fix-bug") {
		t.Errorf("output missing 'fix-bug' label: %q", ctx)
	}
	if !strings.Contains(ctx, "root cause") {
		t.Errorf("output missing quoted sentence: %q", ctx)
	}
	if !strings.Contains(ctx, "/knomit-remember") {
		t.Errorf("output missing /knomit-remember nudge: %q", ctx)
	}
}

// PreCompact has no hookSpecificOutput variant in CC's output schema: a JSON
// envelope fails validation and CC discards the hook's whole result, so the
// nudge never reaches the compaction. CC consumes plain stdout here as the
// custom compact instructions. Assert the payload is bare text, not JSON —
// asserting only on our own encoder passes green while CC still drops it.
func TestHookPreCompact_EmitsPlainTextNotJSON(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
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
	got := out.String()
	if got == "" {
		t.Fatal("expected capture-candidate output, got nothing")
	}
	if strings.Contains(got, "hookSpecificOutput") {
		t.Errorf("PreCompact must not emit a hookSpecificOutput envelope; got: %s", got)
	}
	var envelope map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &envelope); err == nil {
		t.Errorf("PreCompact output must be plain text, not JSON; got: %s", got)
	}
	if !strings.Contains(got, "root cause") {
		t.Errorf("output missing quoted sentence: %q", got)
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
	occurrences := strings.Count(out.String(), "Be careful")
	if occurrences != 1 {
		t.Errorf("expected exactly 1 occurrence of the quoted sentence, got %d:\n%s",
			occurrences, out.String())
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
