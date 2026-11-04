package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
	"knomit/internal/engine/store"
)

var usage = llm.Usage{Input: 11, CacheRead: 22, CacheWrite: 33, Output: 44}

type approvalEvent struct {
	CallID      string `json:"call_id"`
	Tool        string `json:"tool"`
	Action      string `json:"action"`
	Target      string `json:"target"`
	TargetKind  string `json:"target_kind"`
	Destructive bool   `json:"destructive"`
	Grantable   bool   `json:"grantable"`
}

type toolCallEvent struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"`
	IsError bool   `json:"is_error"`
}

// drive reads a stream until the turn ends, answering each approval with
// decisions[callID] ("allow" when absent). It returns every event and the
// approvals in the order they were asked.
func drive(t *testing.T, h *harness, who, id string, s *stream, decisions map[string]string) ([]sseEvent, []approvalEvent) {
	t.Helper()
	var all []sseEvent
	var asked []approvalEvent
	for {
		e := s.next("", 10*1e9)
		all = append(all, e)
		switch e.Name {
		case "approval":
			var a approvalEvent
			require.NoError(t, json.Unmarshal(e.Data, &a))
			asked = append(asked, a)
			d := decisions[a.CallID]
			if d == "" {
				d = "allow"
			}
			require.Equal(t, http.StatusNoContent, h.approve(who, id, a.CallID, d))
		case "turn_done", "turn_error":
			return all, asked
		}
	}
}

func approvedIDs(as []approvalEvent) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.CallID)
	}
	return out
}

// callResult fetches a tool call's stored record.
func callResult(t *testing.T, h *harness, who, id, callID string) callRecord {
	t.Helper()
	code, body := h.do(who, http.MethodGet, "/chats/"+id+"/calls/"+callID, nil)
	require.Equal(t, http.StatusOK, code, string(body))
	var rec callRecord
	require.NoError(t, json.Unmarshal(body, &rec))
	return rec
}

func TestChat_ReadToolRunsWithoutApproval(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", toolStep("tu_r1", "knomit_repos", map[string]any{}, usage), textStep("listed", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "which repos?"))
	evs, asked := drive(t, h, "uid:501", id, s, nil)

	require.Empty(t, asked, "a read-only tool needs no approval")
	_, hasApproval := find(evs, "approval")
	require.False(t, hasApproval)
	var states []string
	for _, e := range evs {
		if e.Name == "tool_call" {
			var c toolCallEvent
			require.NoError(t, json.Unmarshal(e.Data, &c))
			require.Equal(t, "tu_r1", c.ID)
			require.Equal(t, "knomit_repos", c.Name)
			states = append(states, c.State)
		}
	}
	require.Equal(t, []string{"running", "done"}, states)
	require.Equal(t, "turn_done", evs[len(evs)-1].Name)

	rec := callResult(t, h, "uid:501", id, "tu_r1")
	require.Contains(t, rec.Result, "alpha")
	require.False(t, rec.IsError)
}

