package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOllamaChat_CapsFromShow(t *testing.T) {
	t.Run("tools", func(t *testing.T) {
		c := newTestOllama(t, newOllamaServer(t, []string{"completion", "tools"}), OllamaOptions{NumCtx: 8192, MaxOutput: 512})
		require.True(t, c.Caps().Tools)
		require.Equal(t, "", c.UnusableReason())
		require.Equal(t, Caps{Tools: true, MaxBreakpoints: 0, ContextWindow: 8192, MaxOutput: 512, StreamStoppable: true}, c.Caps())
	})
	t.Run("window is capped at the trained context", func(t *testing.T) {
		srv := newOllamaServer(t, []string{"tools"})
		srv.info = map[string]any{"general.architecture": "llama", "llama.context_length": 8000}
		c := newTestOllama(t, srv, OllamaOptions{NumCtx: 32768})
		require.Equal(t, 8000, c.Caps().ContextWindow)
		// num_ctx smaller than the trained context stays as configured.
		c = newTestOllama(t, srv, OllamaOptions{NumCtx: 4096})
		require.Equal(t, 4096, c.Caps().ContextWindow)
	})
	t.Run("no trained context reported keeps num_ctx", func(t *testing.T) {
		srv := newOllamaServer(t, []string{"tools"})
		srv.info = map[string]any{"general.architecture": "llama"}
		require.Equal(t, 32768, newTestOllama(t, srv, OllamaOptions{}).Caps().ContextWindow)
	})
	t.Run("no tools", func(t *testing.T) {
		srv := newOllamaServer(t, []string{"completion"})
		c := newTestOllama(t, srv, OllamaOptions{})
		require.False(t, c.Caps().Tools)
		require.Contains(t, c.UnusableReason(), "does not support tool calling")
		_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.ErrorContains(t, err, "unusable")
		require.Zero(t, srv.chats, "an unusable model never reaches /api/chat")
	})
	t.Run("probe 500 fails closed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{Host: srv.URL, NumCtx: 32768})
		require.NoError(t, err)
		require.False(t, c.Caps().Tools)
		require.Contains(t, c.UnusableReason(), "capability probe failed")
		require.Contains(t, c.UnusableReason(), "HTTP 500")
	})
	t.Run("server unreachable fails closed", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{Host: url})
		require.NoError(t, err)
		require.False(t, c.Caps().Tools)
		require.Contains(t, c.UnusableReason(), "capability probe failed")
	})
	t.Run("host from environment", func(t *testing.T) {
		srv := newOllamaServer(t, []string{"tools"})
		t.Setenv("OLLAMA_HOST", strings.TrimPrefix(srv.URL, "http://"))
		c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{})
		require.NoError(t, err)
		require.True(t, c.Caps().Tools)
	})
}

func TestOllamaChat_LengthStopIsAnError(t *testing.T) {
	srv := newOllamaServer(t, []string{"tools"},
		`{"message":{"role":"assistant","content":"partial "},"done":false}`,
		`{"message":{"role":"assistant","content":"text"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done_reason":"length","done":true,"prompt_eval_count":30,"eval_count":9}`,
	)
	c := newTestOllama(t, srv, OllamaOptions{})
	var events []Event
	resp, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, func(e Event) { events = append(events, e) })
	require.Nil(t, resp)
	require.ErrorIs(t, err, ErrTruncated)
	var se *StopError
	require.True(t, errors.As(err, &se))
	require.Equal(t, StopMaxTokens, se.Reason)
	require.Equal(t, []Block{{Kind: BlockText, Text: "partial text"}}, se.Response.Blocks)
	require.Equal(t, int64(9), se.Response.Usage.Output)
	for _, e := range events {
		require.NotEqual(t, EventDone, e.Kind, "a truncated stream never reports done")
	}
	require.Len(t, events, 2)
}

