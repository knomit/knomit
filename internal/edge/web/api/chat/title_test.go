package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

// titleUsage is the auto-title request's usage: small, and unlike the turn's, so
// a sum shows that it was added.
var titleUsage = llm.Usage{Input: 300, Output: 7}

// withAutoTitle turns auto-title on in a harness (it is off by default there)
// and records each title request's options.
func withAutoTitle(h *harness) *[]llm.StreamOptions {
	var (
		mu   sync.Mutex
		opts []llm.StreamOptions
	)
	h.mgr.titleStream = func(ctx context.Context, c *chat, s llm.Snapshot, o llm.StreamOptions) (*llm.Response, error) {
		mu.Lock()
		opts = append(opts, o)
		mu.Unlock()
		return defaultTitleStream(ctx, c, s, o)
	}
	return &opts
}

// untilTitle reads chat_state frames until one carries want as its title.
func untilTitle(t *testing.T, s *stream, want string) rowState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		e := s.next("chat_state", 10*time.Second)
		if e.Name != "chat_state" {
			continue
		}
		var st rowState
		require.NoError(t, json.Unmarshal(e.Data, &st))
		if st.Title == want {
			return st
		}
	}
	t.Fatalf("no chat_state with title %q", want)
	return rowState{}
}

// waitIdle waits until the chat's turns and side calls have all returned.
func waitIdle(t *testing.T, c *chat) {
	t.Helper()
	done := make(chan struct{})
	go func() { c.turns.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the chat's goroutines did not finish")
	}
}

// After the first completed turn the chat's own model is asked once, with no
// tools, no thinking and a tight budget, for a title; its answer replaces the
// first-message name, goes out as chat_state, is logged, and its cost is added
// to the turn (so to the session cost) and announced as a side usage event.
// SABOTAGE: drop maybeAutoTitle from finishTurn, or addSideUsage -> red.
func TestChat_AutoTitleAfterTheFirstCompletedTurn(t *testing.T) {
	h := newHarness(t)
	opts := withAutoTitle(h)
	m := h.queue("a1", textStep("The rail shows marks per chat state.", usage), textStep("\"Rail marks and chat states.\"\n", titleUsage))
	id := h.create("uid:501", "a1")
	s := h.attachPage("uid:501")
	first := "bind to alpha then tell me how the rail marks chat states"
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, first))

	st := untilTitle(t, s, "Rail marks and chat states")
	c := h.mgr.find(id, writer.String())
	waitIdle(t, c)
	r := h.row("uid:501", id)
	require.Equal(t, "Rail marks and chat states", r.Title, "the quotes and the full stop are trimmed")
	require.Equal(t, st.Title, r.Title)

	// The request: the chat's first exchange, then the prompt; no tools, no system.
	reqs := m.requests()
	require.Len(t, reqs, 2, "the turn's request and ONE title request")
	tr := reqs[1]
	require.Empty(t, tr.Hooks[llm.HookTools])
	require.Empty(t, tr.Hooks[llm.HookSystem])
	conv := tr.Hooks[llm.HookConversation]
	require.Len(t, conv, 2)
	require.Equal(t, llm.RoleUser, conv[0].Entry.Role)
	require.Equal(t, first, conv[0].Entry.Blocks[0].Text)
	require.Equal(t, llm.RoleAssistant, conv[1].Entry.Role)
	require.Equal(t, "The rail shows marks per chat state.", conv[1].Entry.Blocks[0].Text)
	require.Len(t, tr.Hooks[llm.HookTurn], 1)
	require.Equal(t, titlePrompt, tr.Hooks[llm.HookTurn][0].Entry.Blocks[0].Text)
	require.Equal(t, []llm.StreamOptions{{MaxOutput: titleMaxTokens, NoThinking: true}}, *opts)
	require.Equal(t, 32, titleMaxTokens)

	// The cost: the turn's request plus the title request, priced by the card.
	price := h.mgr.price("anthropic", "a1")
	want := price.Estimate(usage).USD + price.Estimate(titleUsage).USD
	require.NotNil(t, r.CostUSD)
	require.InDelta(t, want, *r.CostUSD, 1e-12)
	require.Greater(t, price.Estimate(titleUsage).USD, 0.0)
	c.mu.Lock()
	u := *c.view[0].Usage
	c.mu.Unlock()
	require.Equal(t, usage.Input+titleUsage.Input, u.Input)
	require.Equal(t, usage.Output+titleUsage.Output, u.Output)

	// The persistent record.
	var title map[string]any
	for _, l := range readLog(t, h.logDir, id) {
		if l["type"] == "title" {
			title = l
		}
	}
	require.NotNil(t, title, "a title record in the session log")
	require.Equal(t, "auto", title["source"])
	require.Equal(t, "Rail marks and chat states", title["title"])

	// Once per chat: a second completed turn asks for no title.
	m.mu.Lock()
	m.steps = append(m.steps, textStep("more", usage))
	m.mu.Unlock()
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "and the error mark?"))
	time.Sleep(100 * time.Millisecond)
	waitIdle(t, c)
	require.Len(t, m.requests(), 3, "turn, title, turn: no second title request")
	require.Equal(t, "Rail marks and chat states", h.row("uid:501", id).Title)
}

