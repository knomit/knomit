package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