// shift:false is on the wire, and a generation that overruns the window then
// ends with done_reason "length", which is a *StopError that Is ErrTruncated
// (not a silent "stop" after llama-server discarded KV).
func TestOllamaChat_ShiftFalseOverrunIsErrTruncated(t *testing.T) {
	srv := newOllamaServer(t, []string{"tools"},
		`{"message":{"role":"assistant","content":"cut off"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done_reason":"length","done":true,"prompt_eval_count":32000,"eval_count":768}`,
	)
	resp, err := newTestOllama(t, srv, OllamaOptions{}).Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
	require.Nil(t, resp)
	require.Equal(t, `false`, string(srv.received()[0]["shift"]), `request body carries "shift":false`)
	require.ErrorIs(t, err, ErrTruncated)
	var se *StopError
	require.True(t, errors.As(err, &se))
	require.Equal(t, StopMaxTokens, se.Reason)
}

func TestOllamaChat_AlwaysSendsNumCtxAndKeepAlive(t *testing.T) {
	done := `{"message":{"role":"assistant","content":"ok"},"done_reason":"stop","done":true}`
	for _, tc := range []struct {
		name      string
		o         OllamaOptions
		stream    StreamOptions
		ctx, pred int
		keepAlive string
	}{
		{"defaults send no cap", OllamaOptions{}, StreamOptions{}, 32768, -1, `"30m"`},
		{"small window still sends no cap", OllamaOptions{NumCtx: 4096}, StreamOptions{}, 4096, -1, `"30m"`},
		{"negative means uncapped", OllamaOptions{MaxOutput: -1}, StreamOptions{}, 32768, -1, `"30m"`},
		{"configured", OllamaOptions{NumCtx: 4096, KeepAlive: "2h", MaxOutput: 300}, StreamOptions{}, 4096, 300, `"2h"`},
		{"per-request output", OllamaOptions{NumCtx: 4096, MaxOutput: 300}, StreamOptions{MaxOutput: 77}, 4096, 77, `"30m"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOllamaServer(t, []string{"tools"}, done)
			c := newTestOllama(t, srv, tc.o)
			_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), tc.stream, nil)
			require.NoError(t, err)
			b := srv.received()[0]
			var opts map[string]int
			require.NoError(t, json.Unmarshal(b["options"], &opts))
			require.Equal(t, map[string]int{"num_ctx": tc.ctx, "num_predict": tc.pred}, opts)
			require.Equal(t, tc.keepAlive, string(b["keep_alive"]))
			require.Equal(t, `false`, string(b["truncate"]), "truncate:false on every request")
			require.Equal(t, `false`, string(b["shift"]), "shift:false on every request")
		})
	}
}

func TestOllamaChat_ThinkingModelStreamsReasoningFirst(t *testing.T) {
	srv := newOllamaServer(t, []string{"tools", "thinking"},
		`{"message":{"role":"assistant","content":"","thinking":"let me "},"done":false}`,
		`{"message":{"role":"assistant","content":"","thinking":"think"},"done":false}`,
		`{"message":{"role":"assistant","content":"Answer."},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done_reason":"stop","done":true}`,
	)
	c := newTestOllama(t, srv, OllamaOptions{})
	var kinds []EventKind
	resp, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, func(e Event) { kinds = append(kinds, e.Kind) })
	require.NoError(t, err)
	require.Equal(t, `true`, string(srv.received()[0]["think"]))
	require.Equal(t, []EventKind{EventReasoning, EventTextDelta, EventDone}, kinds)
	require.Equal(t, StopEndTurn, resp.StopReason)
	require.Len(t, resp.Blocks, 2)
	r := resp.Blocks[0].Reasoning
	require.Equal(t, "ollama", r.Provider)
	require.Equal(t, "test-model", r.Model)
	require.JSONEq(t, `{"thinking":"let me think"}`, string(r.Native))
	require.Equal(t, Block{Kind: BlockText, Text: "Answer."}, resp.Blocks[1])
}

// Thinking that arrives after the first content or tool-call line used to be
// accumulated and then dropped: the reasoning block was flushed once. Each
// thinking run must reach the response, the sink and the trace, in stream
// order, between the content and tool calls around it.
func TestOllamaChat_ReasoningAfterFirstFlushIsKept(t *testing.T) {
	srv := newOllamaServer(t, []string{"tools", "thinking"},
		`{"message":{"role":"assistant","content":"","thinking":"first thought"},"done":false}`,
		`{"message":{"role":"assistant","content":"Let me check. "},"done":false}`,
		`{"message":{"role":"assistant","content":"","thinking":"second thought"},"done":false}`,
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"knomit_query","arguments":{"text":"x"}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":"","thinking":"third thought"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done_reason":"stop","done":true}`,
	)
	c := newTestOllama(t, srv, OllamaOptions{})
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	tc, closer, err := NewTracingChat(c, tracePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = closer.Close() })

	var sunk []Event
	resp, err := tc.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{NewID: func() string { return "tu_1" }}, func(e Event) { sunk = append(sunk, e) })
	require.NoError(t, err)

	want := []string{"first thought", "second thought", "third thought"}
	require.Equal(t, []BlockKind{BlockReasoning, BlockText, BlockReasoning, BlockToolUse, BlockReasoning}, blockKinds(resp.Blocks))
	require.Equal(t, want, reasoningTexts(t, resp.Blocks), "response")
	require.Equal(t, "Let me check. ", resp.Blocks[1].Text)

	var sinkKinds []EventKind
	var sinkBlocks []Block
	for _, e := range sunk {
		sinkKinds = append(sinkKinds, e.Kind)
		if e.Kind == EventReasoning {
			sinkBlocks = append(sinkBlocks, Block{Kind: BlockReasoning, Reasoning: e.Reasoning})
		}
	}
	require.Equal(t, []EventKind{EventReasoning, EventTextDelta, EventReasoning, EventToolUse, EventReasoning, EventDone}, sinkKinds)
	require.Equal(t, want, reasoningTexts(t, sinkBlocks), "sink")

	raw, err := os.ReadFile(tracePath)
	require.NoError(t, err)
	var line struct {
		Blocks []Block `json:"blocks"`
	}
	require.NoError(t, json.Unmarshal(raw, &line))
	require.Equal(t, want, reasoningTexts(t, line.Blocks), "trace")
}

func blockKinds(bs []Block) []BlockKind {
	out := make([]BlockKind, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Kind)
	}
	return out
}

// reasoningTexts returns the thinking text of each reasoning block, in order.
func reasoningTexts(t *testing.T, bs []Block) []string {
	t.Helper()
	var out []string
	for _, b := range bs {
		if b.Kind != BlockReasoning {
			continue
		}
		var n struct {
			Thinking string `json:"thinking"`
		}
		require.NoError(t, json.Unmarshal(b.Reasoning.Native, &n))
		out = append(out, n.Thinking)
	}
	return out
}

// ollamaStream runs one Stream over the given lines and returns the response,
// the sink's event kinds and the error.
func ollamaStream(t *testing.T, lines ...string) (*Response, []EventKind, error) {
	t.Helper()
	srv := newOllamaServer(t, []string{"tools", "thinking"}, lines...)
	c := newTestOllama(t, srv, OllamaOptions{})
	ids := []string{"tu_1", "tu_2", "tu_3"}
	n := 0
	var kinds []EventKind
	resp, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{NewID: func() string { n++; return ids[n-1] }}, func(e Event) { kinds = append(kinds, e.Kind) })
	return resp, kinds, err
}

// describeBlocks renders each block as kind:text (or kind:tool name), so one
// assertion pins both the kinds and the texts, in order.
func describeBlocks(bs []Block) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		switch b.Kind {
		case BlockText:
			out = append(out, "text:"+b.Text)
		case BlockToolUse:
			out = append(out, "tool:"+b.ToolUse.Name)
		case BlockReasoning:
			var n struct {
				Thinking string `json:"thinking"`
			}
			_ = json.Unmarshal(b.Reasoning.Native, &n)
			out = append(out, "reasoning:"+n.Thinking)
		}
	}
	return out
}

func TestOllamaChat_EmptyChunksDoNotSplitRuns(t *testing.T) {
	resp, _, err := ollamaStream(t,
		`{"message":{"role":"assistant","content":"","thinking":"a"},"done":false}`,
		`{"message":{"role":"assistant","content":"","thinking":""},"done":false}`,
		`{"message":{"role":"assistant","content":"","thinking":""},"done":false}`,
		`{"message":{"role":"assistant","content":"","thinking":"b"},"done":false}`,
		`{"message":{"role":"assistant","content":"x"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":false}`,
		`{"message":{"role":"assistant","content":"y"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done_reason":"stop","done":true}`,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"reasoning:ab", "text:xy"}, describeBlocks(resp.Blocks))
}

func TestOllamaChat_TextToolText(t *testing.T) {
	resp, _, err := ollamaStream(t,
		`{"message":{"role":"assistant","content":"A"},"done":false}`,
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"knomit_query","arguments":{"text":"x"}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":"B"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done_reason":"stop","done":true}`,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"text:A", "tool:knomit_query", "text:B"}, describeBlocks(resp.Blocks))
	require.Equal(t, StopToolUse, resp.StopReason)
}

func TestOllamaChat_TwoToolCallLinesInARow(t *testing.T) {
	resp, _, err := ollamaStream(t,
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"knomit_repos","arguments":{}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"knomit_query","arguments":{"text":"x"}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done_reason":"stop","done":true}`,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"tool:knomit_repos", "tool:knomit_query"}, describeBlocks(resp.Blocks))
	require.Equal(t, StopToolUse, resp.StopReason)
}

