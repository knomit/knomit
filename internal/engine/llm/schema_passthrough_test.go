package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// anyOfOpsSchema has the shape knomit_update's ops argument is served with: an
// array whose items are an anyOf of closed, const-discriminated branches.
const anyOfOpsSchema = `{"properties":{"ops":{"items":{"anyOf":[` +
	`{"additionalProperties":false,"properties":{"new_str":{"type":"string"},"old_str":{"type":"string"},"op":{"const":"str_replace","type":"string"}},"required":["op","old_str","new_str"],"type":"object"},` +
	`{"additionalProperties":false,"properties":{"op":{"const":"append","type":"string"},"text":{"type":"string"}},"required":["op","text"],"type":"object"}` +
	`]},"type":"array"}},"type":"object"}`

func anyOfSnapshot(t *testing.T) Snapshot {
	t.Helper()
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "tool:knomit_update", Hook: HookTools, Tool: &ToolDef{
		Name: "knomit_update", Description: "Update.", Schema: json.RawMessage(anyOfOpsSchema),
	}}))
	require.NoError(t, b.Add(Item{ID: "turn", Hook: HookTurn, Entry: &Entry{ID: "turn", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "go"}}}}))
	return b.Snapshot()
}

// Neither provider adapter walks, flattens or re-types a tool schema: a nested
// anyOf reaches the wire exactly as the tool declared it. Both adapters pick
// Schema as raw bytes (anthropic.go param.Override, ollama.go
// json.RawMessage); this pins that, so a future typed schema struct that would
// drop anyOf fails here.
func TestProviders_PassAnyOfItemsSchemaThroughUnchanged(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		raw, err := json.Marshal(renderAnthropic(anyOfSnapshot(t), "claude-test-x", AnthropicOptions{}))
		require.NoError(t, err)
		var req struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"input_schema"`
			} `json:"tools"`
		}
		require.NoError(t, json.Unmarshal(raw, &req))
		require.Len(t, req.Tools, 1)
		require.Equal(t, "knomit_update", req.Tools[0].Name)
		require.JSONEq(t, anyOfOpsSchema, string(req.Tools[0].InputSchema))
	})
	t.Run("ollama", func(t *testing.T) {
		raw, err := json.Marshal(renderOllama(anyOfSnapshot(t), "m", OllamaOptions{}))
		require.NoError(t, err)
		var req struct {
			Tools []struct {
				Function struct {
					Name       string          `json:"name"`
					Parameters json.RawMessage `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		require.NoError(t, json.Unmarshal(raw, &req))
		require.Len(t, req.Tools, 1)
		require.Equal(t, "knomit_update", req.Tools[0].Function.Name)
		require.JSONEq(t, anyOfOpsSchema, string(req.Tools[0].Function.Parameters))
	})
}
