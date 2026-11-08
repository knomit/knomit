package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateSubset checks v against a JSON Schema using only the keywords
// knomit_update's ops schema uses. It stands in for a schema-validating MCP
// client: the module has no JSON Schema validator dependency. Any keyword it
// does not implement fails the check, so a schema change that adds one cannot
// pass this test by being silently ignored.
func validateSubset(schema map[string]any, v any) error {
	for k := range schema {
		switch k {
		case "type", "description", "const", "enum", "required", "properties", "additionalProperties", "items", "anyOf":
		default:
			return fmt.Errorf("validateSubset does not implement keyword %q", k)
		}
	}
	if branches, ok := schema["anyOf"].([]any); ok {
		var errs []error
		for _, b := range branches {
			err := validateSubset(b.(map[string]any), v)
			if err == nil {
				return nil
			}
			errs = append(errs, err)
		}
		return fmt.Errorf("matches no anyOf branch: %v", errs)
	}
	if c, ok := schema["const"]; ok && v != c {
		return fmt.Errorf("want const %v, got %v", c, v)
	}
	if e, ok := schema["enum"].([]any); ok {
		found := false
		for _, x := range e {
			found = found || x == v
		}
		if !found {
			return fmt.Errorf("%v not in enum %v", v, e)
		}
	}
	switch schema["type"] {
	case nil:
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("want string, got %T", v)
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("want array, got %T", v)
		}
		if it, ok := schema["items"].(map[string]any); ok {
			for i, x := range arr {
				if err := validateSubset(it, x); err != nil {
					return fmt.Errorf("[%d]: %w", i, err)
				}
			}
		}
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("want object, got %T", v)
		}
		props, _ := schema["properties"].(map[string]any)
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if _, ok := obj[r.(string)]; !ok {
					return fmt.Errorf("missing required %q", r)
				}
			}
		}
		for k, x := range obj {
			p, declared := props[k]
			if !declared {
				if schema["additionalProperties"] == false {
					return fmt.Errorf("undeclared property %q", k)
				}
				continue
			}
			if err := validateSubset(p.(map[string]any), x); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	default:
		return fmt.Errorf("validateSubset does not implement type %v", schema["type"])
	}
	return nil
}

// servedOpsSchemas is knomit_update's ops property as each consumer receives
// it, decoded generically like a client would: the tools/list wire form an
// external MCP client validates against, and the canonical schema the chat
// sends the model (toolDef, hook 1 of the cached prefix).
func servedOpsSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()
	opsOf := func(schema []byte) map[string]any {
		var s struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		require.NoError(t, json.Unmarshal(schema, &s))
		ops, ok := s.Properties["ops"]
		require.True(t, ok)
		return ops
	}
	raw, err := json.Marshal(updateTool())
	require.NoError(t, err)
	var wire struct {
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	require.NoError(t, json.Unmarshal(raw, &wire))
	def, err := toolDef(updateTool())
	require.NoError(t, err)
	return map[string]map[string]any{
		"tools-list": opsOf(wire.InputSchema),
		"chat":       opsOf(def.Schema),
	}
}

// A schema-validating client must still accept every valid ops shape, and the
// served schema must now refuse the mixed shapes the server refuses: the live
// failure was {"op":"str_replace","text":...}, which the old flat schema let
// through.
func TestUpdateTool_OpsSchemaValidatesOpShapes(t *testing.T) {
	cases := []struct {
		name  string
		ops   string
		valid bool
	}{
		{"str_replace", `[{"op":"str_replace","old_str":"a","new_str":"b"}]`, true},
		{"str_replace deleting", `[{"op":"str_replace","old_str":"a","new_str":""}]`, true},
		{"append", `[{"op":"append","text":"t"}]`, true},
		{"both ops in order", `[{"op":"str_replace","old_str":"a","new_str":"b"},{"op":"append","text":"t"}]`, true},
		{"str_replace with text only", `[{"op":"str_replace","text":"t"}]`, false},
		{"str_replace with text and anchors", `[{"op":"str_replace","old_str":"a","new_str":"b","text":"t"}]`, false},
		{"str_replace without new_str", `[{"op":"str_replace","old_str":"a"}]`, false},
		{"append with old_str", `[{"op":"append","text":"t","old_str":"a"}]`, false},
		{"append without text", `[{"op":"append"}]`, false},
		{"unknown op", `[{"op":"replace_all","old_str":"a","new_str":"b"}]`, false},
		{"unknown key", `[{"op":"append","text":"t","at":3}]`, false},
		{"op missing", `[{"text":"t"}]`, false},
	}
	for consumer, schema := range servedOpsSchemas(t) {
		for _, c := range cases {
			t.Run(consumer+"/"+c.name, func(t *testing.T) {
				var v any
				require.NoError(t, json.Unmarshal([]byte(c.ops), &v))
				err := validateSubset(schema, v)
				if c.valid {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					require.NotContains(t, err.Error(), "does not implement", "the validator must understand the schema it refuses with")
				}
			})
		}
	}
}
