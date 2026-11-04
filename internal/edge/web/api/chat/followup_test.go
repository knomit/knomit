package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/edge/mcp"
	"knomit/internal/engine/llm"
)

// An action the classification table does not know asks, is never grantable
// and is asked again every time.
//
// SABOTAGE (one at a time): chat.go Decide `if known && !unresolved {` -> `if
// !unresolved {` (a seeded grant is honoured for an unknown call) fails the
// first ask; answerApproval `if !p.Grantable {` -> `if false {` fails the 400.
func TestChat_UnknownActionAsksAndIsNeverGrantable(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000071", "alpha")
	h.queue("a1",
		toolStep("tu_u1", "knomit_experiment", map[string]any{"binding": handle, "action": "frobnicate"}, usage),
		toolStep("tu_u2", "knomit_experiment", map[string]any{"binding": handle, "action": "frobnicate"}, usage),
		textStep("done", usage))
	id := h.create("uid:501", "a1")
	// Even a grant that somehow exists for the unknown action (the API cannot
	// create one) is not honoured: the lookup itself is guarded by `known`.
	c := h.mgr.find(id, writer.String())
	require.NotNil(t, c)
	c.mu.Lock()
	c.grants[grantKey{"repo:uid-alpha", "knomit_experiment", "frobnicate"}] = grantInfo{Tool: "knomit_experiment", Action: "frobnicate"}
	c.mu.Unlock()
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))

	evs := s.until("approval")
	var first approvalEvent
	require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &first))
	require.Equal(t, approvalEvent{CallID: "tu_u1", Tool: "knomit_experiment", Action: "frobnicate", Target: "alpha", TargetKind: "repo", Grantable: false}, first)

	// "Allow for the session" is refused and the approval stays pending.
	code, body := h.do("uid:501", http.MethodPost, "/chats/"+id+"/approvals/tu_u1", map[string]string{"decision": "allow_session"})
	require.Equal(t, http.StatusBadRequest, code, string(body))
	require.Contains(t, string(body), "Cannot allow for the session")
	code, body = h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), `"tu_u1"`, "the approval is still pending")

	// Allowing it once does not make the next identical call free.
	require.Equal(t, http.StatusNoContent, h.approve("uid:501", id, "tu_u1", "allow"))
	evs = s.until("approval")
	var second approvalEvent
	require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &second))
	require.Equal(t, "tu_u2", second.CallID, "a repeat of an unknown action asks again")
	require.False(t, second.Grantable)
	require.Equal(t, http.StatusNoContent, h.approve("uid:501", id, "tu_u2", "decline"))
	s.until("turn_done")

	code, body = h.do("uid:501", http.MethodGet, "/chats/"+id+"/grants", nil)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), `"count":1`, "no grant was added by the allow_session attempt: only the seeded one remains")
}

