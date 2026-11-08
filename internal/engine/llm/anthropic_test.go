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
	"time"

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
			require.EqualValues(t, 64000, body["max_tokens"])
			require.Equal(t, map[string]any{"type": "adaptive", "display": "summarized"}, body["thinking"])
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

// Thinking blocks are replayed unchanged, signature included, to EVERY
// Anthropic model, not only the one that produced them: the API drops what the
// target model cannot read (user decision, 2026-11-04). Only a non-Anthropic
// provider loses them (TestEntry_ReplayForDropsOnlyOtherProvidersReasoning).
func TestAnthropicChat_ThinkingReplayedUnchangedToAnyAnthropicModel(t *testing.T) {
	srv, _ := anthropicSSEServer(t,
		sseEvent("message_start", msgStartFull),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG-abc"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		// display "omitted": an empty thinking string, only a signature.
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"SIG-empty"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"redacted_thinking","data":"REDACTED-1"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":2}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"done"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":3}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "turn", Hook: HookTurn, Text: "q"}))
	var reasoning []*Reasoning
	resp, err := chat.Stream(context.Background(), b.Snapshot(), StreamOptions{}, func(e Event) {
		if e.Kind == EventReasoning {
			reasoning = append(reasoning, e.Reasoning)
		}
	})
	require.NoError(t, err)
	require.Len(t, reasoning, 3)
	require.Equal(t, "anthropic", reasoning[0].Provider)
	require.Equal(t, "claude-test-x", reasoning[0].Model)
	require.JSONEq(t, `{"type":"thinking","thinking":"let me think","signature":"SIG-abc"}`, string(reasoning[0].Native))
	require.JSONEq(t, `{"type":"thinking","signature":"SIG-empty"}`, string(reasoning[1].Native))
	require.JSONEq(t, `{"type":"redacted_thinking","data":"REDACTED-1"}`, string(reasoning[2].Native))

	hist := NewBuilder()
	require.NoError(t, hist.Add(Item{ID: "u", Hook: HookConversation, Entry: &Entry{ID: "u", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "q"}}}}))
	require.NoError(t, hist.Add(Item{ID: "a", Hook: HookConversation, Entry: &Entry{ID: "a", Role: RoleAssistant, Blocks: resp.Blocks}}))
	require.NoError(t, hist.Add(Item{ID: "turn", Hook: HookTurn, Text: "next"}))
	snap := hist.Snapshot()

	// The same assistant content, in order, for the producing model and for
	// a different Anthropic model alike.
	want := `[{"signature":"SIG-abc","thinking":"let me think","type":"thinking"},{"signature":"SIG-empty","thinking":"","type":"thinking"},{"data":"REDACTED-1","type":"redacted_thinking"},{"text":"done","type":"text"}]`
	for _, model := range []string{"claude-test-x", "claude-test-y"} {
		raw, err := json.Marshal(renderAnthropic(snap, model, AnthropicOptions{}))
		require.NoError(t, err)
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(raw, &req))
		require.Len(t, req.Messages, 3, model)
		require.Equal(t, "assistant", req.Messages[1].Role, model)
		require.Equal(t, want, string(req.Messages[1].Content), model)
	}
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
	c := NewAnthropicChat("claude-test-x", AnthropicOptions{ContextWindow: 123456, Profile: ModelProfile{MaxOutput: 777}})
	require.Equal(t, Caps{Tools: true, MaxBreakpoints: 4, ContextWindow: 123456, MaxOutput: 777, StablePrefix: true}, c.Caps())
	require.Equal(t, Caps{Tools: true, MaxBreakpoints: 4, ContextWindow: 200000, MaxOutput: 64000, StablePrefix: true}, NewAnthropicChat("m", AnthropicOptions{}).Caps())
	require.Equal(t, "anthropic", c.Provider())
	require.Equal(t, "claude-test-x", c.Model())
}

// scriptedServer answers POST /v1/messages by running script, which writes
// SSE events and may flush and wait; bodies records each request body.
func scriptedServer(t *testing.T, script func(w http.ResponseWriter, r *http.Request, flush func())) (*httptest.Server, func() [][]byte) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		script(w, r, func() {
			if fl != nil {
				fl.Flush()
			}
		})
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

