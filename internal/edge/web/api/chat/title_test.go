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

// turnsDone posts each message as a turn and waits for it to end, and for any
// side call it started, before the next.
func turnsDone(t *testing.T, h *harness, id string, s *stream, msgs ...string) {
	t.Helper()
	c := h.mgr.find(id, writer.String())
	for _, msg := range msgs {
		require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, msg))
		s.until("turn_done")
		waitIdle(t, c)
	}
}

// The three turns most tests run: the setup turn first, then the real work.
var threeMsgs = []string{
	"bind to alpha",
	"how does the rail mark a chat's state?",
	"and what does the error mark mean?",
}

// threeAnswers are the scripted answers to threeMsgs.
func threeAnswers() []step {
	return []step{textStep("Bound to alpha.", usage), textStep("A coloured provider mark per state.", usage), textStep("The last turn failed.", usage)}
}

// The title is made once, after the THIRD completed turn (people bind first and
// only then start the real work): no request after turns 1 and 2, exactly one
// after turn 3, none after turn 4. The request carries the conversation so far
// (all three user messages and the answers), no tools, no system, then the
// prompt; its answer replaces the first-message name, goes out as chat_state, is
// logged, and its cost is added to the third turn (so to the session cost).
// SABOTAGE: set titleAfterTurns to 1 or 2, drop maybeAutoTitle from finishTurn,
// or drop addSideUsage -> red.
func TestChat_AutoTitleAfterTheThirdCompletedTurn(t *testing.T) {
	h := newHarness(t)
	opts := withAutoTitle(h)
	steps := threeAnswers()
	m := h.queue("a1", steps[0], steps[1])
	id := h.create("uid:501", "a1")
	s := h.attachPage("uid:501")
	c := h.mgr.find(id, writer.String())

	turnsDone(t, h, id, s, threeMsgs[0])
	require.Len(t, m.requests(), 1, "no title request after turn 1")
	turnsDone(t, h, id, s, threeMsgs[1])
	require.Len(t, m.requests(), 2, "no title request after turn 2")
	require.Equal(t, titleOf(threeMsgs[0]), h.row("uid:501", id).Title)

	m.mu.Lock()
	m.steps = append(m.steps, steps[2], textStep("\"Rail marks and chat states.\"\n", titleUsage))
	m.mu.Unlock()
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, threeMsgs[2]))
	st := untilTitle(t, s, "Rail marks and chat states")
	waitIdle(t, c)
	reqs := m.requests()
	require.Len(t, reqs, 4, "three turns and exactly ONE title request")
	r := h.row("uid:501", id)
	require.Equal(t, "Rail marks and chat states", r.Title, "the quotes and the full stop are trimmed")
	require.Equal(t, st.Title, r.Title)

	// The request: every user message of the three turns and their answers, in
	// order, then the prompt; no tools, no system.
	tr := reqs[3]
	require.Empty(t, tr.Hooks[llm.HookTools])
	require.Empty(t, tr.Hooks[llm.HookSystem])
	var users, answers []string
	for _, it := range tr.Hooks[llm.HookConversation] {
		switch it.Entry.Role {
		case llm.RoleUser:
			users = append(users, it.Entry.Blocks[0].Text)
		case llm.RoleAssistant:
			answers = append(answers, it.Entry.Blocks[0].Text)
		}
	}
	require.Equal(t, threeMsgs, users)
	require.Equal(t, []string{"Bound to alpha.", "A coloured provider mark per state.", "The last turn failed."}, answers)
	require.Len(t, tr.Hooks[llm.HookTurn], 1)
	require.Equal(t, titlePrompt, tr.Hooks[llm.HookTurn][0].Entry.Blocks[0].Text)
	require.Equal(t, []llm.StreamOptions{{MaxOutput: titleMaxTokens, NoThinking: true}}, *opts)
	require.Equal(t, 32, titleMaxTokens)
	require.Equal(t, 3, titleAfterTurns)

	// The cost: the three turns plus the title request, the title's on turn 3.
	price := h.mgr.price("anthropic", "a1")
	require.Greater(t, price.Estimate(titleUsage).USD, 0.0)
	require.InDelta(t, 3*price.Estimate(usage).USD+price.Estimate(titleUsage).USD, *r.CostUSD, 1e-12)
	c.mu.Lock()
	third := c.turnAt
	u0, u3 := *c.view[0].Usage, *c.view[third].Usage
	c.mu.Unlock()
	require.Equal(t, usage.Input, u0.Input, "turn 1 is not charged for the title")
	require.Equal(t, usage.Input+titleUsage.Input, u3.Input, "the third turn is")

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
	require.EqualValues(t, third, title["turn"])

	// Once per chat: a fourth completed turn asks for no title.
	m.mu.Lock()
	m.steps = append(m.steps, textStep("more", usage))
	m.mu.Unlock()
	turnsDone(t, h, id, s, "and the idle mark?")
	require.Len(t, m.requests(), 5, "turn 4 and no second title request")
	require.Equal(t, "Rail marks and chat states", h.row("uid:501", id).Title)
}

