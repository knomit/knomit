package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

// sendTurn posts a turn that names its send.
func (h *harness) sendTurn(who, id, text, sendID string) int {
	h.t.Helper()
	code, _ := h.do(who, http.MethodPost, "/chats/"+id+"/turns", map[string]string{"text": text, "send_id": sendID})
	return code
}

func (h *harness) cancelSend(who, id, sendID string) int {
	h.t.Helper()
	code, _ := h.do(who, http.MethodPost, "/chats/"+id+"/cancel", map[string]string{"send_id": sendID})
	return code
}

type snapshotView struct {
	Running              bool  `json:"running"`
	PromptTokens         int64 `json:"prompt_tokens"`
	ExpectedFirstTokenMS int64 `json:"expected_first_token_ms"`
	Items                []struct {
		Kind        string               `json:"kind"`
		SendID      string               `json:"send_id"`
		Text        string               `json:"text"`
		ID          string               `json:"id"`
		DurationMS  int64                `json:"duration_ms"`
		Summary     *struct{ Count int } `json:"summary"`
		Warning     string               `json:"warning"`
		WarningInfo map[string]any       `json:"warning_info"`
		Error       string               `json:"error"`
		ErrorInfo   map[string]any       `json:"error_info"`
	} `json:"items"`
}

func (h *harness) snapshot(who, id string) snapshotView {
	h.t.Helper()
	code, body := h.do(who, http.MethodGet, "/chats/"+id, nil)
	require.Equal(h.t, http.StatusOK, code, string(body))
	var v snapshotView
	require.NoError(h.t, json.Unmarshal(body, &v))
	return v
}

// Cancel before accept: a Stop that overtakes its send withdraws it.
func TestChat_CancelBeforeAcceptWithdrawsTheSend(t *testing.T) {
	h := newHarness(t)
	m := h.queue("a1", textStep("never asked", usage), textStep("second", usage))
	id := h.create("uid:501", "a1")

	require.Equal(t, http.StatusNoContent, h.cancelSend("uid:501", id, "send-1"))

	// The send arrives after its Stop: refused, no turn, the model never called.
	code, body := h.do("uid:501", http.MethodPost, "/chats/"+id+"/turns", map[string]string{"text": "hello", "send_id": "send-1"})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Contains(t, string(body), "Send withdrawn")
	snap := h.snapshot("uid:501", id)
	require.Empty(t, snap.Items, "a withdrawn send leaves no user item")
	require.False(t, snap.Running)
	require.Empty(t, m.requests(), "the model was never called")

	// The chat is not wedged, and a different send goes through.
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.sendTurn("uid:501", id, "again", "send-2"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	require.Equal(t, "turn_done", evs[len(evs)-1].Name)
	require.Len(t, m.requests(), 1)
	require.Equal(t, "user", h.snapshot("uid:501", id).Items[0].Kind)
}

// Cancel for a send that is already running ends that turn, and only that one.
func TestChat_CancelWithSendIDEndsOnlyThatSendsTurn(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", step{block: true})
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.sendTurn("uid:501", id, "think", "send-1"))

	// Another send's id does not touch the running turn.
	require.Equal(t, http.StatusNoContent, h.cancelSend("uid:501", id, "send-9"))
	time.Sleep(50 * time.Millisecond)
	require.True(t, h.snapshot("uid:501", id).Running, "a stranger's send id leaves the turn running")

	require.Equal(t, http.StatusNoContent, h.cancelSend("uid:501", id, "send-1"))
	evs := s.until("turn_error")
	var e struct {
		Cancelled bool `json:"cancelled"`
	}
	require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &e))
	require.True(t, e.Cancelled)
}

// A Stop for a send whose turn already finished must not poison later sends.
func TestChat_CancelOfAFinishedSendIsHarmless(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", textStep("one", usage), textStep("two", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.sendTurn("uid:501", id, "q1", "send-1"))
	drive(t, h, "uid:501", id, s, nil)

	require.Equal(t, http.StatusNoContent, h.cancelSend("uid:501", id, "send-1"))
	require.Equal(t, http.StatusAccepted, h.sendTurn("uid:501", id, "q2", "send-2"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	require.Equal(t, "turn_done", evs[len(evs)-1].Name)
}

// Without a send id, cancel is what it always was.
func TestChat_CancelWithoutBodyStillEndsTheRunningTurn(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", step{block: true})
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "think"))
	code, _ := h.do("uid:501", http.MethodPost, "/chats/"+id+"/cancel", nil)
	require.Equal(t, http.StatusNoContent, code)
	s.until("turn_error")
}

