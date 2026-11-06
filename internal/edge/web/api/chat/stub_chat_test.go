package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

// toolIn is the hook-1 tool called name in a request the model was sent.
func toolIn(s llm.Snapshot, name string) *llm.ToolDef {
	for _, it := range s.Hooks[llm.HookTools] {
		if it.Tool.Name == name {
			return it.Tool
		}
	}
	return nil
}

// The live re-test's S4: a 4B model never called load_tools, so knomit_learn
// was never reachable. Now knomit_learn is in the first request as a stub; the
// model calls it, the call is not run and raises no card, and is answered with
// the approved sentence; the next request has the full definition; the second
// call raises the approval card, and the fact lands only after Allow.
//
// SABOTAGE: mcp/chat_prefix.go NewChatToolWindow, drop `Purposes:`/the stubs
// (llm/toolwindow.go Project, skip the stub case) -> the first request has no
// knomit_learn and the first assertion fails; agent.go runCalls, skip the
// Window.Handles branch -> the stub call raises a card and the "no card before
// the second call" assertion fails.
func TestChat_CallingTheLearnStubLoadsItThenTheCallAsksAndWritesOnlyAfterAllow(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	const title = "Written through a stub call"
	handle := h.bind(writer, "chat-00000000000000000000000000000b01", "alpha")
	m := h.queueAdaptive("o1", 40000,
		toolStep("tu_stub", "knomit_learn", map[string]any{"binding": handle}, usage),
		toolStep("tu_learn", "knomit_learn", learnArgs(handle, title), usage),
		textStep("written", usage),
	)
	id := h.createAdaptive("uid:501")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))

	// The first event the person is asked about is the second call's card.
	var approval approvalEvent
	for {
		e := s.next("", 10*time.Second)
		require.NotEqual(t, "turn_done", e.Name, "the turn ended without asking")
		if e.Name == "approval" {
			require.NoError(t, json.Unmarshal(e.Data, &approval))
			break
		}
	}
	require.Equal(t, "tu_learn", approval.CallID, "the stub call raised no card: the first card is the real call's")
	require.Equal(t, "knomit_learn", approval.Tool)
	require.NotContains(t, h.queryTitles(writer, "chat-00000000000000000000000000000b02", handle), title,
		"nothing was written before the person answered")
	require.Equal(t, http.StatusNoContent, h.approve("uid:501", id, "tu_learn", "allow"))
	s.until("turn_done")
	require.Contains(t, h.queryTitles(writer, "chat-00000000000000000000000000000b02", handle), title, "the write landed after Allow")

	reqs := m.requests()
	require.Len(t, reqs, 3)
	stub := toolIn(reqs[0], "knomit_learn")
	require.NotNil(t, stub, "knomit_learn is in the first request")
	require.Equal(t, "write a new fact (the person approves first)", stub.Description)
	require.NotContains(t, string(stub.Schema), `"facts"`, "as a stub: no real parameters")
	full := toolIn(reqs[1], "knomit_learn")
	require.NotNil(t, full)
	require.Contains(t, string(full.Schema), `"facts"`, "the next request has the full definition")

	var stubResult *llm.ToolResult
	for _, it := range reqs[1].Hooks[llm.HookTurn] {
		if it.Entry == nil {
			continue
		}
		for _, b := range it.Entry.Blocks {
			if b.ToolResult != nil && b.ToolResult.ToolUseID == "tu_stub" {
				stubResult = b.ToolResult
			}
		}
	}
	require.NotNil(t, stubResult, "the stub call is answered, so the pair holds")
	require.Equal(t, fmt.Sprintf(llm.NotLoadedCallResult, "knomit_learn"), stubResult.Content)
}

// A gated write allowed once and then repeated byte for byte by the model is
// not run again and not asked about again: one approval, one fact (review N5).
//
// SABOTAGE: mount.go, drop `ShortCircuit: mcp.RepeatAnswerable` -> the repeat
// reaches the gate, a second card is asked and the first assertion fails.
func TestChat_AnAllowedWriteRepeatedIsNeitherAskedNorWrittenAgain(t *testing.T) {
	h := newHarness(t)
	const title = "Written once, repeated once"
	handle := h.bind(writer, "chat-00000000000000000000000000000b03", "alpha")
	h.queue("a1",
		toolStep("tu_w1", "knomit_learn", learnArgs(handle, title), usage),
		toolStep("tu_w2", "knomit_learn", learnArgs(handle, title), usage),
		textStep("done", usage),
	)
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write it"))
	evs, asked := drive(t, h, "uid:501", id, s, map[string]string{"tu_w1": "allow", "tu_w2": "allow"})
	require.Equal(t, []string{"tu_w1"}, approvedIDs(asked), "one approval: the repeat did not ask")
	_, done := find(evs, "turn_done")
	require.True(t, done, names(evs))
	require.Equal(t, 1, strings.Count(h.queryTitles(writer, "chat-00000000000000000000000000000b04", handle), title), "exactly one write")
}