// Only turns that finish normally count: a turn that errors, or that the person
// stops, does not, so the title waits for a third COMPLETED one. A chat with
// fewer than three completed turns keeps its first-message name.
// SABOTAGE: count every turn's end (move the call before the err check) -> red.
func TestChat_AutoTitleCountsOnlyCompletedTurns(t *testing.T) {
	h := newHarness(t)
	withAutoTitle(h)
	m := h.queue("a1",
		textStep("one", usage),
		step{err: errors.New("provider exploded")},
		textStep("two", usage),
		step{block: true},
	)
	id := h.create("uid:501", "a1")
	s := h.attachPage("uid:501")
	c := h.mgr.find(id, writer.String())
	turnsDone(t, h, id, s, "bind to alpha")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "this one fails"))
	s.until("turn_error")
	waitIdle(t, c)
	turnsDone(t, h, id, s, "second completed")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "this one is stopped"))
	time.Sleep(50 * time.Millisecond)
	code, _ := h.do("uid:501", http.MethodPost, "/chats/"+id+"/cancel", nil)
	require.Equal(t, http.StatusNoContent, code)
	s.until("turn_error")
	waitIdle(t, c)
	require.Len(t, m.requests(), 4, "four turns ended, two completed: no title request yet")
	require.Equal(t, titleOf("bind to alpha"), h.row("uid:501", id).Title)
	c.mu.Lock()
	done, tried := c.turnsDone, c.titleTried
	c.mu.Unlock()
	require.Equal(t, 2, done)
	require.False(t, tried)

	// The third COMPLETED turn (the fifth sent) names it.
	m.mu.Lock()
	m.steps = append(m.steps, textStep("three", usage), textStep("Alpha rail marks", titleUsage))
	m.mu.Unlock()
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "third completed"))
	untilTitle(t, s, "Alpha rail marks")
	waitIdle(t, c)
	require.Len(t, m.requests(), 6)
	// The failed and the stopped turns' messages are part of the conversation too.
	var users []string
	for _, it := range m.requests()[5].Hooks[llm.HookConversation] {
		if it.Entry.Role == llm.RoleUser {
			users = append(users, it.Entry.Blocks[0].Text)
		}
	}
	require.Equal(t, []string{"bind to alpha", "this one fails", "second completed", "this one is stopped", "third completed"}, users)
}