func writeEvents(w io.Writer, events ...string) {
	for _, e := range events {
		_, _ = io.WriteString(w, e)
	}
}

func textEndStream(stop, deltaExtra string) []string {
	return []string{
		sseEvent("message_start", msgStartFull),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`","stop_sequence":null`+deltaExtra+`},"usage":{"output_tokens":44}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	}
}

func oneTurn(t *testing.T) Snapshot {
	t.Helper()
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "turn", Hook: HookTurn, Text: "q"}))
	return b.Snapshot()
}

// One request shape for every Anthropic model: adaptive thinking with an
// explicit display, an explicit effort, max_tokens from the profile, and none
// of the fields current models reject.
func TestAnthropicChat_RequestShapeFromProfile(t *testing.T) {
	cases := []struct {
		name     string
		profile  ModelProfile
		thinking map[string]any
		effort   string
		max      float64
	}{
		{"defaults", ModelProfile{}, map[string]any{"type": "adaptive", "display": "summarized"}, "medium", 64000},
		{"profile", ModelProfile{Effort: EffortXHigh, MaxOutput: 32000, ThinkingDisplay: DisplayOmitted}, map[string]any{"type": "adaptive", "display": "omitted"}, "xhigh", 32000},
		{"sonnet-like", ModelProfile{Effort: EffortHigh}, map[string]any{"type": "adaptive", "display": "summarized"}, "high", 64000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, bodies := anthropicSSEServer(t, textEndStream("end_turn", "")...)
			chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL, Profile: tc.profile})
			_, err := chat.Stream(context.Background(), prefixSnapshot(t, false), StreamOptions{}, nil)
			require.NoError(t, err)
			bs := bodies()
			require.Len(t, bs, 1)
			var body map[string]any
			require.NoError(t, json.Unmarshal(bs[0], &body))
			require.Equal(t, tc.thinking, body["thinking"])
			require.Equal(t, map[string]any{"effort": tc.effort}, body["output_config"])
			require.EqualValues(t, tc.max, body["max_tokens"])
			for _, k := range []string{"tool_choice", "temperature", "top_p", "top_k"} {
				require.NotContains(t, body, k)
			}
			require.NotContains(t, string(bs[0]), "budget_tokens")
			require.NotContains(t, string(bs[0]), `"disabled"`)
		})
	}

	// A per-request MaxOutput still overrides the profile.
	srv, bodies := anthropicSSEServer(t, textEndStream("end_turn", "")...)
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
	_, err := chat.Stream(context.Background(), oneTurn(t), StreamOptions{MaxOutput: 1234}, nil)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(bodies()[0], &body))
	require.EqualValues(t, 1234, body["max_tokens"])

	// An unknown effort or display fails before any request is sent.
	for _, p := range []ModelProfile{{Effort: "turbo"}, {ThinkingDisplay: "full"}} {
		srv, bodies := anthropicSSEServer(t, textEndStream("end_turn", "")...)
		chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL, Profile: p})
		_, err := chat.Stream(context.Background(), oneTurn(t), StreamOptions{}, nil)
		require.ErrorContains(t, err, "unknown")
		require.Empty(t, bodies())
	}
}

// max_tokens and refusal are not answers: they come back as a *StopError
// carrying the partial response, with no EventDone. pause_turn is a normal
// response.
func TestAnthropicChat_StopReasons(t *testing.T) {
	run := func(t *testing.T, events []string) (*Response, error, []Event) {
		srv, _ := anthropicSSEServer(t, events...)
		chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
		var got []Event
		resp, err := chat.Stream(context.Background(), oneTurn(t), StreamOptions{}, func(e Event) { got = append(got, e) })
		return resp, err, got
	}

	t.Run("max_tokens", func(t *testing.T) {
		resp, err, got := run(t, textEndStream("max_tokens", ""))
		require.Nil(t, resp)
		require.ErrorIs(t, err, ErrTruncated)
		require.NotErrorIs(t, err, ErrRefused)
		var se *StopError
		require.ErrorAs(t, err, &se)
		require.Equal(t, StopMaxTokens, se.Reason)
		require.Equal(t, "partial", se.Response.Blocks[0].Text)
		require.Equal(t, int64(44), se.Response.Usage.Output)
		require.Equal(t, []Event{{Kind: EventTextDelta, Text: "partial"}}, got) // no EventDone
		require.False(t, isRetryable(err))
	})
	t.Run("refusal", func(t *testing.T) {
		_, err, got := run(t, textEndStream("refusal", `,"stop_details":{"type":"refusal","category":"cyber","explanation":"x"}`))
		require.ErrorIs(t, err, ErrRefused)
		var se *StopError
		require.ErrorAs(t, err, &se)
		require.Equal(t, StopRefusal, se.Reason)
		require.Equal(t, "cyber", se.Category)
		require.Equal(t, "llm: the model declined this request (category: cyber)", err.Error())
		require.Len(t, got, 1)
		require.False(t, isRetryable(err))
	})
	t.Run("refusal without details", func(t *testing.T) {
		_, err, _ := run(t, textEndStream("refusal", ""))
		var se *StopError
		require.ErrorAs(t, err, &se)
		require.Equal(t, "", se.Category)
		require.Equal(t, "llm: the model declined this request", err.Error())
	})
	t.Run("model_context_window_exceeded", func(t *testing.T) {
		resp, err, got := run(t, textEndStream("model_context_window_exceeded", ""))
		require.Nil(t, resp)
		require.ErrorIs(t, err, ErrContextFull)
		require.NotErrorIs(t, err, ErrTruncated)
		var se *StopError
		require.ErrorAs(t, err, &se)
		require.Equal(t, StopContextWindowExceeded, se.Reason)
		require.Equal(t, "partial", se.Response.Blocks[0].Text)
		require.Equal(t, int64(44), se.Response.Usage.Output)
		require.Equal(t, []Event{{Kind: EventTextDelta, Text: "partial"}}, got) // no EventDone
		require.False(t, isRetryable(err))
	})
	t.Run("pause_turn", func(t *testing.T) {
		resp, err, got := run(t, textEndStream("pause_turn", ""))
		require.NoError(t, err)
		require.Equal(t, StopPauseTurn, resp.StopReason)
		require.Equal(t, "partial", resp.Blocks[0].Text)
		require.Equal(t, EventDone, got[len(got)-1].Kind)
	})
}

// Arguments the model streams as invalid JSON never reach the neutral history
// as invalid JSON: Args is {}, the bytes are in RawArgs, and ArgsErr is the
// typed error the loop turns into a tool error. The entry still marshals,
// canonicalizes and renders.
func TestAnthropicChat_InvalidToolArgsAreTyped(t *testing.T) {
	srv, _ := anthropicSSEServer(t,
		sseEvent("message_start", msgStartFull),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"knomit_query","input":{}}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":5}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
	var events []Event
	resp, err := chat.Stream(context.Background(), oneTurn(t), StreamOptions{NewID: func() string { return "tu_7" }}, func(e Event) { events = append(events, e) })
	require.NoError(t, err)
	tu := resp.Blocks[0].ToolUse
	require.Equal(t, `{}`, string(tu.Args))
	require.Equal(t, `{"q":`, tu.RawArgs)
	require.Same(t, tu, events[0].ToolUse)

	argsErr := tu.ArgsErr()
	require.ErrorIs(t, argsErr, ErrInvalidToolArgs)
	var ia *InvalidToolArgsError
	require.ErrorAs(t, argsErr, &ia)
	require.Equal(t, InvalidToolArgsError{ToolUseID: "tu_7", Name: "knomit_query", Raw: `{"q":`}, *ia)
	require.NoError(t, (&ToolUse{Args: json.RawMessage(`{}`)}).ArgsErr())

	entry := &Entry{ID: "a", Role: RoleAssistant, Blocks: resp.Blocks}
	_, err = json.Marshal(entry)
	require.NoError(t, err)
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "a", Hook: HookConversation, Entry: entry}))
	require.NoError(t, b.Add(Item{ID: "turn", Hook: HookTurn, Text: "next"}))
	snap := b.Snapshot()
	require.Contains(t, string(snap.CanonicalPrefix(HookTurn)), `"raw_args":"{\"q\":"`)
	raw, err := json.Marshal(renderAnthropic(snap, "claude-test-x", AnthropicOptions{}))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"id":"tu_7","input":{},"name":"knomit_query"`)
}

// Every stream event calls Progress, including the thinking, signature and
// tool-argument deltas that never reach the sink.
func TestAnthropicChat_ProgressOnEveryStreamEvent(t *testing.T) {
	events := []string{
		sseEvent("message_start", msgStartFull),
		sseEvent("ping", `{"type":"ping"}`), // the SDK swallows pings
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"S"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"knomit_learn","input":{}}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"body\":"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"long\"}"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":5}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	}
	srv, _ := anthropicSSEServer(t, events...)
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
	var trail []string // "p" per Progress, the event kind per sink event
	_, err := chat.Stream(context.Background(), oneTurn(t), StreamOptions{Progress: func() { trail = append(trail, "p") }},
		func(e Event) { trail = append(trail, string(e.Kind)) })
	require.NoError(t, err)
	// 11 non-ping events, each one Progress; the sink sees the reasoning at
	// the thinking block's stop, the tool_use at its stop, and done.
	require.Equal(t, []string{
		"p",           // message_start
		"p", "p", "p", // thinking start, thinking_delta, signature_delta
		"p", "reasoning", // thinking stop
		"p", "p", "p", // tool_use start, two input_json_deltas
		"p", "tool_use", // tool_use stop
		"p", "p", // message_delta, message_stop
		"done",
	}, trail)
}

// End to end through WrapChat: a stream that thinks past AttemptTimeout before
// its first sink event is alive and is not cut off; a server that sends
// nothing for AttemptTimeout is a stall.
func TestWrapChat_AnthropicThinkingIsNotAStall(t *testing.T) {
	const timeout = 60 * time.Millisecond
	policy := Policy{AttemptTimeout: timeout} // anthropic's budget: no retries

	t.Run("thinking keeps it alive", func(t *testing.T) {
		srv, bodies := scriptedServer(t, func(w http.ResponseWriter, r *http.Request, flush func()) {
			writeEvents(w,
				sseEvent("message_start", msgStartFull),
				sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
				sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"long thought"}}`))
			flush()
			time.Sleep(4 * timeout) // thinking, with no sink event yet
			writeEvents(w,
				sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"S"}}`),
				sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`))
			writeEvents(w, textEndStream("end_turn", "")[1:]...)
		})
		chat := WrapChat(NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL}), policy)
		resp, err := chat.Stream(context.Background(), oneTurn(t), StreamOptions{}, nil)
		require.NoError(t, err)
		require.Equal(t, StopEndTurn, resp.StopReason)
		require.Equal(t, BlockReasoning, resp.Blocks[0].Kind)
		require.Equal(t, "partial", resp.Blocks[1].Text)
		require.Len(t, bodies(), 1)
	})

	t.Run("silence is a stall", func(t *testing.T) {
		srv, _ := scriptedServer(t, func(w http.ResponseWriter, r *http.Request, flush func()) {
			flush() // headers only
			select {
			case <-time.After(10 * timeout):
			case <-r.Context().Done():
				return
			}
			writeEvents(w, textEndStream("end_turn", "")...)
		})
		chat := WrapChat(NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL}), policy)
		_, err := chat.Stream(context.Background(), oneTurn(t), StreamOptions{}, nil)
		require.ErrorIs(t, err, errAttemptStalled)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

// An SSE error event after message_start (an overload mid-stream) is an
// error, after whatever text already streamed, and there is no EventDone.
func TestAnthropicChat_MidStreamErrorEvent(t *testing.T) {
	srv, _ := anthropicSSEServer(t,
		sseEvent("message_start", msgStartFull),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`),
		sseEvent("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"NEVER"}}`),
	)
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
	var got []Event
	resp, err := chat.Stream(context.Background(), oneTurn(t), StreamOptions{}, func(e Event) { got = append(got, e) })
	require.Nil(t, resp)
	require.ErrorContains(t, err, "overloaded_error")
	require.Equal(t, []Event{{Kind: EventTextDelta, Text: "Hel"}}, got)

	// Through WrapChat the text already reached the sink, so it is not
	// retried even under a budget that would allow it.
	f := 0
	wrapped := WrapChat(NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL}), Policy{MaxRetries: 2})
	_, err = wrapped.Stream(context.Background(), oneTurn(t), StreamOptions{}, func(Event) { f++ })
	require.ErrorContains(t, err, "overloaded_error")
	require.Equal(t, 1, f)
}

// Cancelling the context mid-stream ends Stream promptly with the
// cancellation, and no EventDone.
func TestAnthropicChat_CancelMidStream(t *testing.T) {
	srv, _ := scriptedServer(t, func(w http.ResponseWriter, r *http.Request, flush func()) {
		writeEvents(w, textEndStream("end_turn", "")[:3]...)
		flush()
		<-r.Context().Done() // the client went away
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
	var got []Event
	start := time.Now()
	resp, err := chat.Stream(ctx, oneTurn(t), StreamOptions{}, func(e Event) {
		got = append(got, e)
		cancel() // the user hits stop on the first text
	})
	require.Nil(t, resp)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 5*time.Second)
	require.Equal(t, []Event{{Kind: EventTextDelta, Text: "partial"}}, got)
}

// AnthropicPrefixJSON is exactly the "system" and "tools" bytes of the HTTP
// body the API receives.
func TestAnthropicPrefixJSON_EqualsReceivedBody(t *testing.T) {
	srv, bodies := anthropicSSEServer(t, textEndStream("end_turn", "")...)
	snap := prefixSnapshot(t, true)
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
	_, err := chat.Stream(context.Background(), snap, StreamOptions{}, nil)
	require.NoError(t, err)

	var received, exported map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(bodies()[0], &received))
	prefix, err := AnthropicPrefixJSON(snap, "claude-test-x")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(prefix, &exported))
	require.Len(t, exported, 2)
	for _, k := range []string{"system", "tools"} {
		require.NotEmpty(t, received[k], k)
		require.Equal(t, string(received[k]), string(exported[k]), k)
	}
	require.Contains(t, string(received["system"]), "FRAME B")
}

