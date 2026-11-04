package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// sseEvent renders one Messages-stream event the way the API sends it.
func sseEvent(typ, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", typ, data)
}

// anthropicSSEServer answers every POST /v1/messages with events and records
// each request body it received.
func anthropicSSEServer(t *testing.T, events ...string) (*httptest.Server, func() []json.RawMessage) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies []json.RawMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, e)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []json.RawMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([]json.RawMessage(nil), bodies...)
	}
}

// prefixSnapshot is a snapshot with every shared hook populated and a turn.
func prefixSnapshot(t *testing.T, frame bool) Snapshot {
	t.Helper()
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "tool:knomit_query", Hook: HookTools, Tool: &ToolDef{
		Name: "knomit_query", Description: "Search.",
		Schema: json.RawMessage(`{"properties":{"text":{"type":"string"}},"required":["text"],"type":"object"}`),
	}}))
	require.NoError(t, b.Add(Item{ID: "system", Hook: HookSystem, Text: "SYSTEM PROMPT"}))
	require.NoError(t, b.Add(Item{ID: "instructions", Hook: HookRepo, Text: "INSTRUCTIONS"}))
	if frame {
		require.NoError(t, b.Add(Item{ID: "frame:a", Hook: HookFrame, Text: "FRAME A"}))
		require.NoError(t, b.Add(Item{ID: "frame:b", Hook: HookFrame, Text: "FRAME B"}))
	}
	require.NoError(t, b.Add(Item{ID: "ws:1", Hook: HookWorkingSet, Text: "WORKING FACT"}))
	require.NoError(t, b.Add(Item{ID: "c1", Hook: HookConversation, Entry: &Entry{ID: "c1", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "earlier question"}}}}))
	require.NoError(t, b.Add(Item{ID: "c2", Hook: HookConversation, Entry: &Entry{ID: "c2", Role: RoleAssistant, Blocks: []Block{{Kind: BlockText, Text: "earlier answer"}}}}))
	require.NoError(t, b.Add(Item{ID: "turn", Hook: HookTurn, Entry: &Entry{ID: "turn", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "THE TURN"}}}}))
	return b.Snapshot()
}

// cacheControls finds every cache_control object in a marshalled request and
// returns the JSON path of the block carrying it with its ttl.
func cacheControls(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(raw, &v))
	out := map[string]string{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if cc, ok := x["cache_control"].(map[string]any); ok {
				out[path] = fmt.Sprint(cc["ttl"])
				require.Equal(t, "ephemeral", cc["type"], path)
			}
			for k, c := range x {
				walk(path+"."+k, c)
			}
		case []any:
			for i, c := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), c)
			}
		}
	}
	walk("$", v)
	return out
}