// The usage event of the title request says it is a side request, so the page
// adds its cost to the turn without taking its prompt for the context meter.
func TestChat_AutoTitleUsageEventIsMarkedSide(t *testing.T) {
	h := newHarness(t)
	withAutoTitle(h)
	h.queue("a1", textStep("answer", usage), textStep("A short title", titleUsage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "question"))
	var sides []map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for len(sides) == 0 && time.Now().Before(deadline) {
		e := s.next("usage", 10*time.Second)
		if e.Name != "usage" {
			continue
		}
		var v map[string]any
		require.NoError(t, json.Unmarshal(e.Data, &v))
		if v["side"] == true {
			sides = append(sides, v)
		}
	}
	require.Len(t, sides, 1)
	require.EqualValues(t, 0, sides[0]["turn"])
	require.EqualValues(t, 2, sides[0]["seq"], "numbered after the turn's request")
	require.Equal(t, map[string]any{"input": 300.0, "cache_read": 0.0, "cache_write": 0.0, "output": 7.0,
		"cache_write_5m": 0.0, "cache_write_1h": 0.0, "prompt_eval": 0.0, "estimated": false}, sides[0]["usage"])
	require.NotNil(t, sides[0]["cost_usd"])
}

// A title the person set is never overwritten: a chat renamed before its first
// turn makes no title request at all.
// SABOTAGE: drop the userTitled check in maybeAutoTitle -> red.
func TestChat_AutoTitleNeverOverwritesAUserTitle(t *testing.T) {
	h := newHarness(t)
	withAutoTitle(h)
	m := h.queue("a1", textStep("answer", usage), textStep("Model title", titleUsage))
	id := h.create("uid:501", "a1")
	code, body := h.do("uid:501", http.MethodPatch, "/chats/"+id, map[string]string{"title": "My own name"})
	require.Equal(t, http.StatusOK, code, string(body))
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "question"))
	s.until("turn_done")
	waitIdle(t, h.mgr.find(id, writer.String()))
	require.Len(t, m.requests(), 1, "no title request for a user-titled chat")
	require.Equal(t, "My own name", h.row("uid:501", id).Title, "nor did the first message replace it")
}

// A rename that lands while the title request is in flight wins: the answer is
// dropped. SABOTAGE: drop the userTitled re-check in autoTitle -> red.
func TestChat_ARenameDuringTheTitleRequestWins(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", textStep("answer", usage))
	asked, release := make(chan struct{}), make(chan struct{})
	h.mgr.titleStream = func(ctx context.Context, _ *chat, _ llm.Snapshot, _ llm.StreamOptions) (*llm.Response, error) {
		close(asked)
		<-release
		return &llm.Response{Blocks: []llm.Block{{Kind: llm.BlockText, Text: "Model title"}}, StopReason: llm.StopEndTurn, Usage: titleUsage}, nil
	}
	id := h.create("uid:501", "a1")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "question"))
	<-asked
	code, _ := h.do("uid:501", http.MethodPatch, "/chats/"+id, map[string]string{"title": "Mine"})
	require.Equal(t, http.StatusOK, code)
	close(release)
	c := h.mgr.find(id, writer.String())
	waitIdle(t, c)
	require.Equal(t, "Mine", h.row("uid:501", id).Title)
	// The call was still paid for: its cost is in the session.
	r := h.row("uid:501", id)
	price := h.mgr.price("anthropic", "a1")
	require.InDelta(t, price.Estimate(usage).USD+price.Estimate(titleUsage).USD, *r.CostUSD, 1e-12)
}

