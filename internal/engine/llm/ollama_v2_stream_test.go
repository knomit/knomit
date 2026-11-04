package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOllamaChat_CapsFromShow(t *testing.T) {
	t.Run("tools", func(t *testing.T) {
		c := newTestOllama(t, newOllamaV2Server(t, []string{"completion", "tools"}), OllamaOptions{NumCtx: 8192, MaxOutput: 512})
		require.True(t, c.Caps().Tools)
		require.Equal(t, "", c.UnusableReason())
		require.Equal(t, Caps{Tools: true, MaxBreakpoints: 0, ContextWindow: 8192, MaxOutput: 512}, c.Caps())
	})
	t.Run("no tools", func(t *testing.T) {
		srv := newOllamaV2Server(t, []string{"completion"})
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
		c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{Host: srv.URL})
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
		srv := newOllamaV2Server(t, []string{"tools"})
		t.Setenv("OLLAMA_HOST", strings.TrimPrefix(srv.URL, "http://"))
		c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{})
		require.NoError(t, err)
		require.True(t, c.Caps().Tools)
	})
}

func TestOllamaChat_LengthStopIsAnError(t *testing.T) {
	srv := newOllamaV2Server(t, []string{"tools"},
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

func TestOllamaChat_AlwaysSendsNumCtxAndKeepAlive(t *testing.T) {
	done := `{"message":{"role":"assistant","content":"ok"},"done_reason":"stop","done":true}`
	for _, tc := range []struct {
		name      string
		o         OllamaOptions
		stream    StreamOptions
		ctx, pred int
		keepAlive string
	}{
		{"defaults", OllamaOptions{}, StreamOptions{}, 32768, -1, `"30m"`},
		{"configured", OllamaOptions{NumCtx: 4096, KeepAlive: "2h", MaxOutput: 300}, StreamOptions{}, 4096, 300, `"2h"`},
		{"per-request output", OllamaOptions{NumCtx: 4096, MaxOutput: 300}, StreamOptions{MaxOutput: 77}, 4096, 77, `"30m"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOllamaV2Server(t, []string{"tools"}, done)
			c := newTestOllama(t, srv, tc.o)
			_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), tc.stream, nil)
			require.NoError(t, err)
			b := srv.received()[0]
			var opts map[string]int
			require.NoError(t, json.Unmarshal(b["options"], &opts))
			require.Equal(t, map[string]int{"num_ctx": tc.ctx, "num_predict": tc.pred}, opts)
			require.Equal(t, tc.keepAlive, string(b["keep_alive"]))
		})
	}
}

func TestOllamaChat_ThinkingModelStreamsReasoningFirst(t *testing.T) {
	srv := newOllamaV2Server(t, []string{"tools", "thinking"},
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

func TestOllamaChat_InvalidToolArgsKeptRaw(t *testing.T) {
	srv := newOllamaV2Server(t, []string{"tools"},
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"knomit_query","arguments":"not an object"}},{"function":{"name":"knomit_repos","arguments":null}},{"function":{"name":"knomit_query","arguments":{ "text" : "ok" }}}]},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done_reason":"stop","done":true}`,
	)
	c := newTestOllama(t, srv, OllamaOptions{})
	ids := []string{"tu_a", "tu_b", "tu_c"}
	n := 0
	resp, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{NewID: func() string { n++; return ids[n-1] }}, nil)
	require.NoError(t, err)
	require.Len(t, resp.Blocks, 3)

	bad := resp.Blocks[0].ToolUse
	require.Equal(t, "tu_a", bad.ID)
	require.Equal(t, `{}`, string(bad.Args), "invalid arguments are stored as {}")
	require.Equal(t, `"not an object"`, bad.RawArgs)
	var ia *InvalidToolArgsError
	require.True(t, errors.As(bad.ArgsErr(), &ia))
	require.ErrorIs(t, bad.ArgsErr(), ErrInvalidToolArgs)
	require.Equal(t, "knomit_query", ia.Name)

	none := resp.Blocks[1].ToolUse
	require.Equal(t, `{}`, string(none.Args))
	require.NoError(t, none.ArgsErr(), "null arguments are an empty call, not an error")

	ok := resp.Blocks[2].ToolUse
	require.Equal(t, `{"text":"ok"}`, string(ok.Args), "arguments are compacted")
	require.NoError(t, ok.ArgsErr())

	_, err = json.Marshal(Entry{Role: RoleAssistant, Blocks: resp.Blocks})
	require.NoError(t, err, "the entry still marshals with the {} stored for the bad call")
}

func TestOllamaChat_StreamFailures(t *testing.T) {
	t.Run("error line mid-stream", func(t *testing.T) {
		srv := newOllamaV2Server(t, []string{"tools"},
			`{"message":{"role":"assistant","content":"hi"},"done":false}`,
			`{"error":"an error was encountered while running the model"}`)
		c := newTestOllama(t, srv, OllamaOptions{})
		_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.ErrorContains(t, err, "an error was encountered while running the model")
	})
	t.Run("no done line", func(t *testing.T) {
		srv := newOllamaV2Server(t, []string{"tools"}, `{"message":{"role":"assistant","content":"hi"},"done":false}`)
		c := newTestOllama(t, srv, OllamaOptions{})
		_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.ErrorContains(t, err, "before the done line")
	})
	t.Run("garbage line", func(t *testing.T) {
		srv := newOllamaV2Server(t, []string{"tools"}, `this is not json`)
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
		c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{Host: srv.URL})
		require.NoError(t, err)
		_, err = c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.ErrorContains(t, err, "ollama: HTTP 503: busy")
		require.True(t, isRetryable(err), "the v1 classifier must see this as overload")
	})
	t.Run("tool result with no call", func(t *testing.T) {
		b := NewBuilder()
		require.NoError(t, b.Add(Item{ID: "c1", Hook: HookConversation, Entry: &Entry{ID: "c1", Role: RoleUser, Blocks: []Block{
			{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "tu_9", Content: "orphan"}}}}}))
		srv := newOllamaV2Server(t, []string{"tools"})
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
		chats++
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"","thinking":"hmm"},"done":false}`+"\n")
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"done thinking"},"done_reason":"stop","done":true}`+"\n")
	}))
	t.Cleanup(srv.Close)
	c, err := NewOllamaChat(context.Background(), "m", OllamaOptions{Host: srv.URL})
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

