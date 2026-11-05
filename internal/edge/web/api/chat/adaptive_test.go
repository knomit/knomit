package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/domain/modelparams"
	"knomit/internal/edge/mcp"
	"knomit/internal/engine/llm"
	"knomit/internal/foundation/config"
)

// ---- fixtures ----

// useOllama swaps the harness registry for one that also knows an Ollama
// provider (adaptive "auto") and one with adaptive_tools = "off", and opens the
// scripted models without asking a real registry. Registry.Adaptive is what the
// chat consults, so the tests drive the real decision.
func (h *harness) useOllama() {
	h.t.Helper()
	h.mgr.reg = llm.NewRegistry(map[string]config.LLMProviderConfig{
		"anthropic":  {Kind: "anthropic", APIKey: plainKey, Models: []string{"a1"}},
		"ollama":     {Kind: "ollama", Models: []string{"o1"}},
		"ollama-off": {Kind: "ollama", AdaptiveTools: config.AdaptiveToolsOff, Models: []string{"o1"}},
	})
	h.mgr.openModel = func(_ context.Context, _, model string) (llm.ChatModel, error) {
		return h.nextModel(model), nil
	}
}

// queueAdaptive queues an Ollama-like scripted model (no stable prefix) with
// the given context window.
func (h *harness) queueAdaptive(model string, window int, steps ...step) *scripted {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := &scripted{name: model, adaptive: true, window: window, steps: steps}
	h.models = append(h.models, m)
	return m
}

// createOn starts a chat on provider and returns the status and body.
func (h *harness) createOn(who, provider, model string) (int, []byte) {
	h.t.Helper()
	return h.do(who, http.MethodPost, "/chats", map[string]string{"provider": provider, "model": model})
}

func (h *harness) createAdaptive(who string) string {
	h.t.Helper()
	code, body := h.createOn(who, "ollama", "o1")
	require.Equal(h.t, http.StatusCreated, code, string(body))
	var v struct {
		ID string `json:"id"`
	}
	require.NoError(h.t, json.Unmarshal(body, &v))
	return v.ID
}

// fakeEmb is a deterministic, goroutine-safe embedder: a tool's document vector
// is one-hot at its place in names, and the query vector is the one given. The
// cache of tool vectors is process-wide and keyed by ID, so ID is the test's.
type fakeEmb struct {
	id    string
	names []string
	query []float32
	err   error // EmbedQuery fails with it when set

	mu      sync.Mutex
	docs    []string
	queries []string
}

func newFakeEmb(t *testing.T, names []string, target string) *fakeEmb {
	e := &fakeEmb{id: t.Name(), names: names, query: e1hot(names, target)}
	return e
}

func e1hot(names []string, target string) []float32 {
	v := make([]float32, len(names))
	for i, n := range names {
		if n == target {
			v[i] = 1
		}
	}
	return v
}

func (e *fakeEmb) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queries = append(e.queries, text)
	if e.err != nil {
		return nil, e.err
	}
	return e.query, nil
}

func (e *fakeEmb) EmbedDocument(_ context.Context, title, _ string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.docs = append(e.docs, title)
	return e1hot(e.names, title), nil
}
func (e *fakeEmb) Dim() int   { return len(e.names) }
func (e *fakeEmb) ID() string { return e.id }
func (e *fakeEmb) Thresholds() modelparams.Thresholds {
	panic("Thresholds consulted: tool ranking must not use a cosine cutoff")
}
func (e *fakeEmb) counts() (docs, queries int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.docs), len(e.queries)
}

func recordsOf(lines []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["type"] == typ {
			out = append(out, l)
		}
	}
	return out
}