// The conversation the request carries is bounded: user messages first, each
// cut to titleMessageRunes, then the answers in what is left of
// titleContextRunes. SABOTAGE: drop the cuts -> red.
func TestChat_TitleSnapshotIsBounded(t *testing.T) {
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	long := strings.Repeat("x", 5000)
	count := func(turns int) (users, answers, total int) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.view = nil
		for i := 0; i < turns; i++ {
			c.view = append(c.view, viewItem{Kind: "user", Text: long}, viewItem{Kind: "assistant", Text: long})
		}
		s, ok := c.titleSnapshotLocked(len(c.view) - 2)
		require.True(t, ok)
		for _, it := range s.Hooks[llm.HookConversation] {
			n := len([]rune(it.Entry.Blocks[0].Text))
			require.LessOrEqual(t, n, titleMessageRunes)
			total += n
			if it.Entry.Role == llm.RoleUser {
				users++
			} else {
				answers++
			}
		}
		return
	}
	// Three long turns: every message at its cap, 3600 of the 4000.
	u, a, total := count(3)
	require.Equal(t, 3, u)
	require.Equal(t, 3, a)
	require.Equal(t, 6*titleMessageRunes, total)
	// Eight long turns: the user messages fill the budget first (6 x 600 + 400)
	// and no answer fits.
	u, a, total = count(8)
	require.Equal(t, 7, u)
	require.Equal(t, 0, a)
	require.Equal(t, titleContextRunes, total)
}

// The usage event of the title request says it is a side request, so the page
// adds its cost to the turn without taking its prompt for the context meter.
func TestChat_AutoTitleUsageEventIsMarkedSide(t *testing.T) {
	h := newHarness(t)
	withAutoTitle(h)
	h.queue("a1", append(threeAnswers(), textStep("A short title", titleUsage))...)
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	for _, msg := range threeMsgs {
		require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, msg))
		s.until("turn_done")
	}
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
	require.EqualValues(t, 4, sides[0]["turn"], "the third turn's view index")
	require.EqualValues(t, 4, sides[0]["seq"], "numbered after the three turns' requests")
	require.Equal(t, map[string]any{"input": 300.0, "cache_read": 0.0, "cache_write": 0.0, "output": 7.0,
		"cache_write_5m": 0.0, "cache_write_1h": 0.0, "prompt_eval": 0.0, "estimated": false}, sides[0]["usage"])
	require.NotNil(t, sides[0]["cost_usd"])
}

// A title the person set is never overwritten: a chat renamed before its turns
// makes no title request at all, even after three.
// SABOTAGE: drop the userTitled check in maybeAutoTitle -> red.
func TestChat_AutoTitleNeverOverwritesAUserTitle(t *testing.T) {
	h := newHarness(t)
	withAutoTitle(h)
	m := h.queue("a1", append(threeAnswers(), textStep("Model title", titleUsage))...)
	id := h.create("uid:501", "a1")
	code, body := h.do("uid:501", http.MethodPatch, "/chats/"+id, map[string]string{"title": "My own name"})
	require.Equal(t, http.StatusOK, code, string(body))
	s := h.attach("uid:501", id)
	turnsDone(t, h, id, s, threeMsgs...)
	require.Len(t, m.requests(), 3, "no title request for a user-titled chat")
	require.Equal(t, "My own name", h.row("uid:501", id).Title, "nor did the first message replace it")
}

// A rename that lands while the title request is in flight wins: the answer is
// dropped. SABOTAGE: drop the userTitled re-check in autoTitle -> red.
func TestChat_ARenameDuringTheTitleRequestWins(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", threeAnswers()...)
	asked, release := make(chan struct{}), make(chan struct{})
	h.mgr.titleStream = func(ctx context.Context, _ *chat, _ llm.Snapshot, _ llm.StreamOptions) (*llm.Response, error) {
		close(asked)
		<-release
		return &llm.Response{Blocks: []llm.Block{{Kind: llm.BlockText, Text: "Model title"}}, StopReason: llm.StopEndTurn, Usage: titleUsage}, nil
	}
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	for _, msg := range threeMsgs {
		require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, msg))
		s.until("turn_done")
	}
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
	require.InDelta(t, 3*price.Estimate(usage).USD+price.Estimate(titleUsage).USD, *r.CostUSD, 1e-12)
}

