package chat

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// row is one chat of GET /chats as the rail reads it.
type row struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	State        string   `json:"state"`
	LastActivity string   `json:"last_activity"`
	StartedAt    string   `json:"started_at"`
	Turns        int      `json:"turns"`
	CostUSD      *float64 `json:"cost_usd"`
}

func (h *harness) row(who, id string) row {
	h.t.Helper()
	code, body := h.do(who, http.MethodGet, "/chats", nil)
	require.Equal(h.t, http.StatusOK, code, string(body))
	var v struct {
		Embedded struct {
			Chats []row `json:"chats"`
		} `json:"_embedded"`
	}
	require.NoError(h.t, json.Unmarshal(body, &v))
	for _, r := range v.Embedded.Chats {
		if r.ID == id {
			return r
		}
	}
	h.t.Fatalf("chat %s not listed", id)
	return row{}
}

func TestTitleOf_CutsTheFirstMessage(t *testing.T) {
	require.Equal(t, "what changed this week?", titleOf("  what   changed\nthis week?  "))
	require.Equal(t, 300, chatTitleMax, "a generous cap: the title is cut by CSS in the UI, the server only bounds the payload")
	// A long message: cut at the last word boundary inside the cap.
	got := titleOf(strings.Repeat("abcd ", 100))
	require.Equal(t, strings.TrimSpace(strings.Repeat("abcd ", 60))+"…", got)
	// A message well past the old 60-char cut is kept whole.
	sentence := "Bind to the all lens, then reply with one short sentence containing the inline code and a table"
	require.Equal(t, sentence, titleOf(sentence))
	// No spaces: a hard cut at the cap, multi-byte safe.
	require.Equal(t, strings.Repeat("é", 300)+"…", titleOf(strings.Repeat("é", 400)))
	// Exactly the cap is not cut.
	require.Equal(t, strings.Repeat("x", 300), titleOf(strings.Repeat("x", 300)))
}

func TestChat_ListRowFieldsAndStateTransitions(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000001", "alpha")
	h.queue("a1",
		toolStep("tu_w1", "knomit_learn", learnArgs(handle, "Alpha fact by chat"), usage),
		textStep("written", usage),
		step{err: errors.New("provider exploded")},
	)
	id := h.create("uid:501", "a1")

	r := h.row("uid:501", id)
	require.Equal(t, "", r.Title)
	require.Equal(t, "idle", r.State)
	require.Equal(t, 0, r.Turns)
	require.Nil(t, r.CostUSD)
	require.NotEmpty(t, r.StartedAt)
	require.NotEmpty(t, r.LastActivity)

	s := h.attach("uid:501", id)
	first := "write a fact about alpha " + strings.Repeat("and more words ", 10)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, first))
	s.until("approval")
	r = h.row("uid:501", id)
	require.Equal(t, "awaiting_approval", r.State, "a waiting call outranks the running turn")
	require.Equal(t, titleOf(first), r.Title, "the title is the first message, cut, set when the turn is posted")
	require.Equal(t, 1, r.Turns)

	require.Equal(t, http.StatusNoContent, h.approve("uid:501", id, "tu_w1", "allow"))
	s.until("turn_done")
	r = h.row("uid:501", id)
	require.Equal(t, "idle", r.State)
	require.Equal(t, 1, r.Turns)
	require.Equal(t, titleOf(first), r.Title)

	// A second turn that fails: error, and the title does not move.
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "second message"))
	s.until("turn_error")
	r = h.row("uid:501", id)
	require.Equal(t, "error", r.State)
	require.Equal(t, 2, r.Turns)
	require.Equal(t, titleOf(first), r.Title)
}

func TestChat_ListRowRunningAndStoppedIsNotAnError(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", step{block: true})
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "think forever"))
	require.Equal(t, "running", h.row("uid:501", id).State)
	code, _ := h.do("uid:501", http.MethodPost, "/chats/"+id+"/cancel", nil)
	require.Equal(t, http.StatusNoContent, code)
	s.until("turn_error")
	require.Equal(t, "idle", h.row("uid:501", id).State, "the person's own Stop is not a failure")
}

func TestChat_ListRowCostIsTheSumOfPricedTurns(t *testing.T) {
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	a, b := 0.25, 0.5
	c.mu.Lock()
	c.view = []viewItem{{Kind: "user", Text: "x", CostUSD: &a}, {Kind: "assistant", Text: "y"}, {Kind: "user", Text: "z", CostUSD: &b}}
	c.mu.Unlock()
	r := h.row("uid:501", id)
	require.Equal(t, 2, r.Turns)
	require.NotNil(t, r.CostUSD)
	require.InDelta(t, 0.75, *r.CostUSD, 1e-9)
}

// The chat_state event and the GET /chats row are one rowState: after the turn
// they agree on title, state, turns and started_at, and the title is the cut
// first message.
func TestChat_ChatStateEventMatchesTheListRow(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", textStep("hi", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	first := "tell me about the rail " + strings.Repeat("and its marks ", 10)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, first))
	var ev rowState
	for {
		e := s.next("state", 10*time.Second)
		if e.Name != "chat_state" {
			continue
		}
		require.NoError(t, json.Unmarshal(e.Data, &ev))
		if ev.State == "idle" {
			break
		}
	}
	r := h.row("uid:501", id)
	require.Equal(t, titleOf(first), ev.Title)
	require.Equal(t, ev.Title, r.Title)
	require.Equal(t, ev.State, r.State)
	require.Equal(t, ev.Turns, r.Turns)
	require.Equal(t, ev.StartedAt, r.StartedAt)
	require.NotEmpty(t, ev.LastActivity)
	require.NotEmpty(t, r.LastActivity)
}
