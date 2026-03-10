package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestExploreListsEntries(t *testing.T) {
	store := newMockStore()
	// Set up directory entries for "know".
	store.dirEntries["know"] = []DirEntry{
		{Name: "sub", IsDir: true},
		{Name: "foo.md", IsDir: false},
	}
	// Set up readable foo.md.
	store.files["know/foo.md"] = SerializeFact(Fact{
		Path: "know/foo.md", Title: "Foo Fact", Body: "Foo body.",
		Domain: []string{}, Confidence: 0.9, Sources: 1, Entities: []string{}, Refs: []string{},
	})

	handler := ExploreHandler(store)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"path": "know",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}

	children, ok := resp["children"].([]interface{})
	if !ok {
		t.Fatalf("children not array: %v", resp["children"])
	}
	if len(children) != 2 {
		t.Fatalf("expected 2 children, got %d: %v", len(children), children)
	}
}

func TestExploreDefaultPath(t *testing.T) {
	store := newMockStore()
	store.dirEntries["know"] = []DirEntry{}

	handler := ExploreHandler(store)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := resp["children"]; !ok {
		t.Fatal("missing children in response")
	}
}

func TestExploreInheritedFacts(t *testing.T) {
	store := newMockStore()

	// Set up a nested path: know/area/sub
	store.dirEntries["know/area/sub"] = []DirEntry{}

	// Parent directory "know/area" has a regular fact file (not a subdirectory).
	store.dirEntries["know/area"] = []DirEntry{
		{Name: "parent-fact.md", IsDir: false},
	}
	store.files["know/area/parent-fact.md"] = SerializeFact(Fact{
		Path:       "know/area/parent-fact.md",
		Title:      "Parent Fact",
		Body:       "Inherited from parent.",
		Domain:     []string{},
		Confidence: 0.9,
		Sources:    1,
		Entities:   []string{},
		Refs:       []string{},
	})

	handler := ExploreHandler(store)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"path": "know/area/sub",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}

	inherited, ok := resp["inherited_facts"].([]interface{})
	if !ok {
		t.Fatalf("inherited_facts not array: %v", resp["inherited_facts"])
	}
	if len(inherited) != 1 {
		t.Fatalf("expected 1 inherited fact, got %d: %v", len(inherited), inherited)
	}
	fact, ok := inherited[0].(map[string]interface{})
	if !ok {
		t.Fatalf("inherited fact not an object: %v", inherited[0])
	}
	if fact["title"] != "Parent Fact" {
		t.Fatalf("expected inherited title 'Parent Fact', got %q", fact["title"])
	}
	if fact["file"] != "know/area/parent-fact.md" {
		t.Fatalf("expected inherited file 'know/area/parent-fact.md', got %q", fact["file"])
	}
}

func TestExploreWithManifest(t *testing.T) {
	store := newMockStore()
	store.dirEntries["know/sub"] = []DirEntry{}
	// Set up manifest at know/sub.md.
	store.files["know/sub.md"] = SerializeFact(Fact{
		Path: "know/sub.md", Title: "Sub Manifest", Body: "This is the sub section.",
		Domain: []string{}, Confidence: 1.0, Sources: 1, Entities: []string{}, Refs: []string{},
	})

	handler := ExploreHandler(store)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"path": "know/sub",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	manifest, ok := resp["manifest"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected manifest, got: %v", resp["manifest"])
	}
	if manifest["title"] != "Sub Manifest" {
		t.Fatalf("manifest title: got %q", manifest["title"])
	}
}