// A side-effecting call whose handle lookup FAILS asks (and cannot be
// granted); it does not run unprompted. The store error is injected by closing
// the control database after the handle was minted.
//
// SABOTAGE: chat.go Decide, treat a TargetPin error like an unresolved handle
// (run it as "auto") -> no approval event arrives and the test fails.
func TestChat_TargetLookupErrorAsksInsteadOfRunning(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000072", "alpha")
	h.queue("a1", toolStep("tu_e1", "knomit_learn", learnArgs(handle, "Written despite the store error"), usage), textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.NoError(t, h.sessionsDB.Close(), "inject the store error")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write"))

	evs, asked := drive(t, h, "uid:501", id, s, map[string]string{"tu_e1": "decline"})
	require.Equal(t, []string{"tu_e1"}, approvedIDs(asked), "the unresolvable call asked")
	require.False(t, asked[0].Grantable)
	require.Equal(t, "knomit_learn", asked[0].Tool)
	_, done := find(evs, "turn_done")
	require.True(t, done, names(evs))
	require.Equal(t, llm.DeclinedResult, callResult(t, h, "uid:501", id, "tu_e1").Result, "the declined call did not run")
}

// Review N3: every chat route answers another principal with 404, and the
// 404 comes from ownership, not from a missing call.
//
// SABOTAGE: mount.go find, drop the `c.owner != owner` test -> fails.
func TestChat_OtherPrincipalGets404(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000073", "alpha")
	h.queue("a1", toolStep("tu_o1", "knomit_learn", learnArgs(handle, "Ownership fixture"), usage), textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write"))
	s.until("approval")

	// While tu_o1 is pending, the other principal cannot answer it.
	code, _ := h.do("uid:1000", http.MethodPost, "/chats/"+id+"/approvals/tu_o1", map[string]string{"decision": "allow"})
	require.Equal(t, http.StatusNotFound, code, "approvals")
	require.Equal(t, http.StatusNoContent, h.approve("uid:501", id, "tu_o1", "allow"), "the owner's approval was still pending")
	s.until("turn_done")

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/chats/" + id},
		{http.MethodDelete, "/chats/" + id},
		{http.MethodGet, "/chats/" + id + "/events"},
		{http.MethodPost, "/chats/" + id + "/turns"},
		{http.MethodPost, "/chats/" + id + "/cancel"},
		{http.MethodPost, "/chats/" + id + "/approvals/tu_o1"},
		{http.MethodGet, "/chats/" + id + "/grants"},
		{http.MethodGet, "/chats/" + id + "/calls/tu_o1"},
	} {
		var body any
		if tc.method == http.MethodPost {
			body = map[string]string{"text": "x", "decision": "allow"}
		}
		code, _ := h.do("uid:1000", tc.method, tc.path, body)
		require.Equal(t, http.StatusNotFound, code, tc.method+" "+tc.path)
	}
	require.NotEqual(t, llm.DeclinedResult, callResult(t, h, "uid:501", id, "tu_o1").Result, "the call tu_o1 exists, so the owner reads it")
}

// Review N4: the chat's MCP session is opened on the endpoint
// /api/v1/chats/<id>. A query cursor is owned by (endpoint, principal), so a
// session on exactly that endpoint can resume the chat's cursor and a session
// on any other cannot.
//
// SABOTAGE: mount.go, change the Endpoint passed to OpenInProcessSession ->
// the same-endpoint session can no longer resume it.
func TestChat_SessionEndpointIsTheChatURL(t *testing.T) {
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	require.NotNil(t, c)

	bindOut, err := c.sess.CallTool(context.Background(), "knomit_bind", json.RawMessage(`{"repo":"alpha"}`))
	require.NoError(t, err)
	handle := handleFromResult(bindOut.Content)
	require.NotEmpty(t, handle)
	facts := make([]any, 25)
	for i := range facts {
		facts[i] = map[string]any{
			"topic": "gotchas", "category": "chat/endpoint", "title": fmt.Sprintf("Endpoint fact %04d", i),
			"body": fmt.Sprintf("endpoint body %04d %s", i, strings.Repeat("q", i)), "confidence": 0.8, "sources": 1,
		}
	}
	learn, _ := json.Marshal(map[string]any{"binding": handle, "moment_name": "seed", "facts": facts})
	out, err := c.sess.CallTool(context.Background(), "knomit_learn", learn)
	require.NoError(t, err)
	require.False(t, out.IsError, out.Content)
	q, _ := json.Marshal(map[string]any{"binding": handle, "path": "kb/gotchas/chat/endpoint"})
	first, err := c.sess.CallTool(context.Background(), "knomit_query", q)
	require.NoError(t, err)
	var page struct {
		Cursor *string `json:"cursor"`
	}
	require.NoError(t, json.Unmarshal([]byte(first.Content), &page))
	require.NotNil(t, page.Cursor, "the query must page: %s", first.Content)

	resume := func(sessionID, endpoint string) llm.ToolOutcome {
		s, _, _, err := mcp.OpenInProcessSession(context.Background(), h.srv, h.repos, mcp.InProcessOptions{
			Principal: writer, SessionID: sessionID, Endpoint: endpoint, ClientName: "test-endpoint", ClientVersion: "x",
		})
		require.NoError(t, err)
		defer s.Close(context.Background())
		hb := bindOn(t, s, "alpha")
		args, _ := json.Marshal(map[string]any{"binding": hb, "cursor": *page.Cursor})
		o, err := s.CallTool(context.Background(), "knomit_query", args)
		require.NoError(t, err)
		return o
	}
	same := resume("chat-00000000000000000000000000000074", "/api/v1/chats/"+id)
	require.False(t, same.IsError, "a session on the chat's endpoint resumes its cursor: %s", same.Content)
	other := resume("chat-00000000000000000000000000000075", "/api/v1/chats/chat-00000000000000000000000000000076")
	require.True(t, other.IsError, "a session on another endpoint cannot: %s", other.Content)
}

func bindOn(t *testing.T, s *mcp.InProcessSession, repo string) string {
	t.Helper()
	o, err := s.CallTool(context.Background(), "knomit_bind", json.RawMessage(`{"repo":"`+repo+`"}`))
	require.NoError(t, err)
	h := handleFromResult(o.Content)
	require.NotEmpty(t, h, o.Content)
	return h
}

// Review N5: the non-destructive writes ask. They are grantable, and a
// hypothesize start and an experiment open, sync and commit each need their
// own answer.
//
// SABOTAGE: chat.go Decide, auto-allow every known non-destructive call ->
// fails.
func TestChat_NonDestructiveWritesAsk(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000077", "alpha")
	h.queue("a1",
		toolStep("tu_h", "knomit_hypothesize", map[string]any{"binding": handle}, usage),
		toolStep("tu_xo", "knomit_experiment", map[string]any{"binding": handle, "action": "open", "name": "exp-77"}, usage),
		toolStep("tu_xs", "knomit_experiment", map[string]any{"binding": handle, "action": "sync"}, usage),
		toolStep("tu_xc", "knomit_experiment", map[string]any{"binding": handle, "action": "commit"}, usage),
		textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	decline := map[string]string{"tu_h": "decline", "tu_xo": "decline", "tu_xs": "decline", "tu_xc": "decline"}
	_, asked := drive(t, h, "uid:501", id, s, decline)

	require.Equal(t, []string{"tu_h", "tu_xo", "tu_xs", "tu_xc"}, approvedIDs(asked))
	for i, want := range []struct{ tool, action string }{
		{"knomit_hypothesize", "start"}, {"knomit_experiment", "open"}, {"knomit_experiment", "sync"}, {"knomit_experiment", "commit"},
	} {
		require.Equal(t, want.tool, asked[i].Tool)
		require.Equal(t, want.action, asked[i].Action)
		require.False(t, asked[i].Destructive, want.action)
		require.True(t, asked[i].Grantable, want.action)
		require.Equal(t, llm.DeclinedResult, callResult(t, h, "uid:501", id, asked[i].CallID).Result)
	}
}

// Review N6: with no provider registry the routes are mounted, the provider
// list is empty and starting a chat is refused with 422.
func TestChat_NoProviderRegistry(t *testing.T) {
	h := newHarness(t)
	h.mgr.reg, h.mgr.openModel, h.mgr.price = nil, nil, nil

	code, body := h.do("uid:501", http.MethodGet, "/chat-providers", nil)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), `"count":0`)
	code, body = h.do("uid:501", http.MethodPost, "/chats", map[string]string{"provider": "anthropic", "model": "a1"})
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Contains(t, string(body), "Cannot start the chat")
	require.Contains(t, string(body), "no chat providers are configured")
}

