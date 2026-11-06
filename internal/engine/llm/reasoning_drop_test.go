package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func reasoningBlock(text string) Block {
	raw, _ := json.Marshal(map[string]string{"thinking": text})
	return Block{Kind: BlockReasoning, Reasoning: &Reasoning{Provider: "ollama", Model: "m", Native: raw}}
}

func countReasoning(s Snapshot, hook Hook) int {
	n := 0
	for _, it := range s.Hooks[hook] {
		if it.Entry == nil {
			continue
		}
		for _, b := range it.Entry.Blocks {
			if b.Kind == BlockReasoning {
				n++
			}
		}
	}
	return n
}

// An adaptive request carries no reasoning of ended turns (hook 6) and keeps
// the running turn's (hook 7). Tool_use blocks, results and ids are untouched,
// so every tool_use stays paired; an entry that is only reasoning is left as
// it is rather than emptied; the Builder's own history is unedited.
//
// SABOTAGE: projection.go DropEndedReasoning, return snap unchanged -> fails.
func TestDropEndedReasoning_EndedTurnsLoseReasoningRunningTurnKeepsIt(t *testing.T) {
	w, b := stubWin(t)
	only := Entry{ID: "only", Role: RoleAssistant, Blocks: []Block{reasoningBlock("only thoughts")}}
	addHist(t, b, HookConversation,
		Entry{ID: "u1", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "hi"}}},
		Entry{ID: "a1", Role: RoleAssistant, Blocks: []Block{
			reasoningBlock("thinking one"),
			{Kind: BlockText, Text: "let me look"},
			{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "t1", Name: stubReadTool, Args: json.RawMessage(`{}`)}},
		}},
		Entry{ID: "r1", Role: RoleUser, Blocks: []Block{{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "t1", Content: "ok"}}}},
		Entry{ID: "a2", Role: RoleAssistant, Blocks: []Block{reasoningBlock("thinking two"), {Kind: BlockText, Text: "done"}}},
		only,
	)
	addHist(t, b, HookTurn,
		Entry{ID: "u2", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "again"}}},
		Entry{ID: "a3", Role: RoleAssistant, Blocks: []Block{reasoningBlock("running thoughts"), {Kind: BlockToolUse, ToolUse: &ToolUse{ID: "t2", Name: stubReadTool, Args: json.RawMessage(`{}`)}}}},
	)
	stored := b.Snapshot()
	require.Equal(t, 3, countReasoning(stored, HookConversation))

	sent := w.DropEndedReasoning(stored)
	require.Equal(t, 1, countReasoning(sent, HookConversation), "only the reasoning-only entry keeps its block")
	require.Equal(t, 1, countReasoning(sent, HookTurn), "the running turn keeps its reasoning")
	require.Equal(t, 3, countReasoning(stored, HookConversation), "the stored view is not edited")

	// The first assistant entry keeps its text and its tool_use, in order.
	a1 := sent.Hooks[HookConversation][1].Entry
	require.Equal(t, []BlockKind{BlockText, BlockToolUse}, []BlockKind{a1.Blocks[0].Kind, a1.Blocks[1].Kind})
	require.Len(t, a1.Blocks, 2)
	require.Equal(t, "t1", a1.Blocks[1].ToolUse.ID)
	require.Equal(t, "t1", sent.Hooks[HookConversation][2].Entry.Blocks[0].ToolResult.ToolUseID)
	require.Len(t, sent.Hooks[HookConversation][4].Entry.Blocks, 1, "the reasoning-only entry is whole")
	require.Equal(t, len(stored.Hooks[HookConversation]), len(sent.Hooks[HookConversation]), "no entry is removed")
}