// Live tool_call events carry the arguments; done carries duration and summary.
func TestChat_LiveToolCallCarriesArgumentsDurationAndSummary(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000071", "alpha")
	h.queue("a1",
		toolStep("tu_q", "knomit_query", map[string]any{"binding": handle, "text": "OWASP Q3"}, usage),
		textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "search"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)

	var running, done map[string]any
	for _, e := range evs {
		if e.Name != "tool_call" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal(e.Data, &m))
		if m["state"] == "running" {
			running = m
		} else {
			done = m
		}
	}
	require.NotNil(t, running)
	args, ok := running["arguments"].(map[string]any)
	require.True(t, ok, "the running event carries arguments: %v", running)
	require.Equal(t, "OWASP Q3", args["text"])
	require.Equal(t, handle, args["binding"])

	require.NotNil(t, done)
	require.Greater(t, done["duration_ms"], float64(0), "the done event carries how long the tool ran")
	sum, ok := done["summary"].(map[string]any)
	require.True(t, ok, "the done event carries a summary: %v", done)
	var res struct {
		Facts []json.RawMessage `json:"facts"`
	}
	require.NoError(t, json.Unmarshal([]byte(callResult(t, h, "uid:501", id, "tu_q").Result), &res))
	require.Equal(t, float64(len(res.Facts)), sum["count"], "the summary is the result's own fact count")

	// The snapshot carries the same, so a page that joins late shows it too.
	snap := h.snapshot("uid:501", id)
	found := false
	for _, it := range snap.Items {
		if it.ID == "tu_q" {
			found = true
			require.Greater(t, it.DurationMS, int64(0))
			require.NotNil(t, it.Summary)
			require.Equal(t, len(res.Facts), it.Summary.Count)
		}
	}
	require.True(t, found)
}

// A call that did not run (declined) has no duration.
func TestChat_DeclinedCallCarriesNoDuration(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000072", "alpha")
	h.queue("a1",
		toolStep("tu_w", "knomit_learn", learnArgs(handle, "Declined"), usage),
		textStep("ok", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write"))
	evs, _ := drive(t, h, "uid:501", id, s, map[string]string{"tu_w": "decline"})
	for _, e := range evs {
		if e.Name != "tool_call" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal(e.Data, &m))
		if m["state"] == "done" {
			require.NotContains(t, m, "duration_ms")
			require.NotContains(t, m, "summary")
		}
	}
}

// thinking_delta events stream the model's reasoning text, in order, on the turn.
func TestChat_ThinkingDeltasAreStreamed(t *testing.T) {
	h := newHarness(t)
	st := textStep("answer", usage)
	st.thinking = []string{"Comparing ", "blast radius"}
	h.queue("a1", st)
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "q"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	var got []string
	for _, e := range evs {
		if e.Name == "thinking_delta" {
			var d struct {
				Text string  `json:"text"`
				Turn float64 `json:"turn"`
			}
			require.NoError(t, json.Unmarshal(e.Data, &d))
			require.Equal(t, 0.0, d.Turn, "the turn is the view index of its user item")
			got = append(got, d.Text)
		}
	}
	require.Equal(t, []string{"Comparing ", "blast radius"}, got)
}

// The structured warning carries the server's numbers, in the event and in the snapshot.
func TestChat_PriceWarningIsStructured(t *testing.T) {
	h := newHarness(t)
	big := llm.Usage{Input: 60000, CacheRead: 30001, CacheWrite: 7003, Output: 5} // prompt 97004
	h.queue("claude-test-x", textStep("one", big))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "first"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	e, ok := find(evs, "warning")
	require.True(t, ok)
	var w map[string]any
	require.NoError(t, json.Unmarshal(e.Data, &w))
	require.Equal(t, "price_tier", w["kind"])
	require.EqualValues(t, 97004, w["prompt_tokens"])
	require.EqualValues(t, 95000, w["threshold"])
	require.EqualValues(t, 100000, w["tier_tokens"])
	require.EqualValues(t, 5, w["mult"], "tier 2 input 0.5 over tier 1 input 0.1")

	snap := h.snapshot("uid:501", id)
	info := snap.Items[0].WarningInfo
	require.Equal(t, "price_tier", info["kind"])
	require.EqualValues(t, 97004, info["prompt_tokens"])
	require.EqualValues(t, 5, info["mult"])
	require.NotEmpty(t, snap.Items[0].Warning, "the worded sentence stays for the title")
}

// A context-full end carries needed and window next to the message.
func TestChat_ContextFullErrorIsStructured(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", step{err: fmt.Errorf("provider said no: %w", llm.ErrContextFull)})
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "huge"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	e, ok := find(evs, "turn_error")
	require.True(t, ok)
	var te struct {
		Message   string         `json:"message"`
		ErrorInfo map[string]any `json:"error_info"`
	}
	require.NoError(t, json.Unmarshal(e.Data, &te))
	require.Equal(t, "context_full", te.ErrorInfo["kind"])
	require.EqualValues(t, 1_000_000, te.ErrorInfo["window"], "the model's window")
	require.Contains(t, te.Message, "no longer fits the model's context window")
	snap := h.snapshot("uid:501", id)
	require.Equal(t, "context_full", snap.Items[0].ErrorInfo["kind"])
	require.EqualValues(t, 1_000_000, snap.Items[0].ErrorInfo["window"])
}

// The context meter reads the LAST request's prompt, not a sum.
func TestChat_PromptTokensAreTheLastRequestsPrompt(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000073", "alpha")
	h.queue("a1",
		toolStep("tu_q", "knomit_query", map[string]any{"binding": handle, "text": "x"}, llm.Usage{Input: 100, CacheRead: 20, CacheWrite: 3, Output: 1}),
		textStep("done", llm.Usage{Input: 400, CacheRead: 50, CacheWrite: 7, Output: 2}))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "q"))
	drive(t, h, "uid:501", id, s, nil)

	require.EqualValues(t, 457, h.snapshot("uid:501", id).PromptTokens, "the second request's prompt: 400 + 50 + 7")
	code, body := h.do("uid:501", http.MethodGet, "/chats/"+id+"/context", nil)
	require.Equal(t, http.StatusOK, code)
	var cv struct {
		PromptTokens int64 `json:"prompt_tokens"`
	}
	require.NoError(t, json.Unmarshal(body, &cv))
	require.EqualValues(t, 457, cv.PromptTokens)
}

