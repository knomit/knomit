package chat

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/mcp"
	"knomit/internal/engine/llm"
	"knomit/internal/engine/store"
	storemigrate "knomit/internal/engine/store/migrate"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/clientsessions"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
)

func TestMain(m *testing.M) {
	testsigner.Install()
	os.Exit(m.Run())
}

// The principals the tests act as. writer and writer2 hold read and write; ro
// holds read only.
var (
	writer  = auth.Principal{Kind: auth.KindBridge, ID: "uid:501", Via: auth.ViaSocket}
	writer2 = auth.Principal{Kind: auth.KindBridge, ID: "uid:1000", Via: auth.ViaSocket}
	roUser  = auth.Principal{Kind: auth.KindBridge, ID: "uid:777", Via: auth.ViaSocket}
)

// plainKey is the plaintext api_key in the test provider config. Nothing may
// ever write it to a log or to the session log.
const plainKey = "sk-plain-0a1b2c3d4e5f60718293a4b5c6d7e8f9"

// step is one scripted model response.
type step struct {
	resp   *llm.Response
	err    error
	block  bool // wait for ctx to end, then return its error
	events []llm.Event
	// thinking is streamed through StreamOptions.Thinking before the events.
	thinking []string
	// fn, when set, chooses the step from the request (a model reading the
	// handle a previous tool call returned).
	fn func(llm.Snapshot) step
}

// scripted is a ChatModel that plays steps and records the snapshots it saw.
type scripted struct {
	name string
	// adaptive makes the model an Ollama-like one with no prompt cache to
	// protect (Caps.StablePrefix false); by default it is Anthropic-like.
	adaptive bool
	window   int // the context window it reports; 0 is 1,000,000

	mu    sync.Mutex
	steps []step
	snaps []llm.Snapshot
}

func (m *scripted) Provider() string {
	if m.adaptive {
		return "ollama"
	}
	return "anthropic"
}
func (m *scripted) Model() string { return m.name }
func (m *scripted) Caps() llm.Caps {
	w := m.window
	if w == 0 {
		w = 1_000_000
	}
	return llm.Caps{Tools: true, ContextWindow: w, MaxOutput: 1000, StablePrefix: !m.adaptive}
}

func (m *scripted) Stream(ctx context.Context, s llm.Snapshot, o llm.StreamOptions, sink func(llm.Event)) (*llm.Response, error) {
	m.mu.Lock()
	m.snaps = append(m.snaps, s)
	if len(m.steps) == 0 {
		m.mu.Unlock()
		return nil, errors.New("scripted model: no step left")
	}
	st := m.steps[0]
	m.steps = m.steps[1:]
	m.mu.Unlock()
	if st.fn != nil {
		st = st.fn(s)
	}
	for _, t := range st.thinking {
		if o.Thinking != nil {
			o.Thinking(t)
		}
	}
	for _, ev := range st.events {
		sink(ev)
	}
	if st.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if st.resp != nil {
		for _, b := range st.resp.Blocks {
			switch b.Kind {
			case llm.BlockText:
				sink(llm.Event{Kind: llm.EventTextDelta, Text: b.Text})
			case llm.BlockToolUse:
				sink(llm.Event{Kind: llm.EventToolUse, ToolUse: b.ToolUse})
			}
		}
	}
	return st.resp, st.err
}

func (m *scripted) requests() []llm.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]llm.Snapshot(nil), m.snaps...)
}

func textStep(text string, u llm.Usage) step {
	return step{resp: &llm.Response{Blocks: []llm.Block{{Kind: llm.BlockText, Text: text}}, StopReason: llm.StopEndTurn, Usage: u}}
}

func toolStep(id, name string, args any, u llm.Usage) step {
	raw, _ := json.Marshal(args)
	return step{resp: &llm.Response{
		Blocks:     []llm.Block{{Kind: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: id, Name: name, Args: raw}}},
		StopReason: llm.StopToolUse, Usage: u,
	}}
}

// harness is a real MCP server and manager behind the chat routes.
type harness struct {
	t          *testing.T
	repos      *repos.Manager
	sessions   *clientsessions.Store
	sessionsDB *sql.DB
	srv        *mcpserver.MCPServer
	mgr        *manager
	ts         *httptest.Server
	sessionDir string
	logDir     string
	life       context.CancelFunc

	mu     sync.Mutex
	models []llm.ChatModel
	next   int
}