// On the wire: the Ollama request of the dropped view has no `thinking` for an
// ended turn's assistant message, the stored view's has it, the tool calls and
// their results are in both with the same ids, and the sent bytes are smaller
// by at least the reasoning's. Ollama's reasoning is plain text with no
// signature (Reasoning.Native is {"thinking": text}), so nothing is bound to
// the dropped text.
//
// SABOTAGE: projection.go withoutReasoning, return it -> fails.
func TestDropEndedReasoning_OllamaWireHasNoEndedThinkingAndIsSmaller(t *testing.T) {
	w, b := stubWin(t)
	long := "a long chain of thought, " + strings.Repeat("x", 80)
	addHist(t, b, HookConversation,
		Entry{ID: "u1", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "hi"}}},
		Entry{ID: "a1", Role: RoleAssistant, Blocks: []Block{
			reasoningBlock(long),
			{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "t1", Name: stubReadTool, Args: json.RawMessage(`{}`)}},
		}},
		Entry{ID: "r1", Role: RoleUser, Blocks: []Block{{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "t1", Content: "ok"}}}},
	)
	addHist(t, b, HookTurn, Entry{ID: "u2", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "again"}}})
	stored := b.Snapshot()
	sent := w.DropEndedReasoning(stored)

	full, err := json.Marshal(renderOllama(stored, "m", OllamaOptions{}))
	require.NoError(t, err)
	cut, err := json.Marshal(renderOllama(sent, "m", OllamaOptions{}))
	require.NoError(t, err)
	require.Contains(t, string(full), `"thinking"`)
	require.NotContains(t, string(cut), `"thinking"`)
	require.GreaterOrEqual(t, len(full)-len(cut), len(long), "the sent bytes shrink by the reasoning text")
	require.Contains(t, string(cut), `"tool_call_id":"t1"`)
	require.Contains(t, string(cut), `"id":"t1"`)
}

// A loop without a Window (every Anthropic chat) never drops reasoning: its
// signed thinking blocks must be replayed as stored.
//
// SABOTAGE: agent.go view(), call DropEndedReasoning for a nil Window too
// (build a throwaway ToolWindow) -> fails.
func TestLoop_StaticRequestKeepsEndedReasoning(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: &Response{Blocks: []Block{reasoningBlock("kept"), {Kind: BlockText, Text: "one"}}, StopReason: StopEndTurn}},
		{resp: textResp("two", Usage{})},
	}}
	l, _ := newLoop(t, m, okHost("x"))
	_, err := l.RunTurn(context.Background(), "first")
	require.NoError(t, err)
	_, err = l.RunTurn(context.Background(), "second")
	require.NoError(t, err)
	require.Equal(t, 1, countReasoning(m.snaps[1], HookConversation), "the ended turn's thinking is sent whole")
}

// Through the loop: the second turn's request has no reasoning from the first
// turn but the stored history and the log's turn records keep it whole, and
// within the running turn the model's own earlier reasoning is still sent.
//
// SABOTAGE: agent.go view(), skip DropEndedReasoning -> fails.
func TestLoop_AdaptiveRequestDropsEndedReasoningButStoresIt(t *testing.T) {
	think := func(text string, resp *Response) *Response {
		resp.Blocks = append([]Block{reasoningBlock(text)}, resp.Blocks...)
		return resp
	}
	m := &fakeModel{steps: []fakeStep{
		{resp: think("t1-first", toolResp("c1", "knomit_repos", `{}`, Usage{}))},
		{resp: think("t1-second", textResp("one", Usage{}))},
		{resp: think("t2-first", toolResp("c2", "knomit_repos", `{}`, Usage{}))},
		{resp: textResp("two", Usage{})},
	}}
	r := newRig(t, m, winTool("knomit_repos", 10))
	r.loadAhead(t, "knomit_repos")
	_, err := r.l.RunTurn(context.Background(), "first")
	require.NoError(t, err)
	_, err = r.l.RunTurn(context.Background(), "second")
	require.NoError(t, err)

	require.Len(t, m.snaps, 4)
	// Turn 1's own second request still carries its first reasoning (running turn).
	require.Equal(t, 1, countReasoning(m.snaps[1], HookTurn))
	// Turn 2's requests carry none from turn 1 (it is hook 6 now) ...
	require.Equal(t, 0, countReasoning(m.snaps[2], HookConversation))
	// ... and turn 2's second request keeps its own running reasoning.
	require.Equal(t, 0, countReasoning(m.snaps[3], HookConversation))
	require.Equal(t, 1, countReasoning(m.snaps[3], HookTurn))
	// The stored history is whole: three reasoning blocks across the two turns.
	require.Equal(t, 3, countReasoning(r.b.Snapshot(), HookConversation))
}
