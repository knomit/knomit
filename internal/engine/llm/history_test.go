package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEntry_ReplayForDropsOnlyOtherProvidersReasoning(t *testing.T) {
	e := Entry{ID: "e1", Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockReasoning, Reasoning: &Reasoning{Provider: "anthropic", Model: "claude-a", Native: json.RawMessage(`{"type":"thinking","signature":"S"}`)}},
		{Kind: BlockText, Text: "answer"},
	}}

	// Any Anthropic model gets the block, unchanged, whichever model made it.
	got := e.ReplayFor("anthropic")
	require.Len(t, got.Blocks, 2)
	require.Equal(t, BlockReasoning, got.Blocks[0].Kind)
	require.Equal(t, "claude-a", got.Blocks[0].Reasoning.Model)
	require.JSONEq(t, `{"type":"thinking","signature":"S"}`, string(got.Blocks[0].Reasoning.Native))
	require.Equal(t, "answer", got.Blocks[1].Text)

	// Another provider never sees it, and keeps the rest in order.
	other := e.ReplayFor("ollama")
	require.Len(t, other.Blocks, 1)
	require.Equal(t, BlockText, other.Blocks[0].Kind)
	require.Equal(t, "answer", other.Blocks[0].Text)
	// A reasoning block without provenance is never replayed.
	bare := Entry{Blocks: []Block{{Kind: BlockReasoning}, {Kind: BlockText, Text: "x"}}}
	require.Len(t, bare.ReplayFor("anthropic").Blocks, 1)
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