// A stream that ends with no done line is an error: no response is returned,
// so the thinking run still open at the cut is not emitted to the sink.
func TestOllamaChat_StreamEndsWhileThinkingRunIsOpen(t *testing.T) {
	resp, kinds, err := ollamaStream(t,
		`{"message":{"role":"assistant","content":"","thinking":"unfinished"},"done":false}`,
	)
	require.ErrorContains(t, err, "stream ended before the done line")
	require.Nil(t, resp)
	require.NotContains(t, kinds, EventReasoning)
}

func TestRenderOllama_ReasoningReplay(t *testing.T) {
	mk := func(provider string) Snapshot {
		b := NewBuilder()
		require.NoError(t, b.Add(Item{ID: "c1", Hook: HookConversation, Entry: &Entry{ID: "c1", Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockReasoning, Reasoning: &Reasoning{Provider: provider, Model: "m", Native: json.RawMessage(`{"type":"thinking","thinking":"SECRET","signature":"sig"}`)}},
			{Kind: BlockText, Text: "hello"},
		}}}))
		return b.Snapshot()
	}
	foreign := renderOllama(mk("anthropic"), "m", OllamaOptions{})
	raw, _ := json.Marshal(foreign)
	require.NotContains(t, string(raw), "SECRET", "another provider's reasoning is dropped")
	require.NotContains(t, string(raw), "sig")
	require.Equal(t, "hello", foreign.Messages[0].Content)
	require.Empty(t, foreign.Messages[0].Thinking)

	own := renderOllama(mk("ollama"), "m", OllamaOptions{})
	require.Equal(t, "hello", own.Messages[0].Content)
	require.Equal(t, "SECRET", own.Messages[0].Thinking, "Ollama's own reasoning goes back in the thinking field")
}

