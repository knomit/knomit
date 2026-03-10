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

	// Set up a nested path: know/area/sub with a local fact file.
	store.dirEntries["know/area/sub"] = []DirEntry{
		{Name: "local.md", IsDir: false},
	}
	store.files["know/area/sub/local.md"] = SerializeFact(Fact{
		Path:       "know/area/sub/local.md",
		Title:      "Local Fact",
		Body:       "Local fact, not inherited.",
		Domain:     []string{},
		Confidence: 0.9,
		Sources:    1,
		Entities:   []string{},
		Refs:       []string{},
	})

	// Parent directory "know/area" has a regular fact file (1 level up - inherited).
	store.dirEntries["know/area"] = []DirEntry{
		{Name: "parent.md", IsDir: false},
	}
	store.files["know/area/parent.md"] = SerializeFact(Fact{
		Path:       "know/area/parent.md",
		Title:      "Parent Fact",
		Body:       "Inherited from parent.",
		Domain:     []string{},
		Confidence: 0.9,
		Sources:    1,
		Entities:   []string{},
		Refs:       []string{},
	})

	// Grandparent directory "know" has a regular fact file (2 levels up - also inherited).
	store.dirEntries["know"] = []DirEntry{
		{Name: "grandparent.md", IsDir: false},
	}
	store.files["know/grandparent.md"] = SerializeFact(Fact{
		Path:       "know/grandparent.md",
		Title:      "Grandparent Fact",
		Body:       "Inherited from grandparent.",
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

	// local.md should appear as a regular child entry, not as an inherited fact.
	children, ok := resp["children"].([]interface{})
	if !ok {
		t.Fatalf("children not array: %v", resp["children"])
	}
	if len(children) != 1 {
		t.Fatalf("expected 1 child, got %d: %v", len(children), children)
	}
	child, ok := children[0].(map[string]interface{})
	if !ok {
		t.Fatalf("child not an object: %v", children[0])
	}
	if child["name"] != "local" {
		t.Fatalf("expected child name 'local', got %q", child["name"])
	}

	// Both parent.md (1 level up) and grandparent.md (2 levels up) must appear as inherited facts.
	inherited, ok := resp["inherited_facts"].([]interface{})
	if !ok {
		t.Fatalf("inherited_facts not array: %v", resp["inherited_facts"])
	}
	if len(inherited) != 2 {
		t.Fatalf("expected 2 inherited facts, got %d: %v", len(inherited), inherited)
	}

	// Build a map of file -> title for easier assertion.
	inheritedByFile := map[string]string{}
	for _, item := range inherited {
		f, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("inherited fact not an object: %v", item)
		}
		file, _ := f["file"].(string)
		title, _ := f["title"].(string)
		inheritedByFile[file] = title
	}

	if inheritedByFile["know/area/parent.md"] != "Parent Fact" {
		t.Fatalf("expected inherited 'know/area/parent.md' with title 'Parent Fact', got: %v", inheritedByFile)
	}
	if inheritedByFile["know/grandparent.md"] != "Grandparent Fact" {
		t.Fatalf("expected inherited 'know/grandparent.md' with title 'Grandparent Fact', got: %v", inheritedByFile)
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