func newSessionsStore(t *testing.T) (*clientsessions.Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "control.db")+"?_busy_timeout=5000&_journal_mode=WAL")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, storemigrate.Control(db))
	return clientsessions.New(db, clientsessions.Policy{DeadAfter: time.Hour, HiddenAfter: 3 * time.Hour}), db
}

func newRepo(t *testing.T, m *repos.Manager, name string) {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	uid := "uid-" + name
	require.NoError(t, m.Repos().Insert(control.RepoRecord{UID: uid, Name: name, State: control.StateActive, Profile: "code", CreatedAt: 1}))
	m.Set(name, repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: name, UID: uid, Svc: svc, Ontology: fact.CodeOntology(), OntologyRoot: "kb", AgentBranch: "agent/test",
	}))
}

// newHarness starts the chat routes over a started Manager with repos alpha
// and beta, a real MCP server and a real provider registry (the scripted
// models replace only what the registry would have opened).
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"a1","type":"model","display_name":"A1","created_at":"2026-01-01T00:00:00Z"}],"has_more":false}`)
	})
}

// newHarnessProvider is newHarness with the fake Anthropic endpoint's handler
// chosen by the test (the default lists the model a1).
func newHarnessProvider(t *testing.T, provider http.HandlerFunc) *harness {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: t.TempDir()},
		AgentID: testsigner.FixtureAgentID("agent/test"), AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	st, stDB := newSessionsStore(t)
	m.SetClientSessions(st)
	newRepo(t, m, "alpha")
	newRepo(t, m, "beta")

	srv := mcp.NewServer("kb", m, false, auth.StaticGrants{
		writer.String():  {auth.Read: {}, auth.Write: {}},
		writer2.String(): {auth.Read: {}, auth.Write: {}},
		roUser.String():  {auth.Read: {}},
	})

	anthropicModels := httptest.NewServer(provider)
	t.Cleanup(anthropicModels.Close)
	reg := llm.NewRegistry(map[string]config.LLMProviderConfig{
		"anthropic": {
			Kind: "anthropic", BaseURL: anthropicModels.URL, APIKey: plainKey, Models: []string{"a1", "a2"},
			Entries: []config.LLMModelEntry{{ID: "a1", Name: "Alpha"}, {ID: "a2"}},
			ModelDefaults: map[string]config.LLMModelConfig{"a1": {Price: &config.LLMPriceCard{
				Tiers: []config.LLMPriceTier{
					{UpToPromptTokens: 100_000, InputPerMTok: 0.1, OutputPerMTok: 0.5},
					{InputPerMTok: 0.5, OutputPerMTok: 2.5},
				},
				CacheReadMult: 0.1, CacheWrite5mMult: 1.25, CacheWrite1hMult: 2, WarnPromptTokens: 95_000,
			}}},
		},
	})

	life, endLife := context.WithCancel(context.Background())
	h := &harness{t: t, repos: m, sessions: st, sessionsDB: stDB, srv: srv, sessionDir: filepath.Join(t.TempDir(), "chat-sessions"), logDir: filepath.Join(t.TempDir(), "chat-logs"), life: endLife}
	var released sync.WaitGroup
	h.mgr = newManager(life, srv, m, reg, nil, h.sessionDir, h.logDir, &released)
	realOpen := h.mgr.openModel
	h.mgr.openModel = func(ctx context.Context, provider, model string) (llm.ChatModel, error) {
		if _, err := realOpen(ctx, provider, model); err != nil {
			return nil, err
		}
		return h.nextModel(model), nil
	}
	t.Cleanup(func() {
		endLife()
		released.Wait()
	})

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.Header.Get("X-Test-Principal") {
				case "":
					next.ServeHTTP(w, req)
					return
				case "uid:501":
					req = req.WithContext(auth.WithPrincipal(req.Context(), writer))
				case "uid:1000":
					req = req.WithContext(auth.WithPrincipal(req.Context(), writer2))
				case "uid:777":
					req = req.WithContext(auth.WithPrincipal(req.Context(), roUser))
				}
				// What the local auth edge attaches next to the principal.
				ctx := auth.WithRepoReach(req.Context(), auth.AllRepos)
				ctx = auth.WithLensReach(ctx, auth.AllLenses)
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		h.mgr.routes(r)
	})
	h.ts = httptest.NewServer(r)
	t.Cleanup(h.ts.Close)
	return h
}

// queue adds a scripted model for the next chat opened.
func (h *harness) queue(model string, steps ...step) *scripted {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := &scripted{name: model, steps: steps}
	h.models = append(h.models, m)
	return m
}

func (h *harness) nextModel(name string) llm.ChatModel {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.next >= len(h.models) {
		return &scripted{name: name}
	}
	m := h.models[h.next]
	h.next++
	return m
}

// do sends a JSON request as who ("" for no principal) and returns the status
// and body.
func (h *harness) do(who, method, path string, body any) (int, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(h.t, err)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+"/api/v1"+path, rdr)
	require.NoError(h.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if who != "" {
		req.Header.Set("X-Test-Principal", who)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// create starts a chat and returns its id.
func (h *harness) create(who, model string) string {
	h.t.Helper()
	code, body := h.do(who, http.MethodPost, "/chats", map[string]string{"provider": "anthropic", "model": model})
	require.Equal(h.t, http.StatusCreated, code, string(body))
	var v struct {
		ID string `json:"id"`
	}
	require.NoError(h.t, json.Unmarshal(body, &v))
	return v.ID
}

// sseEvent is one parsed frame.
type sseEvent struct {
	Name string
	Data json.RawMessage
}

// stream is a client attached to a chat's events.
type stream struct {
	t      *testing.T
	events chan sseEvent
	cancel context.CancelFunc
	done   chan struct{}
}

// chatOfEvent is the chat id an event is tagged with ("" for a page-level one).
func chatOfEvent(e sseEvent) string {
	var v struct {
		Chat string `json:"chat"`
	}
	_ = json.Unmarshal(e.Data, &v)
	return v.Chat
}

// attach opens the page stream (GET /chats/events), keeps only chat id's
// events plus ready, and waits for ready. Closing it ends that page stream.
func (h *harness) attach(who, id string) *stream { return h.attachFiltered(who, id) }

// attachPage opens the page stream and waits for ready; every event shows.
func (h *harness) attachPage(who string) *stream { return h.attachFiltered(who, "") }

func (h *harness) attachFiltered(who, only string) *stream {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.ts.URL+"/api/v1/chats/events", nil)
	require.NoError(h.t, err)
	if who != "" {
		req.Header.Set("X-Test-Principal", who)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusOK, resp.StatusCode)
	s := &stream{t: h.t, events: make(chan sseEvent, 1024), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var name string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev := sseEvent{Name: name, Data: json.RawMessage(strings.TrimPrefix(line, "data: "))}
				if only != "" && name != "ready" && chatOfEvent(ev) != only {
					continue
				}
				s.events <- ev
			}
		}
	}()
	h.t.Cleanup(s.close)
	first := s.next("ready", 3*time.Second)
	require.Equal(h.t, "ready", first.Name)
	return s
}

func (s *stream) close() {
	s.cancel()
	<-s.done
}

// next returns the next event, which must arrive within d.
func (s *stream) next(_ string, d time.Duration) sseEvent {
	s.t.Helper()
	select {
	case e := <-s.events:
		return e
	case <-time.After(d):
		s.t.Fatal("no event within", d)
		return sseEvent{}
	}
}

// until returns every event up to and including the first named one.
func (s *stream) until(name string) []sseEvent {
	s.t.Helper()
	var out []sseEvent
	for {
		e := s.next(name, 10*time.Second)
		out = append(out, e)
		if e.Name == name {
			return out
		}
	}
}

func names(evs []sseEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Name)
	}
	return out
}

func find(evs []sseEvent, name string) (sseEvent, bool) {
	for _, e := range evs {
		if e.Name == name {
			return e, true
		}
	}
	return sseEvent{}, false
}

func (h *harness) turn(who, id, text string) int {
	h.t.Helper()
	code, _ := h.do(who, http.MethodPost, "/chats/"+id+"/turns", map[string]string{"text": text})
	return code
}

func (h *harness) approve(who, id, callID, decision string) int {
	h.t.Helper()
	code, _ := h.do(who, http.MethodPost, "/chats/"+id+"/approvals/"+callID, map[string]string{"decision": decision})
	return code
}

// bind mints a binding handle for repo through a throwaway MCP session of the
// same principal on the same server (what the model would have done with
// knomit_bind), so the scripted calls can name it.
func (h *harness) bind(p auth.Principal, sessionID, repo string) string {
	h.t.Helper()
	s, _, _, err := mcp.OpenInProcessSession(context.Background(), h.srv, h.repos, mcp.InProcessOptions{
		Principal: p, SessionID: sessionID, Endpoint: "/api/v1/chats/" + sessionID,
		ClientName: "test-binder", ClientVersion: "x",
	})
	require.NoError(h.t, err)
	h.t.Cleanup(func() { _ = s.Close(context.Background()) })
	out, err := s.CallTool(context.Background(), "knomit_bind", json.RawMessage(`{"repo":"`+repo+`"}`))
	require.NoError(h.t, err)
	var env struct {
		Binding string `json:"binding"`
	}
	require.NoError(h.t, json.NewDecoder(strings.NewReader(out.Content)).Decode(&env))
	require.NotEmpty(h.t, env.Binding, out.Content)
	return env.Binding
}

// learnArgs is a knomit_learn call writing one fact with a distinctive title.
func learnArgs(handle, title string) map[string]any {
	return map[string]any{
		"binding": handle, "moment_name": "chat-write",
		"facts": []any{map[string]any{
			"topic": "gotchas", "category": "chat/api", "title": title,
			"body": "written through the chat api test", "confidence": 0.8, "sources": 1,
		}},
	}
}

// factTitles lists the titles of the facts in a repo's agent branch.
func (h *harness) queryTitles(p auth.Principal, sessionID, handle string) string {
	h.t.Helper()
	s, _, _, err := mcp.OpenInProcessSession(context.Background(), h.srv, h.repos, mcp.InProcessOptions{
		Principal: p, SessionID: sessionID, Endpoint: "/api/v1/chats/" + sessionID,
		ClientName: "test-reader", ClientVersion: "x",
	})
	require.NoError(h.t, err)
	defer s.Close(context.Background())
	out, err := s.CallTool(context.Background(), "knomit_query", json.RawMessage(`{"binding":"`+handle+`","path":"kb/gotchas/chat"}`))
	require.NoError(h.t, err)
	return out.Content
}

// captureLogs swaps the global logger for a buffer for the test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	var buf syncBuffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = orig })
	return &buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// toolNames is the sorted tools/list a principal sees, read through a
// throwaway in-process session on the same server.
func (h *harness) toolNames(p auth.Principal, sessionID string) []string {
	h.t.Helper()
	s, _, tools, err := mcp.OpenInProcessSession(context.Background(), h.srv, h.repos, mcp.InProcessOptions{
		Principal: p, SessionID: sessionID, Endpoint: "/api/v1/chats/" + sessionID,
		ClientName: "test-lister", ClientVersion: "x",
	})
	require.NoError(h.t, err)
	defer s.Close(context.Background())
	var out []string
	for _, td := range tools {
		out = append(out, td.Name)
	}
	sort.Strings(out)
	return out
}

// lastToolResult is the content of the newest tool result in a request.
func lastToolResult(s llm.Snapshot) string {
	var out string
	for _, h := range []llm.Hook{llm.HookConversation, llm.HookTurn} {
		for _, it := range s.Hooks[h] {
			if it.Entry == nil {
				continue
			}
			for _, b := range it.Entry.Blocks {
				if b.Kind == llm.BlockToolResult {
					out = b.ToolResult.Content
				}
			}
		}
	}
	return out
}

// handleFromResult reads the binding handle out of a knomit_bind result.
func handleFromResult(content string) string {
	var env struct {
		Binding string `json:"binding"`
	}
	_ = json.NewDecoder(strings.NewReader(content)).Decode(&env)
	return env.Binding
}