// Review N7: an endpoint that answers 401 and echoes the request headers puts
// the API key into the SDK's error text. It reaches neither the provider list,
// the turn's events, the call and chat views, the process log, nor the session
// log.
//
// SABOTAGE: llm.redactKey returns err unchanged -> fails.
func TestChat_EchoedAPIKeyNeverSurfaces(t *testing.T) {
	logs := captureLogs(t)
	h := newHarnessProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","message":"bad credentials %s"}}`, r.Header.Get("X-Api-Key"))
	})
	h.mgr.openModel = h.mgr.reg.Open // the real model, so the SDK's error is the one shown

	var seen []string
	code, body := h.do("uid:501", http.MethodGet, "/chat-providers", nil)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), "[redacted]", "the provider's error text was reached")
	seen = append(seen, string(body))

	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "hello"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	_, errored := find(evs, "turn_error")
	require.True(t, errored, names(evs))
	for _, e := range evs {
		seen = append(seen, string(e.Data))
	}
	_, body = h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	seen = append(seen, string(body))
	h.do("uid:501", http.MethodDelete, "/chats/"+id, nil)
	for _, l := range readLog(t, h.logDir, id) {
		b, _ := json.Marshal(l)
		seen = append(seen, string(b))
	}
	seen = append(seen, logs.String())
	for i, s := range seen {
		require.NotContains(t, s, plainKey, fmt.Sprintf("capture %d", i))
	}
	require.Contains(t, strings.Join(seen, "\n"), "[redacted]")
}