func TestAnthropicChat_PromptTooLongIsContextFull(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		full   bool
	}{
		"400 prompt is too long": {http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 1048577 tokens > 1000000 maximum"}}`, true},
		"400 another reason":     {http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.0: bad role"}}`, false},
		"413 is not this error":  {http.StatusRequestEntityTooLarge, `{"type":"error","error":{"type":"request_too_large","message":"prompt is too long"}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
			_, err := chat.Stream(context.Background(), prefixSnapshot(t, false), StreamOptions{}, nil)
			require.Error(t, err)
			if tc.full {
				require.ErrorIs(t, err, ErrContextFull)
				require.Contains(t, err.Error(), "prompt is too long: 1048577 tokens")
				require.False(t, isRetryable(err))
			} else {
				require.NotErrorIs(t, err, ErrContextFull)
			}
		})
	}
}

func TestAnthropicChat_CacheWriteSplitByTTL(t *testing.T) {
	start := `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":11,"cache_creation_input_tokens":33,"cache_read_input_tokens":22,"output_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":13}}}}`
	srv, _ := anthropicSSEServer(t, textAndToolStream(start, `{"output_tokens":44}`)...)
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
	resp, err := chat.Stream(context.Background(), prefixSnapshot(t, false), StreamOptions{}, nil)
	require.NoError(t, err)
	require.Equal(t, Usage{Input: 11, CacheRead: 22, CacheWrite: 33, CacheWrite5m: 20, CacheWrite1h: 13, Output: 44}, resp.Usage)
}

// NoThinking (a chat's title request) disables thinking on the wire and keeps
// the tight max_tokens. SABOTAGE: drop the override in AnthropicChat.Stream -> red.
func TestAnthropicChat_NoThinkingDisablesThinking(t *testing.T) {
	srv, bodies := anthropicSSEServer(t, textAndToolStream(msgStartZero, `{"input_tokens":11,"output_tokens":4}`)...)
	chat := NewAnthropicChat("claude-test-x", AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
	_, err := chat.Stream(context.Background(), prefixSnapshot(t, false), StreamOptions{NoThinking: true, MaxOutput: 32}, nil)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(bodies()[0], &body))
	require.Equal(t, map[string]any{"type": "disabled"}, body["thinking"])
	require.EqualValues(t, 32, body["max_tokens"])
}