func strs(v any) []string {
	out := []string{}
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func loadStep(id string, u llm.Usage, names ...string) step {
	return toolStep(id, llm.LoadToolsName, map[string]any{"names": names}, u)
}

// A message that names nothing: no tool scores above the others, so the guess
// preloads nothing and a test controls the loaded set itself.
const blankMessage = "zzzz"

func toolNamesIn(s llm.Snapshot, h llm.Hook) []string {
	var out []string
	for _, it := range s.Hooks[h] {
		if it.Tool != nil {
			out = append(out, it.Tool.Name)
		}
	}
	return out
}

// toolTokens is the estimate of one tool's definition, the way the window sizes
// it (bytes/4 of the snapshot's record).
func toolTokens(t *testing.T, s llm.Snapshot, name string) int {
	t.Helper()
	for _, it := range s.Hooks[llm.HookTools] {
		if it.Tool != nil && it.Tool.Name == name {
			b := llm.NewBuilder()
			require.NoError(t, b.Add(it))
			return b.Snapshot().Record().Tokens[llm.HookTools]
		}
	}
	t.Fatalf("no tool %s in the request", name)
	return 0
}

// ---- wiring: who gets a window ----

// The adaptive decision is Registry.Adaptive's alone: an Ollama chat gets a
// window and sends the loader, an Anthropic chat gets none and sends every tool
// and the unchanged system prompt, and adaptive_tools = "off" gives an Ollama
// chat none.
//
// SABOTAGE: mount.go create, set Window for every chat -> the Anthropic
// assertions fail; skip Registry.Adaptive and test the kind -> the "off" one
// fails.
func TestMount_AdaptiveForOllamaNotForAnthropic(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	all := h.toolNames(writer, "chat-00000000000000000000000000000a01")

	h.queueAdaptive("o1", 0, textStep("hi", usage))
	ol := h.createAdaptive("uid:501")
	co := h.mgr.find(ol, writer.String())
	require.NotNil(t, co.loop.Window, "an Ollama chat is adaptive")

	h.queue("a1", textStep("hi", usage))
	code, body := h.createOn("uid:501", "anthropic", "a1")
	require.Equal(t, http.StatusCreated, code, string(body))
	var an struct{ ID string }
	require.NoError(t, json.Unmarshal(body, &an))
	ca := h.mgr.find(an.ID, writer.String())
	require.Nil(t, ca.loop.Window, "an Anthropic chat is static")

	h.queueAdaptive("o1", 0, textStep("hi", usage))
	code, body = h.createOn("uid:501", "ollama-off", "o1")
	require.Equal(t, http.StatusCreated, code, string(body))
	var off struct{ ID string }
	require.NoError(t, json.Unmarshal(body, &off))
	require.Nil(t, h.mgr.find(off.ID, writer.String()).loop.Window, `adaptive_tools = "off" is the static path`)

	// What each model was sent.
	for _, id := range []string{ol, an.ID} {
		s := h.attach("uid:501", id)
		require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))
		s.until("turn_done")
	}
	adaptiveReq := co.loop.Model.(*scripted).requests()[0]
	require.Equal(t, []string{llm.LoadToolsName}, toolNamesIn(adaptiveReq, llm.HookTools), "the loader, no schemas")
	staticReq := ca.loop.Model.(*scripted).requests()[0]
	require.Equal(t, all, toolNamesIn(staticReq, llm.HookTools), "every tool the principal is shown")
	require.Equal(t, mcp.ChatSystemPrompt, staticReq.Hooks[llm.HookSystem][0].Text)
	require.Equal(t, mcp.ChatSystemPrompt, adaptiveReq.Hooks[llm.HookSystem][0].Text, "hook 2 is the same bytes in both modes")
	require.Equal(t, staticReq.Hooks[llm.HookRepo], adaptiveReq.Hooks[llm.HookRepo], "the instructions are untouched")
}

