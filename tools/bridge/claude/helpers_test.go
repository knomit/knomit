package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

// ---- emitAdditionalContext ----

func TestEmitAdditionalContext_Empty_NoOutput(t *testing.T) {
	var out bytes.Buffer
	if err := emitAdditionalContext(&out, ""); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for empty ctx; got %q", out.String())
	}
}

func TestEmitAdditionalContext_NonEmpty_ValidJSON(t *testing.T) {
	var out bytes.Buffer
	if err := emitAdditionalContext(&out, "hello world"); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\ngot: %s", err, out.String())
	}
	if resp.HookSpecificOutput.AdditionalContext != "hello world" {
		t.Errorf("additionalContext = %q, want %q", resp.HookSpecificOutput.AdditionalContext, "hello world")
	}
}

// ---- parseTranscript ----

func TestParseTranscript_NonexistentFile_Error(t *testing.T) {
	_, err := parseTranscript("/nonexistent/transcript.jsonl", 10)
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestParseTranscript_ValidJSONL_ReturnsBlocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	lines := []string{
		`{"type":"user","message":{"content":"hello world"}}`,
		`{"type":"assistant","message":{"content":"I understand"}}`,
		`{"type":"system","message":{"content":"some system message"}}`,
		`{"type":"user","message":{"content":[{"type":"text","text":"array content"}]}}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	blocks, err := parseTranscript(path, 100)
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	// Should get 3 blocks: user, assistant, user (system is skipped)
	if len(blocks) != 3 {
		t.Errorf("got %d blocks, want 3", len(blocks))
	}
	if blocks[0].Role != "user" || blocks[0].Text != "hello world" {
		t.Errorf("block[0] = %+v", blocks[0])
	}
	if blocks[1].Role != "assistant" || blocks[1].Text != "I understand" {
		t.Errorf("block[1] = %+v", blocks[1])
	}
	if blocks[2].Role != "user" || blocks[2].Text != "array content" {
		t.Errorf("block[2] = %+v", blocks[2])
	}
}

func TestParseTranscript_LimitTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, `{"type":"user","message":{"content":"msg"}}`)
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	blocks, err := parseTranscript(path, 3)
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if len(blocks) != 3 {
		t.Errorf("got %d blocks, want 3 (limit)", len(blocks))
	}
}

func TestParseTranscript_MalformedLines_Skipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	content := `not json at all
{"type":"user","message":{"content":"good line"}}
{"malformed":
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	blocks, err := parseTranscript(path, 100)
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if len(blocks) != 1 {
		t.Errorf("got %d blocks, want 1 (only the good line)", len(blocks))
	}
}

// ---- extractMessageText ----

func TestExtractMessageText_StringContent(t *testing.T) {
	raw := json.RawMessage(`{"content":"hello"}`)
	got := extractMessageText(raw)
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestExtractMessageText_ArrayContent(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"first"},{"type":"image","text":""},{"type":"text","text":"second"}]}`)
	got := extractMessageText(raw)
	want := "first\nsecond"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractMessageText_InvalidJSON_Empty(t *testing.T) {
	raw := json.RawMessage(`not json`)
	got := extractMessageText(raw)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
