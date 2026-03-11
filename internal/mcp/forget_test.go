package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestForgetDeletesFile(t *testing.T) {
	store := newMockStore()
	store.files["know/foo.md"] = SerializeFact(Fact{
		Path: "know/foo.md", Title: "Foo", Body: "Body.",
		Domain: []string{}, Confidence: 0.9, Sources: 1,
		Entities: []string{}, Refs: []string{},
	})

	idx := &mockIndex{}
	handler := ForgetHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/foo.md",
		"moment_name": "forget-test",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	// Verify file was marked for deletion.
	if len(store.deleted) == 0 || store.deleted[0] != "know/foo.md" {
		t.Fatalf("expected know/foo.md to be deleted, got: %v", store.deleted)
	}

	// Verify index delete was called.
	if len(idx.deleted) == 0 || idx.deleted[0] != "know/foo.md" {
		t.Fatalf("expected index delete for know/foo.md, got: %v", idx.deleted)
	}

	// Verify tag was set.
	if len(store.tags) == 0 {
		t.Fatal("expected forget tag to be set")
	}
	if !strings.HasPrefix(store.tags[0], "forget/") {
		t.Fatalf("tag should start with forget/, got %q", store.tags[0])
	}

	// Verify result JSON.
	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["file"] != "know/foo.md" {
		t.Fatalf("response file: got %q", resp["file"])
	}
	if _, ok := resp["commit"]; !ok {
		t.Fatal("missing commit in response")
	}
}

func TestForgetFileNotFound(t *testing.T) {
	store := newMockStore()
	idx := &mockIndex{}
	handler := ForgetHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/nonexistent.md",
		"moment_name": "test",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for missing file")
	}
}