func TestOllamaChat_UsageCacheEstimate(t *testing.T) {
	s := ollamaSnapshot(t, "x")
	est := 0
	for _, n := range s.Record().Tokens {
		est += n
	}
	require.Greater(t, est, 50, "the fixture must estimate more tokens than the evaluated count below")

	run := func(evaluated string) Usage {
		srv := newOllamaV2Server(t, []string{"tools"},
			`{"message":{"role":"assistant","content":"ok"},"done_reason":"stop","done":true,"prompt_eval_count":`+evaluated+`,"eval_count":3}`)
		resp, err := newTestOllama(t, srv, OllamaOptions{}).Stream(context.Background(), s, StreamOptions{}, nil)
		require.NoError(t, err)
		return resp.Usage
	}
	require.Equal(t, Usage{Input: 20, PromptEval: 20, Output: 3, CacheRead: int64(est - 20), Estimated: true}, run("20"))
	// More evaluated than estimated: the estimate never goes negative.
	require.Equal(t, Usage{Input: 100000, PromptEval: 100000, Output: 3, CacheRead: 0, Estimated: true}, run("100000"))
}

func TestOllamaPrefixJSON_MatchesWire(t *testing.T) {
	srv := newOllamaV2Server(t, []string{"tools"}, `{"message":{"role":"assistant","content":"ok"},"done_reason":"stop","done":true}`)
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