func TestRenderAnthropic_BreakpointsAtHookBoundaries(t *testing.T) {
	raw, err := json.Marshal(renderAnthropic(prefixSnapshot(t, false), "claude-test-x", AnthropicOptions{}))
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"$.system[1]":              "1h", // end of hook 3: tools + system + instructions
		"$.messages[2].content[0]": "5m", // last block of the last message
	}, cacheControls(t, raw))

	var req struct {
		System   []struct{ Text string } `json:"system"`
		Messages []struct{ Role string } `json:"messages"`
		Tools    []map[string]any        `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(raw, &req))
	require.Equal(t, []string{"SYSTEM PROMPT", "INSTRUCTIONS", "WORKING FACT"},
		[]string{req.System[0].Text, req.System[1].Text, req.System[2].Text})
	require.Equal(t, []string{"user", "assistant", "user"}, []string{req.Messages[0].Role, req.Messages[1].Role, req.Messages[2].Role})
	// The tool's schema goes out as the canonical bytes, whole.
	require.Contains(t, string(raw), `"input_schema":{"properties":{"text":{"type":"string"}},"required":["text"],"type":"object"}`)
	require.NotContains(t, req.Tools[0], "cache_control")

	// A frame adds one 1h breakpoint after its LAST item, and the working set
	// after it stays uncached.
	raw, err = json.Marshal(renderAnthropic(prefixSnapshot(t, true), "claude-test-x", AnthropicOptions{}))
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"$.system[1]":              "1h",
		"$.system[3]":              "1h",
		"$.messages[2].content[0]": "5m",
	}, cacheControls(t, raw))
	require.Contains(t, string(raw), `"text":"FRAME B","cache_control"`)

	// The API renders tools, then system, then messages, whatever the JSON key
	// order, so every 1h breakpoint (system) precedes the 5m one (messages).
}

func TestRenderAnthropic_ToolResultFollowsToolUse(t *testing.T) {
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "system", Hook: HookSystem, Text: "S"}))
	require.NoError(t, b.Add(Item{ID: "a1", Hook: HookConversation, Entry: &Entry{ID: "a1", Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockText, Text: "checking"},
		{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "tu_1", Name: "knomit_repos", Args: json.RawMessage(`{}`)}},
	}}}))
	require.NoError(t, b.Add(Item{ID: "u1", Hook: HookConversation, Entry: &Entry{ID: "u1", Role: RoleUser, Blocks: []Block{
		{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "tu_1", Content: "alpha, beta", IsError: true}},
	}}}))
	raw, err := json.Marshal(renderAnthropic(b.Snapshot(), "claude-test-x", AnthropicOptions{}))
	require.NoError(t, err)

	var req struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(raw, &req))
	require.Len(t, req.Messages, 2)
	require.Equal(t, "assistant", req.Messages[0].Role)
	require.Equal(t, "tool_use", req.Messages[0].Content[1]["type"])
	require.Equal(t, "tu_1", req.Messages[0].Content[1]["id"])
	require.Equal(t, "knomit_repos", req.Messages[0].Content[1]["name"])
	require.Equal(t, "user", req.Messages[1].Role)
	res := req.Messages[1].Content[0]
	require.Equal(t, "tool_result", res["type"])
	require.Equal(t, "tu_1", res["tool_use_id"])
	require.Equal(t, true, res["is_error"])
	// The rolling breakpoint sits on the tool_result, the last block.
	require.Equal(t, map[string]string{"$.system[0]": "1h", "$.messages[1].content[0]": "5m"}, cacheControls(t, raw))
}

func TestRenderAnthropic_MergesConsecutiveUserItems(t *testing.T) {
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "note", Hook: HookConversation, Text: "session note"}))
	require.NoError(t, b.Add(Item{ID: "turn", Hook: HookTurn, Text: "question"}))
	raw, err := json.Marshal(renderAnthropic(b.Snapshot(), "claude-test-x", AnthropicOptions{}))
	require.NoError(t, err)
	var req struct {
		Messages []struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(raw, &req))
	require.Len(t, req.Messages, 1)
	require.Equal(t, "session note", req.Messages[0].Content[0].Text)
	require.Equal(t, "question", req.Messages[0].Content[1].Text)
}

const (
	msgStartZero = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`
	msgStartFull = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":11,"cache_creation_input_tokens":33,"cache_read_input_tokens":22,"output_tokens":1}}}`
)

func textAndToolStream(start, deltaUsage string) []string {
	return []string{
		sseEvent("message_start", start),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_provider_9","name":"knomit_query","input":{}}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":`+deltaUsage+`}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	}
}

