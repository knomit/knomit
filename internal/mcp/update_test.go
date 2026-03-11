package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestUpdateMergesFields(t *testing.T) {
	store := newMockStore()
	factContent := SerializeFact(Fact{
		Path: "know/foo.md", Title: "Original Title", Body: "Original body.",
		Domain: []string{"testing"}, Confidence: 0.7, Sources: 1,
		Entities: []string{}, Refs: []string{"https://old.ref"},
	})
	store.files["know/foo.md"] = factContent

	idx := &mockIndex{}
	handler := UpdateHandler(store, idx)

	newBody := "Updated body."
	newConf := 0.95

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/foo.md",
		"moment_name": "update-test",
		"updates": map[string]interface{}{
			"body":       newBody,
			"confidence": newConf,
			"refs":       []interface{}{"https://new.ref"},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	// Verify file was updated.
	written, ok := store.written["know/foo.md"]
	if !ok {
		t.Fatal("expected know/foo.md to be written")
	}

	updatedFact, err := ParseFact("know/foo.md", written)
	if err != nil {
		t.Fatalf("parse updated fact: %v", err)
	}
	if updatedFact.Body != newBody {
		t.Fatalf("body: got %q want %q", updatedFact.Body, newBody)
	}
	if updatedFact.Confidence != newConf {
		t.Fatalf("confidence: got %v want %v", updatedFact.Confidence, newConf)
	}
	// Refs should be appended, not replaced.
	if len(updatedFact.Refs) != 2 {
		t.Fatalf("refs: expected 2, got %v", updatedFact.Refs)
	}

	// Verify result JSON.
	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := resp["commit"]; !ok {
		t.Fatal("missing commit in response")
	}
	if _, ok := resp["moment_tag"]; !ok {
		t.Fatal("missing moment_tag in response")
	}
}

func TestUpdateFileNotFound(t *testing.T) {
	store := newMockStore()
	idx := &mockIndex{}
	handler := UpdateHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/nonexistent.md",
		"moment_name": "test",
		"updates":     map[string]interface{}{},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for missing file")
	}
}

func TestUpdateRefsAppended(t *testing.T) {
	store := newMockStore()
	factContent := SerializeFact(Fact{
		Path: "know/refs.md", Title: "Refs Test", Body: "Body.",
		Domain: []string{}, Confidence: 0.8, Sources: 1,
		Entities: []string{}, Refs: []string{"https://existing.ref"},
	})
	store.files["know/refs.md"] = factContent

	idx := &mockIndex{}
	handler := UpdateHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/refs.md",
		"moment_name": "refs-append",
		"updates": map[string]interface{}{
			"refs": []interface{}{"https://new1.ref", "https://new2.ref"},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	written := store.written["know/refs.md"]
	updatedFact, err := ParseFact("know/refs.md", written)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(updatedFact.Refs) != 3 {
		t.Fatalf("expected 3 refs, got %v", updatedFact.Refs)
	}
}