// Several reasoning runs in one assistant entry replay as one thinking field,
// joined by a newline so the run boundary does not glue words together.
func TestRenderOllama_TwoReasoningRunsJoinWithNewline(t *testing.T) {
	b := NewBuilder()
	reason := func(s string) Block {
		return Block{Kind: BlockReasoning, Reasoning: &Reasoning{Provider: "ollama", Model: "m", Native: json.RawMessage(`{"thinking":"` + s + `"}`)}}
	}
	require.NoError(t, b.Add(Item{ID: "c1", Hook: HookConversation, Entry: &Entry{ID: "c1", Role: RoleAssistant, Blocks: []Block{
		reason("Plan the query."),
		{Kind: BlockText, Text: "Looking."},
		reason("Check the result."),
	}}}))
	out := renderOllama(b.Snapshot(), "m", OllamaOptions{})
	require.Equal(t, "Looking.", out.Messages[0].Content)
	require.Equal(t, "Plan the query.\nCheck the result.", out.Messages[0].Thinking)
}

func TestOllamaChat_ToolArgumentsAreObjects(t *testing.T) {
	srv := newOllamaServer(t, []string{"tools"},
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"knomit_repos","arguments":null}},{"function":{"name":"knomit_query","arguments":{ "text" : "ok" }}}]},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done_reason":"stop","done":true}`,
	)
	c := newTestOllama(t, srv, OllamaOptions{})
	ids := []string{"tu_a", "tu_b"}
	n := 0
	resp, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{NewID: func() string { n++; return ids[n-1] }}, nil)
	require.NoError(t, err)
	require.Len(t, resp.Blocks, 2)
	none := resp.Blocks[0].ToolUse
	require.Equal(t, `{}`, string(none.Args))
	require.NoError(t, none.ArgsErr(), "null arguments are an empty call")
	ok := resp.Blocks[1].ToolUse
	require.Equal(t, `{"text":"ok"}`, string(ok.Args), "arguments are compacted")
	require.Equal(t, "", ok.RawArgs)
}