// With no embedder the guess is lexical only; with one it is the embedding
// ranker; an embedder that fails at call time falls back to lexical and the log
// says so. The ranker kind is read off the session log's preload record.
//
// SABOTAGE: mount.go, build the embedding ranker even when emb is nil -> a nil
// pointer or no "lexical" record; or drop the fallback -> the third case has no
// preload.
func TestLexicalFallbackWhenNoEmbedder(t *testing.T) {
	names := func(h *harness) []string { return h.toolNames(writer, "chat-00000000000000000000000000000a02") }

	run := func(t *testing.T, emb *fakeEmb, msg string) (*harness, map[string]any) {
		h := newHarness(t)
		h.useOllama()
		if emb != nil {
			h.mgr.emb = emb
		}
		h.queueAdaptive("o1", 40000, textStep("ok", usage))
		id := h.createAdaptive("uid:501")
		s := h.attach("uid:501", id)
		require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, msg))
		s.until("turn_done")
		recs := recordsOf(readLog(t, h.logDir, id), "preload")
		require.Len(t, recs, 1)
		return h, recs[0]
	}

	t.Run("no embedder", func(t *testing.T) {
		_, rec := run(t, nil, "explain")
		require.Equal(t, "lexical", rec["ranker"])
		require.NotEmpty(t, rec["ranked"], "the lexical ranking is logged")
	})

	t.Run("embedder", func(t *testing.T) {
		h := newHarness(t)
		emb := newFakeEmb(t, names(h), "knomit_explain")
		h2, rec := run(t, emb, "something unrelated to any name")
		_ = h2
		require.Equal(t, "embedding", rec["ranker"])
		require.Equal(t, []any{"knomit_explain"}, rec["loaded"], "only the tool the query vector points at scored above the rest")
		picks := rec["picks"].([]any)
		require.Len(t, picks, 1)
		require.Equal(t, "knomit_explain", picks[0].(map[string]any)["name"])
		require.InDelta(t, 1.0, picks[0].(map[string]any)["score"], 1e-9)
		_, queries := emb.counts()
		require.Equal(t, 1, queries, "one EmbedQuery for the turn's message")
	})

	t.Run("embedder fails at call time", func(t *testing.T) {
		h := newHarness(t)
		emb := newFakeEmb(t, names(h), "knomit_explain")
		emb.err = errors.New("onnx session closed")
		_, rec := run(t, emb, "explain")
		require.Equal(t, "lexical", rec["ranker"], "the guess is made by the fallback")
		require.Contains(t, rec["embedding_error"], "onnx session closed")
	})
}

// The chat warms the embedding ranker's tool vectors when it opens, off the
// request path: the first turn embeds only its message.
//
// SABOTAGE: mount.go, drop the WarmToolVectors goroutine -> the first turn
// embeds every tool and the "no more documents" assertion fails.
func TestChat_OpenWarmsToolVectorsOffTheRequestPath(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	names := h.toolNames(writer, "chat-00000000000000000000000000000a03")
	emb := newFakeEmb(t, names, "knomit_explain")
	h.mgr.emb = emb
	h.queueAdaptive("o1", 40000, textStep("ok", usage))
	id := h.createAdaptive("uid:501")
	require.Eventually(t, func() bool { d, _ := emb.counts(); return d == len(names) }, 5*time.Second, 5*time.Millisecond,
		"every tool is embedded once, by the chat's open and not by a turn")

	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))
	s.until("turn_done")
	docs, queries := emb.counts()
	require.Equal(t, len(names), docs, "the first turn embedded no tool")
	require.Equal(t, 1, queries)
}

// An Anthropic chat opens without ever touching the embedder.
func TestChat_StaticChatNeverEmbeds(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	emb := newFakeEmb(t, []string{"x"}, "x")
	h.mgr.emb = emb
	h.queue("a1", textStep("ok", usage))
	code, body := h.createOn("uid:501", "anthropic", "a1")
	require.Equal(t, http.StatusCreated, code, string(body))
	var v struct{ ID string }
	require.NoError(t, json.Unmarshal(body, &v))
	s := h.attach("uid:501", v.ID)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", v.ID, "explain"))
	s.until("turn_done")
	docs, queries := emb.counts()
	require.Zero(t, docs)
	require.Zero(t, queries)
}