// The expected first-token time is the slowest this run has seen for the model.
func TestChat_ExpectedFirstTokenIsTheSlowestObserved(t *testing.T) {
	h := newHarness(t)
	slow := func(d time.Duration) step {
		return step{fn: func(llm.Snapshot) step {
			time.Sleep(d)
			return textStep("ok", usage)
		}}
	}
	h.queue("a1", slow(120*time.Millisecond), slow(5*time.Millisecond))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)

	providerMS := func() int64 {
		code, body := h.do("uid:501", http.MethodGet, "/chat-providers", nil)
		require.Equal(t, http.StatusOK, code)
		var v struct {
			Embedded struct {
				Providers []struct {
					Models []struct {
						ID string `json:"id"`
						MS int64  `json:"expected_first_token_ms"`
					} `json:"models"`
				} `json:"providers"`
			} `json:"_embedded"`
		}
		require.NoError(t, json.Unmarshal(body, &v))
		for _, p := range v.Embedded.Providers {
			for _, m := range p.Models {
				if m.ID == "a1" {
					return m.MS
				}
			}
		}
		t.Fatal("a1 not listed")
		return 0
	}
	require.EqualValues(t, 0, providerMS(), "nothing observed yet: no value")
	require.EqualValues(t, 0, h.snapshot("uid:501", id).ExpectedFirstTokenMS)

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "slow"))
	drive(t, h, "uid:501", id, s, nil)
	first := h.snapshot("uid:501", id).ExpectedFirstTokenMS
	require.GreaterOrEqual(t, first, int64(120))
	require.Less(t, first, int64(2000))
	require.Equal(t, first, providerMS())

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "fast"))
	drive(t, h, "uid:501", id, s, nil)
	require.Equal(t, first, h.snapshot("uid:501", id).ExpectedFirstTokenMS, "a faster reply does not lower the slowest")
}

// The snapshot's user item carries the send id of the POST that started it: the
// exact identity the page matches its own send by. Another send's turn, and a
// turn that came without an id, do not carry ours.
func TestChat_SnapshotUserItemCarriesItsSendID(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", textStep("one", usage), textStep("two", usage), textStep("three", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.sendTurn("uid:501", id, "from this tab", "tab-A-1"))
	drive(t, h, "uid:501", id, s, nil)
	require.Equal(t, http.StatusAccepted, h.sendTurn("uid:501", id, "from another tab", "tab-B-7"))
	drive(t, h, "uid:501", id, s, nil)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "no id at all"))
	drive(t, h, "uid:501", id, s, nil)

	var got [][2]string
	for _, it := range h.snapshot("uid:501", id).Items {
		if it.Kind == "user" {
			got = append(got, [2]string{it.Text, it.SendID})
		}
	}
	require.Equal(t, [][2]string{{"from this tab", "tab-A-1"}, {"from another tab", "tab-B-7"}, {"no id at all", ""}}, got)
}