// Real Ollama never sends malformed tool arguments: its server fails the
// request instead (llm/llama_server.go "llama-server returned invalid tool
// call arguments", v0.40.14), as an HTTP 500 before any output or as an
// {"error":...} line after. Either way the whole turn is an error, never a
// tool call to answer, and it is not retried (500 is not overload).
func TestOllamaChat_InvalidToolArgumentsFailTheTurn(t *testing.T) {
	const msg = `llama-server returned invalid tool call arguments for "knomit_query": invalid character 'x' looking for beginning of value`
	t.Run("error line after content", func(t *testing.T) {
		srv := newOllamaServer(t, []string{"tools"},
			`{"message":{"role":"assistant","content":"calling"},"done":false}`,
			`{"error":"`+strings.ReplaceAll(msg, `"`, `\"`)+`"}`)
		resp, err := newTestOllama(t, srv, OllamaOptions{}).Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.Nil(t, resp)
		require.ErrorContains(t, err, "invalid tool call arguments")
		require.False(t, isRetryable(err))
	})
	t.Run("http 500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/show" {
				_, _ = io.WriteString(w, `{"capabilities":["tools"]}`)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
		}))
		t.Cleanup(srv.Close)
		c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{Host: srv.URL, NumCtx: 32768})
		require.NoError(t, err)
		resp, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.Nil(t, resp)
		require.ErrorContains(t, err, "ollama: HTTP 500")
		require.ErrorContains(t, err, "invalid tool call arguments")
		require.False(t, isRetryable(err))
	})
}

// With truncate:false an over-long prompt is refused, not cut. The text is
// what a real Ollama v0.40.14 forwards from its llama.cpp b11963 runner: the
// llama-server HTTP 400 (tools/server/server-context.cpp:4080-4086, status
// from server-common.cpp:125-128), sent as {"error":"..."}. Ollama's own
// promptTooLongMessage is reachable only with truncate:true and is NOT what
// this path sees, so it is deliberately not the fixture.
func TestOllamaChat_PromptOverContextIsErrContextFull(t *testing.T) {
	const tooLong = "request (40000 tokens) exceeds the available context size (32768 tokens), try increasing it"
	t.Run("http 400", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/show" {
				_, _ = io.WriteString(w, `{"capabilities":["tools"]}`)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": tooLong})
		}))
		t.Cleanup(srv.Close)
		c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{Host: srv.URL, NumCtx: 32768})
		require.NoError(t, err)
		_, err = c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.ErrorIs(t, err, ErrContextFull)
		require.ErrorContains(t, err, "ollama: HTTP 400")
		require.ErrorContains(t, err, "exceeds the available context size")
		require.False(t, isRetryable(err), "the same prompt fails the same way")
	})
	t.Run("error line", func(t *testing.T) {
		srv := newOllamaServer(t, []string{"tools"}, `{"error":"`+tooLong+`"}`)
		_, err := newTestOllama(t, srv, OllamaOptions{}).Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.ErrorIs(t, err, ErrContextFull)
	})
	t.Run("other 400 is not context-full", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/show" {
				_, _ = io.WriteString(w, `{"capabilities":["tools"]}`)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": `"m" does not support thinking`})
		}))
		t.Cleanup(srv.Close)
		c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{Host: srv.URL, NumCtx: 32768})
		require.NoError(t, err)
		_, err = c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrContextFull)
	})
}

