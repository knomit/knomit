package mcp

import (
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestUnmarshalArg_MissingKey(t *testing.T) {
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}
	var target []string
	err := unmarshalArg(req, "items", &target)
	if err == nil {
		t.Fatal("expected error for missing key")
	}
	if err.Error() != "items is required" {
		t.Errorf("got %q, want %q", err.Error(), "items is required")
	}
}

func TestUnmarshalArg_InvalidFormat(t *testing.T) {
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"items": "not-an-array",
	}
	var target []struct{ Name string }
	err := unmarshalArg(req, "items", &target)
	if err == nil {
		t.Fatal("expected error for invalid format")
	}
	if len(err.Error()) == 0 {
		t.Error("expected non-empty error message")
	}
}

func TestUnmarshalArg_ValidInput(t *testing.T) {
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{"name": "foo"},
			map[string]interface{}{"name": "bar"},
		},
	}
	var target []struct {
		Name string `json:"name"`
	}
	err := unmarshalArg(req, "items", &target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(target) != 2 || target[0].Name != "foo" || target[1].Name != "bar" {
		t.Errorf("unexpected result: %+v", target)
	}
}
