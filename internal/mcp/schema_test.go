package mcp

import (
	"fmt"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// allTools returns every tool definition registered by the server.
func allTools() []mcpgo.Tool {
	return []mcpgo.Tool{
		learnTool(),
		queryTool(),
		updateTool(),
		explainTool(),
		exploreTool("kb"),
		retractTool(),
		reviewTool(),
	}
}

// --- Generic recursive schema validator ---

// schemaErrors recursively walks a JSON-schema-like map and returns errors for
// every array missing "items" and every object missing "properties".
func schemaErrors(path string, schema map[string]any) []string {
	var errs []string

	typ, _ := schema["type"].(string)

	if typ == "array" {
		if _, ok := schema["items"]; !ok {
			errs = append(errs, fmt.Sprintf("%s: array has no items schema", path))
		} else if items, ok := schema["items"].(map[string]any); ok {
			errs = append(errs, schemaErrors(path+".items", items)...)
		}
	}

	if typ == "object" {
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: object has no properties schema", path))
		} else {
			for name, v := range props {
				if sub, ok := v.(map[string]any); ok {
					errs = append(errs, schemaErrors(path+"."+name, sub)...)
				}
			}
		}
	}

	// If this is not explicitly typed but has properties, recurse into them.
	if typ == "" {
		if props, ok := schema["properties"].(map[string]any); ok {
			for name, v := range props {
				if sub, ok := v.(map[string]any); ok {
					errs = append(errs, schemaErrors(path+"."+name, sub)...)
				}
			}
		}
	}

	return errs
}

// validateToolSchema checks that a tool has basic metadata and that its
// inputSchema is recursively well-formed (no bare arrays/objects).
func validateToolSchema(t *testing.T, tool mcpgo.Tool) {
	t.Helper()

	if tool.Name == "" {
		t.Error("tool has empty name")
	}
	if tool.Description == "" {
		t.Errorf("tool %s has empty description", tool.Name)
	}

	// Walk top-level properties.
	for name, v := range tool.InputSchema.Properties {
		prop, ok := v.(map[string]any)
		if !ok {
			t.Errorf("tool %s: property %s is not a map", tool.Name, name)
			continue
		}
		for _, e := range schemaErrors(tool.Name+"."+name, prop) {
			t.Error(e)
		}
	}
}

// --- Test: every tool schema is well-formed ---

func TestSchemaAllToolsWellFormed(t *testing.T) {
	for _, tool := range allTools() {
		t.Run(tool.Name, func(t *testing.T) {
			validateToolSchema(t, tool)
		})
	}
}

// --- Test: knomit_learn specifics ---

func TestSchemaLearnFacts(t *testing.T) {
	tool := learnTool()

	// facts property must exist and be an array with items.
	factsProp, ok := tool.InputSchema.Properties["facts"].(map[string]any)
	if !ok {
		t.Fatal("facts property missing or not a map")
	}
	if factsProp["type"] != "array" {
		t.Fatalf("facts type = %v, want array", factsProp["type"])
	}

	items, ok := factsProp["items"].(map[string]any)
	if !ok {
		t.Fatal("facts.items missing or not a map")
	}
	if items["type"] != "object" {
		t.Fatalf("facts.items.type = %v, want object", items["type"])
	}

	// Check required fields on items.
	required, ok := items["required"].([]string)
	if !ok {
		t.Fatal("facts.items.required missing or wrong type")
	}
	wantRequired := map[string]bool{
		"topic":    true,
		"category": true,
		"title":    true,
		"body":     true,
	}
	for _, r := range required {
		delete(wantRequired, r)
	}
	for missing := range wantRequired {
		t.Errorf("facts.items.required missing %q", missing)
	}

	// Check that all expected properties exist.
	props, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatal("facts.items.properties missing or not a map")
	}
	wantProps := []string{
		"topic", "category", "title", "body", "type",
		"domain", "confidence", "sources", "entities", "refs",
	}
	for _, p := range wantProps {
		if _, exists := props[p]; !exists {
			t.Errorf("facts.items.properties missing %q", p)
		}
	}

	// Verify nested arrays inside items also have item schemas.
	for _, arrayProp := range []string{"domain", "entities", "refs"} {
		sub, ok := props[arrayProp].(map[string]any)
		if !ok {
			t.Errorf("facts.items.properties.%s not a map", arrayProp)
			continue
		}
		if sub["type"] != "array" {
			t.Errorf("facts.items.properties.%s type = %v, want array", arrayProp, sub["type"])
			continue
		}
		if _, ok := sub["items"]; !ok {
			t.Errorf("facts.items.properties.%s: array has no items schema", arrayProp)
		}
	}
}

// --- Test: knomit_query specifics ---

func TestSchemaQueryArrays(t *testing.T) {
	tool := queryTool()

	for _, name := range []string{"entities", "domain"} {
		t.Run(name, func(t *testing.T) {
			prop, ok := tool.InputSchema.Properties[name].(map[string]any)
			if !ok {
				t.Fatalf("%s property missing or not a map", name)
			}
			if prop["type"] != "array" {
				t.Fatalf("%s type = %v, want array", name, prop["type"])
			}
			items, ok := prop["items"].(map[string]any)
			if !ok {
				t.Fatalf("%s.items missing or not a map", name)
			}
			if items["type"] != "string" {
				t.Errorf("%s.items.type = %v, want string", name, items["type"])
			}
		})
	}
}

// --- Test: knomit_update specifics ---

func TestSchemaUpdateUpdates(t *testing.T) {
	tool := updateTool()

	updatesProp, ok := tool.InputSchema.Properties["updates"].(map[string]any)
	if !ok {
		t.Fatal("updates property missing or not a map")
	}
	if updatesProp["type"] != "object" {
		t.Fatalf("updates type = %v, want object", updatesProp["type"])
	}

	props, ok := updatesProp["properties"].(map[string]any)
	if !ok {
		t.Fatal("updates.properties missing or not a map")
	}

	wantProps := []string{
		"title", "body", "type", "confidence", "sources", "domain", "entities", "refs",
	}
	for _, p := range wantProps {
		if _, exists := props[p]; !exists {
			t.Errorf("updates.properties missing %q", p)
		}
	}

	// Verify nested arrays inside updates also have item schemas.
	for _, arrayProp := range []string{"domain", "entities", "refs"} {
		sub, ok := props[arrayProp].(map[string]any)
		if !ok {
			t.Errorf("updates.properties.%s not a map", arrayProp)
			continue
		}
		if sub["type"] != "array" {
			t.Errorf("updates.properties.%s type = %v, want array", arrayProp, sub["type"])
			continue
		}
		if _, ok := sub["items"]; !ok {
			t.Errorf("updates.properties.%s: array has no items schema", arrayProp)
		}
	}
}

// --- Test: required fields are listed for tools that have them ---

func TestSchemaRequiredFields(t *testing.T) {
	cases := []struct {
		name     string
		tool     mcpgo.Tool
		required []string
	}{
		{"knomit_learn", learnTool(), []string{"moment_name", "facts"}},
		{"knomit_update", updateTool(), []string{"file", "moment_name", "updates"}},
		{"knomit_explain", explainTool(), []string{"file"}},
		{"knomit_retract", retractTool(), []string{"file", "moment_name"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := make(map[string]bool, len(tc.tool.InputSchema.Required))
			for _, r := range tc.tool.InputSchema.Required {
				got[r] = true
			}
			for _, want := range tc.required {
				if !got[want] {
					t.Errorf("required field %q not listed in schema", want)
				}
			}
		})
	}
}