// Two calls to the SAME tool in one turn are told apart by id on the wire.
func TestOllamaChat_ParallelSameNameCallsKeepTheirIDs(t *testing.T) {
	roundTrip := func(t *testing.T, line string, ids ...string) (resp *Response, sent []ollamaMsg) {
		srv := newOllamaServer(t, []string{"tools"}, line,
			`{"message":{"role":"assistant","content":""},"done_reason":"stop","done":true}`)
		c := newTestOllama(t, srv, OllamaOptions{})
		n := 0
		resp, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{NewID: func() string { n++; return ids[n-1] }}, nil)
		require.NoError(t, err)
		require.Len(t, resp.Blocks, 2)

		// Answer the calls in the REVERSE order, then send the conversation back.
		b := NewBuilder()
		require.NoError(t, b.Add(Item{ID: "a", Hook: HookConversation, Entry: &Entry{ID: "a", Role: RoleAssistant, Blocks: resp.Blocks}}))
		require.NoError(t, b.Add(Item{ID: "r", Hook: HookConversation, Entry: &Entry{ID: "r", Role: RoleUser, Blocks: []Block{
			{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: resp.Blocks[1].ToolUse.ID, Content: "SECOND"}},
			{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: resp.Blocks[0].ToolUse.ID, Content: "FIRST"}},
		}}}))
		_, err = c.Stream(context.Background(), b.Snapshot(), StreamOptions{}, nil)
		require.NoError(t, err)
		var body struct {
			Messages []ollamaMsg `json:"messages"`
		}
		raw, _ := json.Marshal(srv.received()[1])
		require.NoError(t, json.Unmarshal(raw, &body))
		return resp, body.Messages
	}

	t.Run("server sends ids", func(t *testing.T) {
		resp, msgs := roundTrip(t, `{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_aaa11111","function":{"name":"knomit_query","arguments":{"text":"1"}}},{"id":"call_bbb22222","function":{"name":"knomit_query","arguments":{"text":"2"}}}]},"done":false}`, "tu_a", "tu_b")
		require.Equal(t, "tu_a", resp.Blocks[0].ToolUse.ID, "ToolUse.ID stays knomit's")
		require.Equal(t, "tu_b", resp.Blocks[1].ToolUse.ID)
		require.Len(t, msgs, 3)
		require.Equal(t, "tu_a", msgs[0].ToolCalls[0].ID, "knomit's id is sent, not the server's call_aaa11111")
		require.Equal(t, "tu_b", msgs[0].ToolCalls[1].ID)
		require.Equal(t, ollamaMsg{Role: "tool", Content: "SECOND", ToolName: "knomit_query", ToolCallID: "tu_b"}, msgs[1])
		require.Equal(t, ollamaMsg{Role: "tool", Content: "FIRST", ToolName: "knomit_query", ToolCallID: "tu_a"}, msgs[2])
	})
	t.Run("server sends no ids", func(t *testing.T) {
		resp, msgs := roundTrip(t, `{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"knomit_query","arguments":{"text":"1"}}},{"function":{"name":"knomit_query","arguments":{"text":"2"}}}]},"done":false}`, "tu_a", "tu_b")
		require.Equal(t, "tu_a", resp.Blocks[0].ToolUse.ID)
		require.Len(t, msgs, 3)
		require.Equal(t, "tu_a", msgs[0].ToolCalls[0].ID, "knomit's id is sent either way")
		require.Equal(t, "tu_b", msgs[0].ToolCalls[1].ID)
		require.Equal(t, ollamaMsg{Role: "tool", Content: "SECOND", ToolName: "knomit_query", ToolCallID: "tu_b"}, msgs[1])
		require.Equal(t, ollamaMsg{Role: "tool", Content: "FIRST", ToolName: "knomit_query", ToolCallID: "tu_a"}, msgs[2])
	})
}