func TestAnthropicChat_StreamMapsEventsAndUsage(t *testing.T) {
	// Two real response shapes: the counts arrive cumulatively in
	// message_delta, or input and cache counts arrive in message_start and
	// message_delta carries only the output count.
	for name, ev := range map[string][]string{
		"delta carries every count": textAndToolStream(msgStartZero, `{"input_tokens":11,"cache_creation_input_tokens":33,"cache_read_input_tokens":22,"output_tokens":44}`),
		"start carries input/cache": textAndToolStream(msgStartFull, `{"output_tokens":44}`),
	} {
		t.Run(name, func(t *testing.T) {
			srv, bodies := anthropicSSEServer(t, ev...)
			chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
			var got []Event
			n := 0
			resp, err := chat.Stream(context.Background(), prefixSnapshot(t, false),
				StreamOptions{NewID: func() string { n++; return fmt.Sprintf("tu_%d", 40+n) }},
				func(e Event) { got = append(got, e) })
			require.NoError(t, err)

			require.Len(t, got, 4)
			require.Equal(t, Event{Kind: EventTextDelta, Text: "Hel"}, got[0])
			require.Equal(t, Event{Kind: EventTextDelta, Text: "lo"}, got[1])
			require.Equal(t, EventToolUse, got[2].Kind)
			require.Equal(t, "tu_41", got[2].ToolUse.ID) // knomit id, not toolu_provider_9
			require.Equal(t, "knomit_query", got[2].ToolUse.Name)
			require.JSONEq(t, `{"q":"x"}`, string(got[2].ToolUse.Args))
			require.Equal(t, EventDone, got[3].Kind)

			require.Equal(t, Usage{Input: 11, CacheRead: 22, CacheWrite: 33, Output: 44}, resp.Usage)
			require.Equal(t, "tool_use", resp.StopReason)
			require.Len(t, resp.Blocks, 2)
			require.Equal(t, "Hello", resp.Blocks[0].Text)
			require.Equal(t, "tu_41", resp.Blocks[1].ToolUse.ID)
			require.NotContains(t, string(mustJSON(t, resp)), "toolu_provider_9")

			// What the server received.
			bs := bodies()
			require.Len(t, bs, 1)
			var body map[string]any
			require.NoError(t, json.Unmarshal(bs[0], &body))
			require.Equal(t, "claude-test-x", body["model"])
			require.Equal(t, true, body["stream"])
			require.EqualValues(t, defaultMaxTokens, body["max_tokens"])
			require.NotContains(t, body, "thinking")
			require.Equal(t, map[string]string{"$.system[1]": "1h", "$.messages[2].content[0]": "5m"}, cacheControls(t, bs[0]))
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestAnthropicChat_ThinkingReplayedOnlyToSameModel(t *testing.T) {
	srv, bodies := anthropicSSEServer(t,
		sseEvent("message_start", msgStartFull),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG-abc"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"done"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL, ThinkingBudget: 9000})
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "turn", Hook: HookTurn, Text: "q"}))
	var reasoning []*Reasoning
	resp, err := chat.Stream(context.Background(), b.Snapshot(), StreamOptions{}, func(e Event) {
		if e.Kind == EventReasoning {
			reasoning = append(reasoning, e.Reasoning)
		}
	})
	require.NoError(t, err)
	require.Len(t, reasoning, 1)
	require.Equal(t, "anthropic", reasoning[0].Provider)
	require.Equal(t, "claude-test-x", reasoning[0].Model)
	require.JSONEq(t, `{"type":"thinking","thinking":"let me think","signature":"SIG-abc"}`, string(reasoning[0].Native))

	// The request asked for thinking with the budget, and max_tokens above it.
	var body map[string]any
	require.NoError(t, json.Unmarshal(bodies()[0], &body))
	require.Equal(t, map[string]any{"type": "enabled", "budget_tokens": float64(9000)}, body["thinking"])
	require.EqualValues(t, 9000+defaultMaxTokens, body["max_tokens"])

	// Put the turn and the answer into history and render for both models.
	hist := NewBuilder()
	require.NoError(t, hist.Add(Item{ID: "u", Hook: HookConversation, Entry: &Entry{ID: "u", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "q"}}}}))
	require.NoError(t, hist.Add(Item{ID: "a", Hook: HookConversation, Entry: &Entry{ID: "a", Role: RoleAssistant, Blocks: resp.Blocks}}))
	require.NoError(t, hist.Add(Item{ID: "turn", Hook: HookTurn, Text: "next"}))
	snap := hist.Snapshot()

	same, err := json.Marshal(renderAnthropic(snap, "claude-test-x", AnthropicOptions{}))
	require.NoError(t, err)
	require.Contains(t, string(same), `{"signature":"SIG-abc","thinking":"let me think","type":"thinking"}`)

	other, err := json.Marshal(renderAnthropic(snap, "claude-test-y", AnthropicOptions{}))
	require.NoError(t, err)
	require.NotContains(t, string(other), `"thinking"`)
	require.NotContains(t, string(other), "SIG-abc")
	require.Contains(t, string(other), `"text":"done"`)
}

func TestAnthropicChat_HTTPErrorIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad tool_use id"}}`)
	}))
	t.Cleanup(srv.Close)
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
	var events int
	_, err := chat.Stream(context.Background(), prefixSnapshot(t, false), StreamOptions{}, func(Event) { events++ })
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad tool_use id")
	require.Zero(t, events) // no EventDone on failure
}

func TestAnthropicChat_Caps(t *testing.T) {
	c := NewAnthropicChat("claude-test-x", AnthropicOptions{ContextWindow: 123456, MaxOutput: 777})
	require.Equal(t, Caps{Tools: true, MaxBreakpoints: 4, ContextWindow: 123456, MaxOutput: 777}, c.Caps())
	require.Equal(t, Caps{Tools: true, MaxBreakpoints: 4, ContextWindow: 200000, MaxOutput: 8192}, NewAnthropicChat("m", AnthropicOptions{}).Caps())
	require.Equal(t, "anthropic", c.Provider())
	require.Equal(t, "claude-test-x", c.Model())
}