func TestChat_WriteToolWaitsForApproval(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000001", "alpha")
	h.queue("a1", toolStep("tu_w1", "knomit_learn", learnArgs(handle, "Alpha fact by chat"), usage), textStep("written", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write a fact"))

	evs := s.until("approval")
	var a approvalEvent
	require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &a))
	require.Equal(t, approvalEvent{CallID: "tu_w1", Tool: "knomit_learn", Action: "learn", Target: "alpha", TargetKind: "repo", Destructive: true, Grantable: true}, a)
	require.NotContains(t, h.queryTitles(writer, "chat-00000000000000000000000000000002", handle), "Alpha fact by chat",
		"nothing is written before the person allows it")

	require.Equal(t, http.StatusNoContent, h.approve("uid:501", id, "tu_w1", "allow"))
	s.until("turn_done")
	require.Contains(t, h.queryTitles(writer, "chat-00000000000000000000000000000003", handle), "Alpha fact by chat")

	// The commit carries the chat's trace trailer, added by the host.
	rec := callResult(t, h, "uid:501", id, "tu_w1")
	var res struct {
		Commits []struct{ Hash string } `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(rec.Result), &res), rec.Result)
	require.Len(t, res.Commits, 1)
	var msg string
	require.NoError(t, h.repos.Get("alpha").WithRead(func(svc *store.Service) {
		info, err := svc.FactsRepo().CommitInfo(context.Background(), plumbing.NewHash(res.Commits[0].Hash))
		require.NoError(t, err)
		msg = info.Message
	}))
	require.Contains(t, msg, "\nKnomit-Trace: chat:"+id+"\n")
	// What the model sent is what the history keeps: no trace in the stored args.
	require.NotContains(t, string(rec.Arguments), "Knomit-Trace")
}

// Review Focus 2: a grant is keyed on the target pin.
func TestChat_AllowForSessionScopedToTarget(t *testing.T) {
	h := newHarness(t)
	a1 := h.bind(writer, "chat-00000000000000000000000000000011", "alpha")
	a2 := h.bind(writer, "chat-00000000000000000000000000000012", "alpha")
	b1 := h.bind(writer, "chat-00000000000000000000000000000013", "beta")
	require.NotEqual(t, a1, a2)
	h.queue("a1",
		toolStep("tu_1", "knomit_learn", learnArgs(a1, "First alpha"), usage),
		toolStep("tu_2", "knomit_learn", learnArgs(a1, "Second alpha"), usage),
		toolStep("tu_3", "knomit_learn", learnArgs(b1, "Beta write"), usage),
		toolStep("tu_4", "knomit_learn", learnArgs(a2, "Alpha via the other handle"), usage),
		textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write four facts"))
	_, asked := drive(t, h, "uid:501", id, s, map[string]string{"tu_1": "allow_session"})

	require.Equal(t, []string{"tu_1", "tu_3"}, approvedIDs(asked),
		"the second alpha write and the one through another alpha handle are covered; beta is not")
	require.Equal(t, "alpha", asked[0].Target)
	require.Equal(t, "beta", asked[1].Target)
}

func TestChat_ReviewAnswerGrantDoesNotCoverRollback(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000021", "alpha")
	h.queue("a1",
		toolStep("tu_a", "knomit_review", map[string]any{"binding": handle, "session_id": "s-91", "response": "r-91"}, usage),
		toolStep("tu_b", "knomit_review", map[string]any{"binding": handle, "session_id": "s-91", "response": "r-92"}, usage),
		toolStep("tu_c", "knomit_experiment", map[string]any{"binding": handle, "action": "rollback", "name": "exp-93"}, usage),
		textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	_, asked := drive(t, h, "uid:501", id, s, map[string]string{"tu_a": "allow_session"})

	require.Equal(t, []string{"tu_a", "tu_c"}, approvedIDs(asked), "the second answer is covered, the rollback is not")
	require.Equal(t, "answer", asked[0].Action)
	require.Equal(t, "rollback", asked[1].Action)
}

// Review Focus 6 and Q7: a takeover is a write of its own kind.
func TestChat_ReviewTakeoverAsksAndIsNotCoveredByAnswerGrant(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000031", "alpha")
	h.queue("a1",
		toolStep("tu_a", "knomit_review", map[string]any{"binding": handle, "session_id": "s-81", "response": "r-81"}, usage),
		toolStep("tu_t", "knomit_review", map[string]any{"binding": handle, "takeover": true}, usage),
		toolStep("tu_s", "knomit_review", map[string]any{"binding": handle}, usage),
		toolStep("tu_p", "knomit_review", map[string]any{"binding": handle, "session_id": "s-81", "page": 2}, usage),
		textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	_, asked := drive(t, h, "uid:501", id, s, map[string]string{"tu_a": "allow_session", "tu_t": "decline", "tu_s": "decline"})

	require.Equal(t, []string{"tu_a", "tu_t", "tu_s"}, approvedIDs(asked), "a page needs no approval; takeover and start do")
	require.Equal(t, []string{"answer", "takeover", "start"}, []string{asked[0].Action, asked[1].Action, asked[2].Action})
	require.True(t, asked[1].Destructive)
	require.Equal(t, "knomit_review", asked[1].Tool)
	require.Equal(t, llm.DeclinedResult, callResult(t, h, "uid:501", id, "tu_t").Result, "a declined takeover did not run")
	require.Equal(t, llm.DeclinedResult, callResult(t, h, "uid:501", id, "tu_s").Result)
	require.NotEqual(t, llm.DeclinedResult, callResult(t, h, "uid:501", id, "tu_p").Result, "the page call ran")
}

func TestChat_GrantsEndpointListsSessionGrants(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000041", "alpha")
	h.queue("a1", toolStep("tu_g", "knomit_learn", learnArgs(handle, "Grant fixture"), usage), textStep("ok", usage))
	id := h.create("uid:501", "a1")

	code, body := h.do("uid:501", http.MethodGet, "/chats/"+id+"/grants", nil)
	require.Equal(t, http.StatusOK, code)
	require.JSONEq(t, `{"count":0,"_links":{"self":{"href":"/api/v1/chats/`+id+`/grants"}},"_embedded":{"grants":[]}}`, string(body))

	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	drive(t, h, "uid:501", id, s, map[string]string{"tu_g": "allow_session"})

	code, body = h.do("uid:501", http.MethodGet, "/chats/"+id+"/grants", nil)
	require.Equal(t, http.StatusOK, code)
	var v struct {
		Embedded struct {
			Grants []grantInfo `json:"grants"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(body, &v))
	require.Equal(t, []grantInfo{{Target: "alpha", Kind: "repo", Tool: "knomit_learn", Action: "learn"}}, v.Embedded.Grants)
}