// A failed title request keeps the first-message name, and is not retried. A
// cut-off one (max tokens) is still paid for. An answer too long to be a title is
// not used. SABOTAGE: set the title on error, or retry -> red.
func TestChat_AutoTitleFailureKeepsTheFirstMessageName(t *testing.T) {
	for name, st := range map[string]step{
		"provider error": {err: errors.New("provider exploded")},
		"cut off":        {err: &llm.StopError{Reason: llm.StopMaxTokens, Response: &llm.Response{Blocks: []llm.Block{{Kind: llm.BlockText, Text: "A title that never"}}, Usage: titleUsage}}},
		"not a title":    textStep(strings.Repeat("word ", 30), titleUsage),
		"no text":        textStep("   \n ", titleUsage),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			withAutoTitle(h)
			m := h.queue("a1", textStep("answer", usage), st, textStep("second", usage))
			id := h.create("uid:501", "a1")
			s := h.attach("uid:501", id)
			first := "what is in alpha?"
			require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, first))
			s.until("turn_done")
			c := h.mgr.find(id, writer.String())
			waitIdle(t, c)
			require.Len(t, m.requests(), 2, "the title was asked for once")
			require.Equal(t, titleOf(first), h.row("uid:501", id).Title)
			c.mu.Lock()
			tried := c.titleTried
			c.mu.Unlock()
			require.True(t, tried)

			// No retry on the next completed turn.
			require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "again"))
			s.until("turn_done")
			waitIdle(t, c)
			require.Len(t, m.requests(), 3)
			require.Equal(t, titleOf(first), h.row("uid:501", id).Title)

			price := h.mgr.price("anthropic", "a1")
			r := h.row("uid:501", id)
			paid := 2 * price.Estimate(usage).USD
			if name != "provider error" {
				paid += price.Estimate(titleUsage).USD // a response came back: its tokens were billed
			}
			require.InDelta(t, paid, *r.CostUSD, 1e-12)
		})
	}
}

// PATCH /chats/{id} renames the chat: the answer is its row, chat_state carries
// the new title, the session log records it, and the chat is user-titled. An
// empty, blank or missing title is a 400; another principal's chat is a 404.
// SABOTAGE: drop the route, or let an empty title through -> red.
func TestChat_RenameThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	s := h.attachPage("uid:501")

	code, body := h.do("uid:501", http.MethodPatch, "/chats/"+id, map[string]string{"title": "  Rail   marks\nexplained  "})
	require.Equal(t, http.StatusOK, code, string(body))
	var got row
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, id, got.ID)
	require.Equal(t, "Rail marks explained", got.Title, "whitespace collapsed, as a first-message name is")
	untilTitle(t, s, "Rail marks explained")
	require.Equal(t, "Rail marks explained", h.row("uid:501", id).Title)
	c := h.mgr.find(id, writer.String())
	c.mu.Lock()
	user := c.userTitled
	c.mu.Unlock()
	require.True(t, user)
	var rec map[string]any
	for _, l := range readLog(t, h.logDir, id) {
		if l["type"] == "title" {
			rec = l
		}
	}
	require.Equal(t, "user", rec["source"])
	require.Equal(t, "Rail marks explained", rec["title"])

	for _, bad := range []any{map[string]string{"title": ""}, map[string]string{"title": " \n\t "}, map[string]string{}} {
		code, body := h.do("uid:501", http.MethodPatch, "/chats/"+id, bad)
		require.Equal(t, http.StatusBadRequest, code, string(body))
	}
	require.Equal(t, "Rail marks explained", h.row("uid:501", id).Title, "a refused rename changes nothing")

	code, _ = h.do("uid:1000", http.MethodPatch, "/chats/"+id, map[string]string{"title": "theirs"})
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, "Rail marks explained", h.row("uid:501", id).Title)
}

func TestCleanTitle(t *testing.T) {
	resp := func(s string) *llm.Response { return &llm.Response{Blocks: []llm.Block{{Kind: llm.BlockText, Text: s}}} }
	require.Equal(t, "Rail marks and states", cleanTitle(resp("\n  \"Rail marks and states.\"  \nsecond line")))
	require.Equal(t, "Chat titles in knomit", cleanTitle(resp("Title: Chat titles in knomit")))
	require.Equal(t, "", cleanTitle(resp(strings.Repeat("w ", 13))), "more than 12 words is not a title")
	require.Equal(t, "", cleanTitle(resp("")))
	require.Equal(t, "", cleanTitle(nil))
}