func TestOllamaChat_StreamFailures(t *testing.T) {
	t.Run("error line mid-stream", func(t *testing.T) {
		srv := newOllamaServer(t, []string{"tools"},
			`{"message":{"role":"assistant","content":"hi"},"done":false}`,
			`{"error":"an error was encountered while running the model"}`)
		c := newTestOllama(t, srv, OllamaOptions{})
		_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.ErrorContains(t, err, "an error was encountered while running the model")
	})
	t.Run("no done line", func(t *testing.T) {
		srv := newOllamaServer(t, []string{"tools"}, `{"message":{"role":"assistant","content":"hi"},"done":false}`)
		c := newTestOllama(t, srv, OllamaOptions{})
		_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.ErrorContains(t, err, "before the done line")
	})
	t.Run("garbage line", func(t *testing.T) {
		srv := newOllamaServer(t, []string{"tools"}, `this is not json`)
		c := newTestOllama(t, srv, OllamaOptions{})
		_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.ErrorContains(t, err, "read stream")
	})
	t.Run("overloaded status is retryable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/show" {
				_, _ = io.WriteString(w, `{"capabilities":["tools"]}`)
				return
			}
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)
		c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{Host: srv.URL, NumCtx: 32768})
		require.NoError(t, err)
		_, err = c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.ErrorContains(t, err, "ollama: HTTP 503: busy")
		require.True(t, isRetryable(err), "the classifier must see this as overload")
	})
	t.Run("tool result with no call", func(t *testing.T) {
		b := NewBuilder()
		require.NoError(t, b.Add(Item{ID: "c1", Hook: HookConversation, Entry: &Entry{ID: "c1", Role: RoleUser, Blocks: []Block{
			{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "tu_9", Content: "orphan"}}}}}))
		srv := newOllamaServer(t, []string{"tools"})
		c := newTestOllama(t, srv, OllamaOptions{})
		_, err := c.Stream(context.Background(), b.Snapshot(), StreamOptions{}, nil)
		require.ErrorContains(t, err, `"tu_9" answers no tool call`)
		require.Zero(t, srv.chats)
	})
}

// Every stream line is progress, including thinking-only ones that reach no
// sink; WrapChat's stall timer must not cut such a stream off.
func TestOllamaChat_ThinkingLinesAreProgress(t *testing.T) {
	chats := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			_, _ = io.WriteString(w, `{"capabilities":["tools","thinking"]}`)
			return
		}
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r) // e.g. the one /api/ps spill check after the first request
			return
		}
		chats++
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"","thinking":"hmm"},"done":false}`+"\n")
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"done thinking"},"done_reason":"stop","done":true}`+"\n")
	}))
	t.Cleanup(srv.Close)
	c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{Host: srv.URL, NumCtx: 32768})
	require.NoError(t, err)

	progress := 0
	resp, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{Progress: func() { progress++ }}, nil)
	require.NoError(t, err)
	require.Equal(t, 2, progress, "one Progress call per stream line")
	require.Equal(t, "done thinking", resp.Blocks[1].Text)

	wrapped := WrapChat(c, Policy{AttemptTimeout: 100 * time.Millisecond})
	resp, err = wrapped.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
	require.NoError(t, err, "a 300ms silence after a thinking line is not a stall under a 100ms first-activity timeout")
	require.Equal(t, "done thinking", resp.Blocks[1].Text)
	require.Equal(t, 2, chats, "the wrapped call was not retried")
}