func TestChat_ListReturnsOnlyCallersChats(t *testing.T) {
	h := newHarness(t)
	a := h.create("uid:501", "a1")
	b := h.create("uid:501", "a2")
	c := h.create("uid:1000", "a1")

	ids := func(who string) []string {
		code, body := h.do(who, http.MethodGet, "/chats", nil)
		require.Equal(t, http.StatusOK, code)
		var v struct {
			Count    int `json:"count"`
			Embedded struct {
				Chats []chatItem `json:"chats"`
			} `json:"_embedded"`
		}
		require.NoError(t, json.Unmarshal(body, &v))
		require.Equal(t, len(v.Embedded.Chats), v.Count)
		var out []string
		for _, ch := range v.Embedded.Chats {
			out = append(out, ch.ID)
		}
		return out
	}
	want := []string{a, b}
	sort.Strings(want)
	require.Equal(t, want, ids("uid:501"))
	require.Equal(t, []string{c}, ids("uid:1000"))
}

func TestChat_DeclineIsToolError(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000051", "alpha")
	m := h.queue("a1", toolStep("tu_d", "knomit_learn", learnArgs(handle, "Declined write"), usage), textStep("understood", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write"))
	drive(t, h, "uid:501", id, s, map[string]string{"tu_d": "decline"})

	reqs := m.requests()
	require.Len(t, reqs, 2)
	var got *llm.ToolResult
	for _, it := range reqs[1].Hooks[llm.HookTurn] {
		for _, b := range it.Entry.Blocks {
			if b.Kind == llm.BlockToolResult {
				got = b.ToolResult
			}
		}
	}
	require.NotNil(t, got, "the model's next request carries the tool result")
	require.Equal(t, llm.ToolResult{ToolUseID: "tu_d", Content: llm.DeclinedResult, IsError: true}, *got)
	require.NotContains(t, h.queryTitles(writer, "chat-00000000000000000000000000000052", handle), "Declined write")
}

func TestChat_OtherPrincipalGets404(t *testing.T) {
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/chats/" + id},
		{http.MethodDelete, "/chats/" + id},
		{http.MethodGet, "/chats/" + id + "/events"},
		{http.MethodPost, "/chats/" + id + "/turns"},
		{http.MethodPost, "/chats/" + id + "/cancel"},
		{http.MethodGet, "/chats/" + id + "/grants"},
		{http.MethodGet, "/chats/" + id + "/calls/tu_1"},
	} {
		var body any
		if tc.method == http.MethodPost {
			body = map[string]string{"text": "x"}
		}
		code, _ := h.do("uid:1000", tc.method, tc.path, body)
		require.Equal(t, http.StatusNotFound, code, tc.method+" "+tc.path)
	}
	code, _ := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code, "the owner still sees it")
}

