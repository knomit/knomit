package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// ollamaV2Server answers /api/show with caps and every /api/chat with the
// given NDJSON lines (each followed by a newline), recording each chat body
// and counting chat calls.
type ollamaV2Server struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]json.RawMessage
	chats  int
}

func newOllamaV2Server(t *testing.T, caps []string, lines ...string) *ollamaV2Server {
	t.Helper()
	s := &ollamaV2Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": caps})
		case "/api/chat":
			b, _ := io.ReadAll(r.Body)
			var m map[string]json.RawMessage
			_ = json.Unmarshal(b, &m)
			s.mu.Lock()
			s.bodies = append(s.bodies, m)
			s.chats++
			s.mu.Unlock()
			for _, l := range lines {
				_, _ = io.WriteString(w, l+"\n")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *ollamaV2Server) received() []map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), s.bodies...)
}

func newTestOllama(t *testing.T, srv *ollamaV2Server, o OllamaOptions) *OllamaChat {
	t.Helper()
	o.Host = srv.URL
	c, err := NewOllamaChat(context.Background(), "test-model", o)
	require.NoError(t, err)
	return c
}

// ollamaSnapshot is a snapshot whose hook 6 holds one tool round trip.
func ollamaSnapshot(t *testing.T, turn string) Snapshot {
	t.Helper()
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "tool:knomit_repos", Hook: HookTools, Tool: &ToolDef{Name: "knomit_repos", Description: "List repos.", Schema: json.RawMessage(`{"type":"object"}`)}}))
	require.NoError(t, b.Add(Item{ID: "tool:knomit_query", Hook: HookTools, Tool: &ToolDef{Name: "knomit_query", Description: "Search.", Schema: json.RawMessage(`{"properties":{"text":{"type":"string"}},"type":"object"}`)}}))
	require.NoError(t, b.Add(Item{ID: "system", Hook: HookSystem, Text: "SYSTEM PROMPT"}))
	require.NoError(t, b.Add(Item{ID: "instructions", Hook: HookRepo, Text: "INSTRUCTIONS"}))
	require.NoError(t, b.Add(Item{ID: "ws:1", Hook: HookWorkingSet, Text: "WORKING FACT"}))
	require.NoError(t, b.Add(Item{ID: "c1", Hook: HookConversation, Entry: &Entry{ID: "c1", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "list repos"}}}}))
	require.NoError(t, b.Add(Item{ID: "c2", Hook: HookConversation, Entry: &Entry{ID: "c2", Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockText, Text: "Looking."},
		{Kind: BlockToolUse, ToolUse: &ToolUse{ID: "tu_1", Name: "knomit_repos", Args: json.RawMessage(`{"a":1}`)}},
	}}}))
	require.NoError(t, b.Add(Item{ID: "c3", Hook: HookConversation, Entry: &Entry{ID: "c3", Role: RoleUser, Blocks: []Block{
		{Kind: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "tu_1", Content: "repo-list-result"}},
	}}}))
	require.NoError(t, b.Add(Item{ID: "turn", Hook: HookTurn, Entry: &Entry{ID: "turn", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: turn}}}}))
	return b.Snapshot()
}

func TestRenderOllama_ToolResultCarriesToolName(t *testing.T) {
	req := renderOllama(ollamaSnapshot(t, "next"), "m", OllamaOptions{})
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	require.Contains(t, string(raw), `{"role":"tool","content":"repo-list-result","tool_name":"knomit_repos"}`)
	require.NotContains(t, string(raw), "tu_1")

	// The whole layout, message by message: nothing else is sent.
	var roles []string
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	require.Equal(t, []string{"system", "user", "assistant", "tool", "user"}, roles)
	require.Equal(t, "SYSTEM PROMPT\n\nINSTRUCTIONS\n\nWORKING FACT", req.Messages[0].Content)
	require.Equal(t, "Looking.", req.Messages[2].Content)
	require.Equal(t, `{"a":1}`, string(req.Messages[2].ToolCalls[0].Function.Arguments))
	require.Equal(t, "knomit_repos", req.Messages[2].ToolCalls[0].Function.Name)
	require.Equal(t, "next", req.Messages[4].Content)
	// Tools are in name order whatever the insertion order was.
	require.Equal(t, "knomit_query", req.Tools[0].Function.Name)
	require.Equal(t, "knomit_repos", req.Tools[1].Function.Name)
	require.Equal(t, "function", req.Tools[0].Type)
}