// A failed title request keeps the first-message name, and is not retried. A
// cut-off one (max tokens) is still paid for. An answer too long to be a title is
// not used. SABOTAGE: set the title on error, or retry -> red.
func TestChat_AutoTitleFailureKeepsTheFirstMessageName(t *testing.T) {
	for name, st := range map[string]step{
		"provider error":   {err: errors.New("provider exploded")},
		"cut off":          {err: &llm.StopError{Reason: llm.StopMaxTokens, Response: &llm.Response{Blocks: []llm.Block{{Kind: llm.BlockText, Text: "A title that never"}}, Usage: titleUsage}}},
		"cut off mid-line": {err: &llm.StopError{Reason: llm.StopMaxTokens, Response: &llm.Response{Blocks: []llm.Block{{Kind: llm.BlockText, Text: "Latest cyberai facts and their sour"}}, Usage: titleUsage}}},
		"not a title":      textStep(strings.Repeat("word ", 30), titleUsage),
		"no text":          textStep("   \n ", titleUsage),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			withAutoTitle(h)
			m := h.queue("a1", append(threeAnswers(), st, textStep("fourth", usage))...)
			id := h.create("uid:501", "a1")
			s := h.attach("uid:501", id)
			turnsDone(t, h, id, s, threeMsgs...)
			c := h.mgr.find(id, writer.String())
			require.Len(t, m.requests(), 4, "the title was asked for once")
			require.Equal(t, titleOf(threeMsgs[0]), h.row("uid:501", id).Title)
			c.mu.Lock()
			tried := c.titleTried
			c.mu.Unlock()
			require.True(t, tried)

			// No retry on the next completed turn.
			turnsDone(t, h, id, s, "again")
			require.Len(t, m.requests(), 5)
			require.Equal(t, titleOf(threeMsgs[0]), h.row("uid:501", id).Title)

			price := h.mgr.price("anthropic", "a1")
			r := h.row("uid:501", id)
			paid := 4 * price.Estimate(usage).USD
			if name != "provider error" {
				paid += price.Estimate(titleUsage).USD // a response came back: its tokens were billed
			}
			require.InDelta(t, paid, *r.CostUSD, 1e-12)
		})
	}
}

// A small model names the chat on its first line and keeps going until the
// budget cuts it off (lfm3, seen live): a first line the model ended is the title.
// SABOTAGE: drop the firstLineEnded rescue in autoTitle -> red.
func TestChat_AutoTitleCutOffAfterAWholeFirstLineIsUsed(t *testing.T) {
	h := newHarness(t)
	withAutoTitle(h)
	cut := step{err: &llm.StopError{Reason: llm.StopMaxTokens, Response: &llm.Response{
		Blocks: []llm.Block{{Kind: llm.BlockText, Text: "CyberAI KB Latest Facts  \nAI Agent Updates  \nEmer"}}, Usage: titleUsage}}}
	h.queue("a1", append(threeAnswers(), cut)...)
	id := h.create("uid:501", "a1")
	s := h.attachPage("uid:501")
	for _, msg := range threeMsgs {
		require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, msg))
		s.until("turn_done")
	}
	untilTitle(t, s, "CyberAI KB Latest Facts")
	waitIdle(t, h.mgr.find(id, writer.String()))
	require.Equal(t, "CyberAI KB Latest Facts", h.row("uid:501", id).Title)
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
	resp := func(s string) *llm.Response {
		return &llm.Response{Blocks: []llm.Block{{Kind: llm.BlockText, Text: s}}}
	}
	require.Equal(t, "Rail marks and states", cleanTitle(resp("\n  \"Rail marks and states.\"  \nsecond line")))
	require.Equal(t, "Chat titles in knomit", cleanTitle(resp("Title: Chat titles in knomit")))
	require.Equal(t, "", cleanTitle(resp(strings.Repeat("w ", 13))), "more than 12 words is not a title")
	require.Equal(t, "", cleanTitle(resp("")))
	require.Equal(t, "", cleanTitle(nil))
}