func TestOllamaChat_UsageFromCachedCount(t *testing.T) {
	run := func(counts string) Usage {
		srv := newOllamaServer(t, []string{"tools"},
			`{"message":{"role":"assistant","content":"ok"},"done_reason":"stop","done":true,`+counts+`,"eval_count":3}`)
		resp, err := newTestOllama(t, srv, OllamaOptions{}).Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.NoError(t, err)
		return resp.Usage
	}
	// prompt_eval_count is the WHOLE prompt; the cached part is not Input.
	require.Equal(t, Usage{Input: 200, PromptEval: 900, Output: 3, CacheRead: 700},
		run(`"prompt_eval_count":900,"prompt_eval_cached_count":700`))
	// A reported 0 is a real "nothing cached", not the fallback.
	require.Equal(t, Usage{Input: 900, PromptEval: 900, Output: 3, CacheRead: 0},
		run(`"prompt_eval_count":900,"prompt_eval_cached_count":0`))
	// A cached count above the total never makes Input negative.
	require.Equal(t, Usage{Input: 0, PromptEval: 900, Output: 3, CacheRead: 950},
		run(`"prompt_eval_count":900,"prompt_eval_cached_count":950`))
}

// A server with no prompt_eval_cached_count (Ollama before v0.35.0) gets the
// bytes/4 estimate, flagged on the estimated field only.
func TestOllamaChat_UsageFallbackEstimate(t *testing.T) {
	s := ollamaSnapshot(t, "x")
	est := 0
	for _, n := range s.Record().Tokens {
		est += n
	}
	require.Greater(t, est, 50, "the fixture must estimate more tokens than the evaluated count below")

	run := func(evaluated string) Usage {
		srv := newOllamaServer(t, []string{"tools"},
			`{"message":{"role":"assistant","content":"ok"},"done_reason":"stop","done":true,"prompt_eval_count":`+evaluated+`,"eval_count":3}`)
		resp, err := newTestOllama(t, srv, OllamaOptions{}).Stream(context.Background(), s, StreamOptions{}, nil)
		require.NoError(t, err)
		return resp.Usage
	}
	require.Equal(t, Usage{Input: 20, PromptEval: 20, Output: 3, CacheRead: int64(est - 20), Estimated: true, EstimatedFields: []string{"cache_read"}}, run("20"))
	// More evaluated than estimated: the estimate never goes negative.
	require.Equal(t, Usage{Input: 100000, PromptEval: 100000, Output: 3, CacheRead: 0, Estimated: true, EstimatedFields: []string{"cache_read"}}, run("100000"))
}

func TestOllamaPrefixJSON_MatchesWire(t *testing.T) {
	srv := newOllamaServer(t, []string{"tools"}, `{"message":{"role":"assistant","content":"ok"},"done_reason":"stop","done":true}`)
	s := ollamaSnapshot(t, "x")
	_, err := newTestOllama(t, srv, OllamaOptions{}).Stream(context.Background(), s, StreamOptions{}, nil)
	require.NoError(t, err)
	b := srv.received()[0]
	var msgs []json.RawMessage
	require.NoError(t, json.Unmarshal(b["messages"], &msgs))
	want := `{"system":` + string(msgs[0]) + `,"tools":` + string(b["tools"]) + `}`
	got, err := OllamaPrefixJSON(s, "test-model")
	require.NoError(t, err)
	require.Equal(t, want, string(got))
	require.Contains(t, string(got), "SYSTEM PROMPT")
	require.Contains(t, string(got), "knomit_query")

	empty, err := OllamaPrefixJSON(NewBuilder().Snapshot(), "m")
	require.NoError(t, err)
	require.Equal(t, `{"system":null,"tools":null}`, string(empty), "no system text and no tools: both null, not missing")
}