// Q2 and Review Focus 8.
func TestChat_ReadOnlyPrincipalCanChatReads(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(roUser, "chat-00000000000000000000000000000061", "alpha")
	m := h.queue("a1",
		toolStep("tu_q", "knomit_query", map[string]any{"binding": handle, "text": "anything"}, usage),
		toolStep("tu_l", "knomit_learn", learnArgs(handle, "Written by a reader"), usage),
		textStep("done", usage))
	id := h.create("uid:777", "a1")
	s := h.attach("uid:777", id)

	// Hook 1 is exactly this principal's tools/list.
	c := h.mgr.find(id, roUser.String())
	require.NotNil(t, c)
	var hook1 []string
	for _, it := range c.loop.Builder.Snapshot().Hooks[llm.HookTools] {
		hook1 = append(hook1, it.Tool.Name)
	}
	listed := h.toolNames(roUser, "chat-00000000000000000000000000000062")
	require.Equal(t, listed, hook1)
	for _, n := range []string{"knomit_learn", "knomit_update", "knomit_retract"} {
		require.NotContains(t, hook1, n)
	}
	require.Contains(t, hook1, "knomit_query")

	require.Equal(t, http.StatusAccepted, h.turn("uid:777", id, "read then write"))
	_, asked := drive(t, h, "uid:777", id, s, nil)
	require.Equal(t, []string{"tu_l"}, approvedIDs(asked), "the read ran without asking; the write asked and was allowed")
	require.False(t, callResult(t, h, "uid:777", id, "tu_q").IsError)
	rec := callResult(t, h, "uid:777", id, "tu_l")
	require.True(t, rec.IsError, "the permission gate refused the write")
	require.Contains(t, rec.Result, "write")
	require.Len(t, m.requests(), 3)
	require.NotContains(t, h.queryTitles(writer, "chat-00000000000000000000000000000063", handle), "Written by a reader")
}

func TestChat_ProvidersListsModels(t *testing.T) {
	h := newHarness(t)
	code, body := h.do("uid:501", http.MethodGet, "/chat-providers", nil)
	require.Equal(t, http.StatusOK, code)
	var v struct {
		Embedded struct {
			Providers []providerView `json:"providers"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(body, &v))
	require.Equal(t, []providerView{{Name: "anthropic", Kind: "anthropic", Models: []modelView{
		{ID: "a1", Available: true, Tools: true},
		{ID: "a2", Reason: "not offered by the provider"},
	}}}, v.Embedded.Providers)
	require.NotContains(t, string(body), plainKey)
}

func TestChat_CreateRefusesUnknownModel(t *testing.T) {
	h := newHarness(t)
	code, body := h.do("uid:501", http.MethodPost, "/chats", map[string]string{"provider": "anthropic", "model": "zzz"})
	require.Equal(t, http.StatusBadRequest, code, string(body))
	require.Contains(t, string(body), "allowlist")
	code, _ = h.do("", http.MethodPost, "/chats", map[string]string{"provider": "anthropic", "model": "a1"})
	require.Equal(t, http.StatusUnauthorized, code, "a chat needs a signed-in principal")
}

func TestChat_BusyTurnIs409AndCancelUnwinds(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", step{block: true})
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "think forever"))
	require.Equal(t, http.StatusConflict, h.turn("uid:501", id, "again"))
	code, _ := h.do("uid:501", http.MethodPost, "/chats/"+id+"/cancel", nil)
	require.Equal(t, http.StatusNoContent, code)
	evs := s.until("turn_error")
	var e struct {
		Message   string `json:"message"`
		Cancelled bool   `json:"cancelled"`
	}
	require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &e))
	require.True(t, e.Cancelled)
	require.True(t, strings.Contains(e.Message, "cancelled"), e.Message)
	// The chat is reusable.
	h.mgr.find(id, writer.String()).mu.Lock()
	running := h.mgr.find(id, writer.String()).running
	h.mgr.find(id, writer.String()).mu.Unlock()
	require.False(t, running)
}