// A send withdrawn by a timeout's cancel, whose POST then arrives late, starts
// no turn: the snapshot never lists its id, so the page's restored text is the
// only copy.
func TestChat_LateArrivingPostAfterATimeoutCancelLeavesNoTurn(t *testing.T) {
	h := newHarness(t)
	m := h.queue("a1", textStep("never", usage))
	id := h.create("uid:501", "a1")
	require.Equal(t, http.StatusNoContent, h.cancelSend("uid:501", id, "late-1"))
	require.Equal(t, http.StatusConflict, h.sendTurn("uid:501", id, "late text", "late-1"))
	snap := h.snapshot("uid:501", id)
	require.Empty(t, snap.Items)
	require.Empty(t, m.requests())
}

// A second Stop for the same send changes nothing: one entry, still withdrawn once.
func TestChat_DuplicateCancelForOneSendIsHarmless(t *testing.T) {
	h := newHarness(t)
	m := h.queue("a1", textStep("after", usage))
	id := h.create("uid:501", "a1")
	require.Equal(t, http.StatusNoContent, h.cancelSend("uid:501", id, "dup"))
	require.Equal(t, http.StatusNoContent, h.cancelSend("uid:501", id, "dup"))
	c := h.mgr.find(id, writer.String())
	c.mu.Lock()
	require.Equal(t, []string{"dup"}, c.sends, "one entry for two cancels")
	require.Equal(t, "withdrawn", c.sendState["dup"])
	c.mu.Unlock()

	require.Equal(t, http.StatusConflict, h.sendTurn("uid:501", id, "x", "dup"), "still withdrawn")
	require.Empty(t, m.requests())
	// A cancel after the refusal still leaves other sends alone.
	require.Equal(t, http.StatusNoContent, h.cancelSend("uid:501", id, "dup"))
	require.Equal(t, http.StatusAccepted, h.sendTurn("uid:501", id, "y", "other"))
}

// The per-chat memory of send ids is bounded: the oldest is forgotten after
// maxSends newer ones, and a forgotten withdrawn id no longer withdraws.
func TestChat_SendIDMemoryIsBoundedAndEvictsTheOldest(t *testing.T) {
	cancelMany := func(h *harness, id string, n int) {
		for i := 0; i < n; i++ {
			require.Equal(t, http.StatusNoContent, h.cancelSend("uid:501", id, fmt.Sprintf("n-%d", i)))
		}
	}
	// maxSends-1 newer ids: "first" is the oldest of exactly maxSends, still remembered.
	h := newHarness(t)
	h.queue("a1", textStep("ok", usage))
	id := h.create("uid:501", "a1")
	require.Equal(t, http.StatusNoContent, h.cancelSend("uid:501", id, "first"))
	cancelMany(h, id, maxSends-1)
	c := h.mgr.find(id, writer.String())
	c.mu.Lock()
	require.Len(t, c.sends, maxSends)
	require.Len(t, c.sendState, maxSends)
	c.mu.Unlock()
	require.Equal(t, http.StatusConflict, h.sendTurn("uid:501", id, "x", "first"), "at the bound it is still withdrawn")

	// One more newer id evicts "first": it is no longer withdrawn, and the memory stays at the bound.
	h2 := newHarness(t)
	h2.queue("a1", textStep("ok", usage))
	id2 := h2.create("uid:501", "a1")
	require.Equal(t, http.StatusNoContent, h2.cancelSend("uid:501", id2, "first"))
	cancelMany(h2, id2, maxSends)
	c2 := h2.mgr.find(id2, writer.String())
	c2.mu.Lock()
	require.Len(t, c2.sends, maxSends, "never more than maxSends")
	require.Len(t, c2.sendState, maxSends)
	require.NotContains(t, c2.sendState, "first", "the oldest was forgotten")
	require.Contains(t, c2.sendState, fmt.Sprintf("n-%d", maxSends-1), "the newest is kept")
	c2.mu.Unlock()
	require.Equal(t, http.StatusAccepted, h2.sendTurn("uid:501", id2, "x", "first"), "a forgotten withdrawn id starts its turn")
}