func TestRenderOllama_PrefixStableWhenTurnChanges(t *testing.T) {
	a := renderOllama(ollamaSnapshot(t, "turn one"), "m", OllamaOptions{})
	b := renderOllama(ollamaSnapshot(t, "a different turn"), "m", OllamaOptions{})
	for _, f := range []func(r ollamaChatRequestV2) any{
		func(r ollamaChatRequestV2) any { return r.Messages[0] },
		func(r ollamaChatRequestV2) any { return r.Tools },
	} {
		ja, _ := json.Marshal(f(a))
		jb, _ := json.Marshal(f(b))
		require.NotEmpty(t, ja)
		require.Equal(t, string(ja), string(jb))
	}
	la, _ := json.Marshal(a.Messages[len(a.Messages)-1])
	lb, _ := json.Marshal(b.Messages[len(b.Messages)-1])
	require.NotEqual(t, string(la), string(lb))
	require.Contains(t, string(la), "turn one")
	require.Contains(t, string(lb), "a different turn")

	// Everything before the newest turn is byte-equal too (the KV-cache prefix).
	ma, _ := json.Marshal(a.Messages[:len(a.Messages)-1])
	mb, _ := json.Marshal(b.Messages[:len(b.Messages)-1])
	require.Equal(t, string(ma), string(mb))
}

func TestOllamaChat_StreamToolCallGetsKnomitID(t *testing.T) {
	srv := newOllamaV2Server(t, []string{"completion", "tools"},
		`{"model":"test-model","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"knomit_query","arguments":{"text":"x"}}}]},"done":false}`,
		`{"model":"test-model","message":{"role":"assistant","content":""},"done_reason":"stop","done":true,"prompt_eval_count":120,"eval_count":7}`,
	)
	c := newTestOllama(t, srv, OllamaOptions{})
	var events []Event
	resp, err := c.Stream(context.Background(), ollamaSnapshot(t, "go"), StreamOptions{NewID: func() string { return "tu_42" }}, func(e Event) { events = append(events, e) })
	require.NoError(t, err)

	require.Len(t, events, 2)
	require.Equal(t, EventToolUse, events[0].Kind)
	require.Equal(t, "tu_42", events[0].ToolUse.ID)
	require.Equal(t, "knomit_query", events[0].ToolUse.Name)
	require.JSONEq(t, `{"text":"x"}`, string(events[0].ToolUse.Args))
	require.NoError(t, events[0].ToolUse.ArgsErr())
	require.Equal(t, EventDone, events[1].Kind)

	require.Equal(t, StopToolUse, resp.StopReason, "Ollama says done_reason stop; the calls make it a tool_use stop")
	require.Equal(t, []Block{{Kind: BlockToolUse, ToolUse: events[0].ToolUse}}, resp.Blocks)
	want := resp.Usage
	require.Equal(t, int64(120), want.PromptEval)
	require.Equal(t, int64(120), want.Input)
	require.Equal(t, int64(7), want.Output)
	require.True(t, want.Estimated)

	got := srv.received()
	require.Len(t, got, 1)
	b := got[0]
	// Every field of the request body, none left unasserted.
	require.Len(t, b, 6)
	require.Equal(t, `"test-model"`, string(b["model"]))
	require.Equal(t, `true`, string(b["stream"]))
	require.Equal(t, `"30m"`, string(b["keep_alive"]))
	require.Equal(t, `{"num_ctx":32768,"num_predict":-1}`, string(b["options"]))
	require.Contains(t, b, "messages")
	require.Contains(t, b, "tools")
	require.NotContains(t, b, "think", "a model without the thinking capability is never sent think")
}
