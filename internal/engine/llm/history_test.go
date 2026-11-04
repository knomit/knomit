package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEntry_ReplayForDropsForeignReasoning(t *testing.T) {
	e := Entry{ID: "e1", Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockReasoning, Reasoning: &Reasoning{Provider: "anthropic", Model: "claude-a", Native: json.RawMessage(`{"type":"thinking"}`)}},
		{Kind: BlockText, Text: "answer"},
	}}

	other := e.ReplayFor("anthropic", "claude-b")
	require.Len(t, other.Blocks, 1)
	require.Equal(t, BlockText, other.Blocks[0].Kind)
	require.Equal(t, "answer", other.Blocks[0].Text)

	same := e.ReplayFor("anthropic", "claude-a")
	require.Len(t, same.Blocks, 2)
	require.Equal(t, BlockReasoning, same.Blocks[0].Kind)
	require.Equal(t, "claude-a", same.Blocks[0].Reasoning.Model)

	// Same model name under another provider is foreign too.
	require.Len(t, e.ReplayFor("bedrock", "claude-a").Blocks, 1)
	// The receiver is untouched.
	require.Len(t, e.Blocks, 2)
}

func TestEntry_JSONRoundTripIsProviderNeutral(t *testing.T) {
	e := Entry{ID: "e2", Role: RoleUser, Blocks: []Block{
		{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "tu_1", Name: "knomit_query", Args: json.RawMessage(`{"text":"x"}`)}},
		{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "tu_1", Content: "r", IsError: true}},
		{Kind: BlockText, Text: "hi"},
	}, Usage: &Usage{Input: 1, CacheRead: 2, CacheWrite: 3, Output: 4}, Approvals: []Approval{{ToolUseID: "tu_1", Decision: "allow"}}}

	raw, err := json.Marshal(e)
	require.NoError(t, err)
	for _, k := range []string{"cache_control", "tool_call_id", "tool_name"} {
		require.NotContains(t, string(raw), `"`+k+`"`)
	}
	require.True(t, strings.Contains(string(raw), `"tool_use_id":"tu_1"`), string(raw))

	var back Entry
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, e, back)
}