// A server tool named like the loader refuses to open the chat, with the
// approved sentence.
func TestMount_LoaderNameCollisionRefusesToOpen(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	h.srv.AddTool(mcpgo.NewTool(llm.LoadToolsName, mcpgo.WithDescription("A server tool that took the loader's name.")),
		func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText("x"), nil
		})
	require.Contains(t, h.toolNames(writer, "chat-00000000000000000000000000000a04"), llm.LoadToolsName, "fixture: the server offers it")

	h.queueAdaptive("o1", 0, textStep("hi", usage))
	code, body := h.createOn("uid:501", "ollama", "o1")
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Contains(t, string(body), fmt.Sprintf(llm.ReservedToolNameMessage, llm.LoadToolsName))

	// The static path does not care: the same server opens for Anthropic.
	h.queue("a1", textStep("hi", usage))
	code, body = h.createOn("uid:501", "anthropic", "a1")
	require.Equal(t, http.StatusCreated, code, string(body))
}

// A window too small for even the tool names refuses to open, with the
// adaptive opening-too-big sentence and its sizes.
//
// SABOTAGE: mount.go, ignore the CatalogueTooBigError -> the chat opens and the
// 422 assertion fails.
func TestMount_CatalogueThatCannotFitRefusesToOpen(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	names := h.toolNames(writer, "chat-00000000000000000000000000000a05")
	var lines []string
	for _, n := range names {
		lines = append(lines, "- "+n)
	}
	need := len(strings.Join(lines, "\n")) / 4

	const window = 1100 // the reserve is the model's MaxOutput, 1000: 100 tokens usable, P = 12
	require.Greater(t, need, llm.PreloadBudget(window-1000), "fixture: the names exceed P")
	h.queueAdaptive("o1", window, textStep("hi", usage))
	code, body := h.createOn("uid:501", "ollama", "o1")
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	want := fmt.Sprintf(llm.OpeningTooBigAdaptiveMessage, need, "o1", window, 1000, 100, llm.OllamaRaiseNumCtxHint)
	require.Contains(t, string(body), want)
}

// ---- events ----

type toolsEvent struct {
	Turn      int      `json:"turn"`
	Reason    string   `json:"reason"`
	Loaded    []string `json:"loaded"`
	Evicted   []string `json:"evicted"`
	LoadedNow []string `json:"loaded_now"`
	Total     int      `json:"total"`
	Tokens    int      `json:"tokens"`
}

// A scripted run that loads one tool and then, under pressure, evicts it yields
// SSE tools events with exactly those names and reasons, and turn_done carries
// the window's counts. The snapshot says the same.
//
// SABOTAGE: chat.go onLoopEvent, drop the LoopToolsChanged case -> no tools
// events and this fails.
func TestChat_ToolsEventIsEmittedOnLoadAndEvict(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	total := len(h.toolNames(writer, "chat-00000000000000000000000000000a06"))
	const window = 40000
	usable := window - 1000 // the scripted model's MaxOutput is the reserve
	m := h.queueAdaptive("o1", window,
		loadStep("tu_load", llm.Usage{Input: 50}, "knomit_query"),
		// The turn's last response reports a prompt one token over the usable
		// room, so the next turn's first estimate is over budget and the only
		// unpinned loaded tool is evicted before that request.
		textStep("ok", llm.Usage{Input: int64(usable + 1)}),
		textStep("again", llm.Usage{Input: 100}),
	)
	id := h.createAdaptive("uid:501")
	s := h.attach("uid:501", id)

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))
	first := s.until("turn_done")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))
	second := s.until("turn_done")

	var tools []toolsEvent
	for _, e := range append(first, second...) {
		if e.Name == "tools" {
			var te toolsEvent
			require.NoError(t, json.Unmarshal(e.Data, &te))
			tools = append(tools, te)
		}
	}
	tokens := toolTokens(t, m.requests()[1], "knomit_query")
	turnOf := func(evs []sseEvent) int {
		var d struct {
			Turn int `json:"turn"`
		}
		require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &d))
		return d.Turn
	}
	require.Equal(t, 0, turnOf(first))
	require.Greater(t, turnOf(second), 0)
	require.Equal(t, []toolsEvent{
		{Turn: turnOf(first), Reason: llm.ToolsReasonModel, Loaded: []string{"knomit_query"}, Evicted: []string{}, LoadedNow: []string{"knomit_query"}, Total: total, Tokens: tokens},
		{Turn: turnOf(second), Reason: llm.ToolsReasonPressure, Loaded: []string{}, Evicted: []string{"knomit_query"}, LoadedNow: []string{}, Total: total, Tokens: 0},
	}, tools)

	done := func(evs []sseEvent) turnTools {
		var d struct {
			Tools *turnTools `json:"tools"`
		}
		require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &d))
		require.NotNil(t, d.Tools)
		return *d.Tools
	}
	require.Equal(t, turnTools{Loaded: 1, Evicted: 0, LoadedNow: 1, Total: total, Tokens: tokens}, done(first))
	require.Equal(t, turnTools{Loaded: 0, Evicted: 1, LoadedNow: 0, Total: total, Tokens: 0}, done(second))

	// The snapshot is authoritative: each turn's user item carries the same.
	code, body := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code)
	var snap struct {
		Items []struct {
			Kind  string     `json:"kind"`
			Tools *turnTools `json:"tools"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(body, &snap))
	var userTools []turnTools
	for _, it := range snap.Items {
		if it.Kind == "user" {
			require.NotNil(t, it.Tools)
			userTools = append(userTools, *it.Tools)
		}
	}
	require.Equal(t, []turnTools{done(first), done(second)}, userTools)

	// The loader's row names what it loaded.
	var loaderDone struct {
		Loaded []string `json:"loaded"`
		Name   string   `json:"name"`
		State  string   `json:"state"`
	}
	for _, e := range first {
		if e.Name == "tool_call" {
			var tc struct {
				Name   string   `json:"name"`
				State  string   `json:"state"`
				Loaded []string `json:"loaded"`
			}
			require.NoError(t, json.Unmarshal(e.Data, &tc))
			if tc.State == "done" {
				loaderDone.Loaded, loaderDone.Name, loaderDone.State = tc.Loaded, tc.Name, tc.State
			}
		}
	}
	require.Equal(t, llm.LoadToolsName, loaderDone.Name)
	require.Equal(t, []string{"knomit_query"}, loaderDone.Loaded)
}

// An Anthropic chat's turn_done and snapshot carry no tools field at all.
func TestChat_StaticTurnDoneHasNoTools(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", textStep("hi", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "hello"))
	evs := s.until("turn_done")
	var d map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &d))
	require.NotContains(t, d, "tools")
	for _, e := range evs {
		require.NotEqual(t, "tools", e.Name)
	}
	_, body := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.NotContains(t, string(body), `"tools"`)
}

// ---- session log ----

// learnBig writes one fact whose body is longer than the log's inline cap and
// ends in a marker that lies past the cap.
func (h *harness) learnBig(session, title, marker string) {
	h.t.Helper()
	s, _, _, err := mcp.OpenInProcessSession(context.Background(), h.srv, h.repos, mcp.InProcessOptions{
		Principal: writer, SessionID: session, Endpoint: "/api/v1/chats/" + session,
		ClientName: "test-writer", ClientVersion: "x",
	})
	require.NoError(h.t, err)
	defer s.Close(context.Background())
	bound, err := s.CallTool(context.Background(), "knomit_bind", json.RawMessage(`{"repo":"alpha"}`))
	require.NoError(h.t, err)
	args := learnArgs(handleFromResult(bound.Content), title)
	args["facts"].([]any)[0].(map[string]any)["body"] = strings.Repeat("padding for the log cap. ", 380) + marker
	raw, err := json.Marshal(args)
	require.NoError(h.t, err)
	out, err := s.CallTool(context.Background(), "knomit_learn", raw)
	require.NoError(h.t, err)
	require.False(h.t, out.IsError, out.Content)
}

// The session log of an adaptive chat: a preload record per turn, a tools
// record per change, a request record whose projection counts the stubbed
// results, and turn records that still hold the whole result which the
// tool_call record cut at 8 KiB.
//
// SABOTAGE: sessionlog.go, write the cut result into the turn record -> the
// marker is missing from it and this fails; chat.go, skip Tools() -> no tools
// records.
func TestSessionLog_ToolsPreloadAndProjectionRecords(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	const marker = "END-OF-THE-BIG-BODY"
	h.learnBig("chat-00000000000000000000000000000a07", "A fact with a long body", marker)

	m := h.queueAdaptive("o1", 40000,
		loadStep("tu_load", usage, "knomit_bind", "knomit_query"),
		toolStep("tu_bind", "knomit_bind", map[string]any{"repo": "alpha"}, usage),
		step{fn: func(s llm.Snapshot) step {
			return toolStep("tu_query", "knomit_query", map[string]any{
				"binding": handleFromResult(lastToolResult(s)), "path": "kb/gotchas/chat", "include_body": true,
			}, usage)
		}},
		textStep("done", usage),
		textStep("and again", usage),
	)
	id := h.createAdaptive("uid:501")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))
	s.until("turn_done")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))
	s.until("turn_done")

	lines := readLog(t, h.logDir, id)

	// The query's full result is in the turn record and cut in the tool_call.
	var queryCall map[string]any
	for _, c := range recordsOf(lines, "tool_call") {
		if c["call_id"] == "tu_query" {
			queryCall = c
		}
	}
	require.NotNil(t, queryCall)
	require.Equal(t, true, queryCall["truncated"])
	require.Equal(t, 8192, len(queryCall["result"].(string)))
	require.Greater(t, queryCall["bytes"], 9000.0, "the result is longer than the inline cap")
	require.NotContains(t, queryCall["result"], marker)
	var turnWithMarker int
	for _, tr := range recordsOf(lines, "turn") {
		b, _ := json.Marshal(tr["entry"])
		if strings.Contains(string(b), marker) {
			turnWithMarker++
		}
	}
	require.Equal(t, 1, turnWithMarker, "exactly one turn record holds the whole result")

	// The projection: the second turn's request carries the first turn's long
	// results as stubs, and the record counts exactly those the model was sent.
	reqs := recordsOf(lines, "request")
	require.Len(t, reqs, 5)
	sent := m.requests()
	elided := func(s llm.Snapshot) int {
		n := 0
		for _, it := range s.Hooks[llm.HookConversation] {
			for _, b := range it.Entry.Blocks {
				if b.ToolResult != nil && b.ToolResult.Elided {
					n++
				}
			}
		}
		return n
	}
	for i, r := range reqs {
		p := r["projection"].(map[string]any)
		require.EqualValues(t, elided(sent[i]), p["stubbed_results"], "request %d", i)
	}
	require.EqualValues(t, 0, reqs[0]["projection"].(map[string]any)["stubbed_results"])
	last := reqs[4]["projection"].(map[string]any)
	require.GreaterOrEqual(t, last["stubbed_results"], 1.0, "the long query result of the ended turn is stubbed")
	require.Greater(t, last["saved_tokens_est"], 1500.0)

	// Tools: the model loaded two tools in one call.
	var toolRecs []map[string]any
	for _, r := range recordsOf(lines, "tools") {
		toolRecs = append(toolRecs, r)
	}
	require.NotEmpty(t, toolRecs)
	require.Equal(t, llm.ToolsReasonModel, toolRecs[0]["reason"])
	require.Equal(t, []string{"knomit_bind", "knomit_query"}, strs(toolRecs[0]["loaded"]))
	require.Equal(t, []string{"knomit_bind", "knomit_query"}, strs(toolRecs[0]["loaded_now"]))

	// A preload record per turn, even when the guess loaded nothing.
	pre := recordsOf(lines, "preload")
	require.Len(t, pre, 2)
	require.Equal(t, "lexical", pre[0]["ranker"])
	require.Empty(t, pre[0]["loaded"])
}

// The preload record names what was picked with its score, the budget, what
// was loaded, and at the turn's end which picks the model called.
//
// SABOTAGE: chat.go finishTurn, write the picks as the hits -> the second case
// (a pick the model did not call) fails.
func TestSessionLog_PreloadHitAtTurnEnd(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	names := h.toolNames(writer, "chat-00000000000000000000000000000a08")
	h.mgr.emb = newFakeEmb(t, names, "knomit_explain")

	h.queueAdaptive("o1", 40000,
		toolStep("tu_x", "knomit_explain", map[string]any{"path": "kb/none"}, usage), // calls the pick
		textStep("done", usage),
		textStep("no call", usage), // a second turn that calls nothing
	)
	id := h.createAdaptive("uid:501")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "one"))
	s.until("turn_done")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "two"))
	s.until("turn_done")

	pre := recordsOf(readLog(t, h.logDir, id), "preload")
	require.Len(t, pre, 2)
	budget := llm.PreloadBudget(40000 - 1000)
	first := pre[0]
	require.EqualValues(t, budget, first["budget"])
	require.Equal(t, []string{"knomit_explain"}, strs(first["loaded"]))
	require.Equal(t, []string{"knomit_explain"}, strs(first["hits"]), "the model called the pick")
	require.EqualValues(t, 1, first["picked"])
	require.EqualValues(t, 1, first["called"])

	// Turn two: the pick is already loaded, so nothing new is picked; and a pick
	// the model did not call is no hit.
	second := pre[1]
	require.Empty(t, second["loaded"])
	require.Empty(t, second["hits"])
	require.EqualValues(t, 0, second["picked"])
}

// The loader's call is logged with decision "auto", raises no approval event and
// is not in the pending list.
//
// SABOTAGE: have the loop send load_tools through Decide -> an approval event
// for it appears and this fails.
func TestSessionLog_LoaderCallHasAutoDecisionAndNoPendingApproval(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	h.queueAdaptive("o1", 40000,
		loadStep("tu_load", usage, "knomit_repos"),
		toolStep("tu_repos", "knomit_repos", map[string]any{}, usage),
		textStep("done", usage),
	)
	id := h.createAdaptive("uid:501")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))
	evs, asked := drive(t, h, "uid:501", id, s, nil)
	require.Empty(t, asked, "the loader and a read-only tool raise no approval")
	for _, e := range evs {
		require.NotEqual(t, "approval", e.Name)
	}

	var loaderApproval map[string]any
	for _, a := range recordsOf(readLog(t, h.logDir, id), "approval") {
		if a["tool"] == llm.LoadToolsName {
			loaderApproval = a
		}
	}
	require.NotNil(t, loaderApproval)
	require.Equal(t, "auto", loaderApproval["decision"])
	require.Equal(t, "tu_load", loaderApproval["call_id"])

	_, body := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	var snap struct {
		Pending []any `json:"pending_approvals"`
	}
	require.NoError(t, json.Unmarshal(body, &snap))
	require.Empty(t, snap.Pending)
	rec := callResult(t, h, "uid:501", id, "tu_load")
	require.Equal(t, llm.LoadToolsName, rec.Name)
	require.False(t, rec.IsError)
}

// A write tool the model loaded still raises its approval card before it runs;
// "allow for session" is granted on the unchanged key (pin, tool, action).
//
// SABOTAGE: have load_tools mark the tool approved (skip Decide for a loaded
// tool) -> the fact lands before the card and this fails.
func TestChat_ApprovalCardForALoadedWriteStillAppears(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	handle := h.bind(writer, "chat-00000000000000000000000000000a09", "alpha")
	h.queueAdaptive("o1", 40000,
		loadStep("tu_load", usage, "knomit_learn"),
		toolStep("tu_learn", "knomit_learn", learnArgs(handle, "Loaded then approved"), usage),
		textStep("written", usage),
	)
	id := h.createAdaptive("uid:501")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))

	var approval approvalEvent
	for {
		e := s.next("", 10*time.Second)
		require.NotEqual(t, "turn_done", e.Name, "the turn ended without asking")
		if e.Name == "approval" {
			require.NoError(t, json.Unmarshal(e.Data, &approval))
			break
		}
	}
	require.Equal(t, "knomit_learn", approval.Tool)
	require.Equal(t, "tu_learn", approval.CallID)
	require.True(t, approval.Grantable)
	require.NotContains(t, h.queryTitles(writer, "chat-00000000000000000000000000000a10", handle), "Loaded then approved",
		"nothing was written before the person answered")

	require.Equal(t, http.StatusNoContent, h.approve("uid:501", id, "tu_learn", "allow_session"))
	s.until("turn_done")
	require.Contains(t, h.queryTitles(writer, "chat-00000000000000000000000000000a10", handle), "Loaded then approved")

	var appr []string
	for _, a := range recordsOf(readLog(t, h.logDir, id), "approval") {
		appr = append(appr, fmt.Sprintf("%s=%s", a["tool"], a["decision"]))
	}
	sort.Strings(appr)
	require.Equal(t, []string{"knomit_learn=allow_session", "load_tools=auto"}, appr)

	code, body := h.do("uid:501", http.MethodGet, "/chats/"+id+"/grants", nil)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), `"tool":"knomit_learn"`)
	require.Contains(t, string(body), `"action":"learn"`)
}

// An Anthropic chat's request prefix is byte for byte what the static prefix
// builder makes of the same tools: nothing about the adaptive path reaches it.
//
// SABOTAGE: mount.go create, set Window for every chat, or have AddPrefix drop
// a tool -> the Anthropic prefix bytes differ and this fails.
func TestChat_AnthropicRequestPrefixBytesAreTheStaticPrefix(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	m := h.queue("a1", textStep("hi", usage))
	code, body := h.createOn("uid:501", "anthropic", "a1")
	require.Equal(t, http.StatusCreated, code, string(body))
	var v struct{ ID string }
	require.NoError(t, json.Unmarshal(body, &v))
	s := h.attach("uid:501", v.ID)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", v.ID, blankMessage))
	s.until("turn_done")

	const refID = "chat-00000000000000000000000000000a11"
	sess, instructions, tools, err := mcp.OpenInProcessSession(context.Background(), h.srv, h.repos, mcp.InProcessOptions{
		Principal: writer, SessionID: refID, Endpoint: "/api/v1/chats/" + refID,
		ClientName: "test-ref", ClientVersion: "x",
	})
	require.NoError(t, err)
	defer sess.Close(context.Background())
	ref := llm.NewBuilder()
	require.NoError(t, mcp.AddPrefix(ref, instructions, tools))

	got, err := llm.AnthropicPrefixJSON(m.requests()[0], "claude-test-x")
	require.NoError(t, err)
	want, err := llm.AnthropicPrefixJSON(ref.Snapshot(), "claude-test-x")
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
	require.Contains(t, string(got), `"knomit_learn"`, "fixture: the prefix holds the real tools")
	require.NotContains(t, string(got), llm.LoadToolsName)
}
