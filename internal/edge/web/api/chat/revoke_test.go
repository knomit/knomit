package chat

import (
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

// Revoking "Allow for session": DELETE /chats/{id}/grants names one grant as
// the listing shows it, and only that grant goes.

func grantsOf(t *testing.T, h *harness, who, id string) []grantInfo {
	t.Helper()
	code, body := h.do(who, http.MethodGet, "/chats/"+id+"/grants", nil)
	require.Equal(t, http.StatusOK, code, string(body))
	var v struct {
		Embedded struct {
			Grants []grantInfo `json:"grants"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(body, &v))
	return v.Embedded.Grants
}

// SABOTAGE: chat.go revokeGrant, make it return true without deleting -> the
// second turn's alpha write is not asked and the test fails.
func TestChat_RevokedGrantAsksAgainAndLeavesTheOtherInForce(t *testing.T) {
	h := newHarness(t)
	a := h.bind(writer, "chat-00000000000000000000000000000051", "alpha")
	b := h.bind(writer, "chat-00000000000000000000000000000052", "beta")
	h.queue("a1",
		toolStep("tu_1", "knomit_learn", learnArgs(a, "Alpha one"), usage),
		toolStep("tu_2", "knomit_learn", learnArgs(b, "Beta one"), usage),
		textStep("first done", usage),
		toolStep("tu_3", "knomit_learn", learnArgs(a, "Alpha two"), usage),
		toolStep("tu_4", "knomit_learn", learnArgs(b, "Beta two"), usage),
		textStep("second done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "one"))
	_, asked := drive(t, h, "uid:501", id, s, map[string]string{"tu_1": "allow_session", "tu_2": "allow_session"})
	require.Equal(t, []string{"tu_1", "tu_2"}, approvedIDs(asked))
	require.Equal(t, []grantInfo{
		{Target: "alpha", Kind: "repo", Tool: "knomit_learn", Action: "learn"},
		{Target: "beta", Kind: "repo", Tool: "knomit_learn", Action: "learn"},
	}, grantsOf(t, h, "uid:501", id))

	// Revoke alpha's grant only.
	code, body := h.do("uid:501", http.MethodDelete, "/chats/"+id+"/grants",
		grantInfo{Target: "alpha", Kind: "repo", Tool: "knomit_learn", Action: "learn"})
	require.Equal(t, http.StatusNoContent, code, string(body))
	require.Equal(t, []grantInfo{{Target: "beta", Kind: "repo", Tool: "knomit_learn", Action: "learn"}}, grantsOf(t, h, "uid:501", id))

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "two"))
	_, asked = drive(t, h, "uid:501", id, s, nil)
	require.Equal(t, []string{"tu_3"}, approvedIDs(asked), "alpha asks again; beta's grant is still in force")
	require.Equal(t, "alpha", asked[0].Target)
}

func TestChat_RevokeOfAnUnknownGrantIs404(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000053", "alpha")
	h.queue("a1", toolStep("tu_1", "knomit_learn", learnArgs(handle, "Alpha"), usage), textStep("ok", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	drive(t, h, "uid:501", id, s, map[string]string{"tu_1": "allow_session"})

	for name, g := range map[string]grantInfo{
		"another target": {Target: "beta", Kind: "repo", Tool: "knomit_learn", Action: "learn"},
		"another action": {Target: "alpha", Kind: "repo", Tool: "knomit_learn", Action: "update"},
		"another tool":   {Target: "alpha", Kind: "repo", Tool: "knomit_update", Action: "learn"},
		"another kind":   {Target: "alpha", Kind: "lens", Tool: "knomit_learn", Action: "learn"},
	} {
		code, _ := h.do("uid:501", http.MethodDelete, "/chats/"+id+"/grants", g)
		require.Equal(t, http.StatusNotFound, code, name)
	}
	require.Len(t, grantsOf(t, h, "uid:501", id), 1, "none of those removed the real grant")

	// A second revoke of the same grant: the first removed it.
	real := grantInfo{Target: "alpha", Kind: "repo", Tool: "knomit_learn", Action: "learn"}
	code, _ := h.do("uid:501", http.MethodDelete, "/chats/"+id+"/grants", real)
	require.Equal(t, http.StatusNoContent, code)
	code, _ = h.do("uid:501", http.MethodDelete, "/chats/"+id+"/grants", real)
	require.Equal(t, http.StatusNotFound, code)

	// An incomplete body is a bad request, not a 404.
	code, _ = h.do("uid:501", http.MethodDelete, "/chats/"+id+"/grants", grantInfo{Target: "alpha"})
	require.Equal(t, http.StatusBadRequest, code)
}

// Ownership is the approvals route's: another principal's chat is a 404, and
// their revoke changes nothing.
func TestChat_RevokeIsTheOwnersAlone(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000054", "alpha")
	h.queue("a1", toolStep("tu_1", "knomit_learn", learnArgs(handle, "Alpha"), usage), textStep("ok", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	drive(t, h, "uid:501", id, s, map[string]string{"tu_1": "allow_session"})

	g := grantInfo{Target: "alpha", Kind: "repo", Tool: "knomit_learn", Action: "learn"}
	code, _ := h.do("uid:1000", http.MethodDelete, "/chats/"+id+"/grants", g)
	require.Equal(t, http.StatusNotFound, code)
	code, _ = h.do("", http.MethodDelete, "/chats/"+id+"/grants", g)
	require.Equal(t, http.StatusUnauthorized, code)
	require.Len(t, grantsOf(t, h, "uid:501", id), 1)
}

// The usage event numbers the chat's requests, so a client can tell a repeat of
// one request's usage from the next request's.
//
// SABOTAGE: chat.go onLoopEvent, drop `c.usageSeq++` -> every seq is 0 and the
// test fails.
func TestChat_UsageEventsCarryAnIncreasingSeq(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", toolStep("tu_r", "knomit_repos", map[string]any{}, usage), textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	var seqs []int
	for _, e := range evs {
		if e.Name != "usage" {
			continue
		}
		var u struct {
			Seq int `json:"seq"`
		}
		require.NoError(t, json.Unmarshal(e.Data, &u))
		seqs = append(seqs, u.Seq)
	}
	require.Equal(t, []int{1, 2}, seqs, "two requests: the tool call and the answer")
}

// A pause_turn continuation is another request of the same turn: its usage is
// reported with the next seq, not the same one (a client counts a seq once).
//
// SABOTAGE: chat.go onLoopEvent, move `c.usageSeq++` out of the LoopUsage case
// so the pause response's usage reuses the previous seq (or drop the increment)
// -> the seqs are not [1,2] and the test fails.
func TestChat_UsageSeqIncreasesAcrossAPauseTurnContinuation(t *testing.T) {
	h := newHarness(t)
	pause := step{resp: &llm.Response{
		Blocks: []llm.Block{{Kind: llm.BlockText, Text: "working"}}, StopReason: llm.StopPauseTurn, Usage: usage,
	}}
	h.queue("a1", pause, textStep("finished", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)

	var seqs []int
	done := 0
	for _, e := range evs {
		switch e.Name {
		case "usage":
			var u struct {
				Seq int `json:"seq"`
			}
			require.NoError(t, json.Unmarshal(e.Data, &u))
			seqs = append(seqs, u.Seq)
		case "turn_done":
			done++
		}
	}
	require.Equal(t, 1, done, "both responses belong to one turn: "+strings.Join(names(evs), ","))
	require.Equal(t, []int{1, 2}, seqs, "two responses in one turn give two usage events with distinct, increasing seq")
}

// The snapshot (GET /chats/{id}) is the whole truth about every turn: each user
// item carries the turn's summed usage and cost, its price warning, and how it
// ended, so a page that holds one patches nothing.
type snapTurn struct {
	Kind      string     `json:"kind"`
	Text      string     `json:"text"`
	Usage     *turnUsage `json:"usage"`
	CostUSD   *float64   `json:"cost_usd"`
	Warning   string     `json:"warning"`
	Error     string     `json:"error"`
	Cancelled bool       `json:"cancelled"`
}

func userTurns(t *testing.T, h *harness, id string) ([]snapTurn, int) {
	t.Helper()
	code, body := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code, string(body))
	var v struct {
		UsageSeq int        `json:"usage_seq"`
		Items    []snapTurn `json:"items"`
	}
	require.NoError(t, json.Unmarshal(body, &v))
	var out []snapTurn
	for _, it := range v.Items {
		if it.Kind == "user" {
			out = append(out, it)
		}
	}
	return out, v.UsageSeq
}

// SABOTAGE: chat.go onLoopEvent LoopUsage, drop the `it.Usage = u` assignment
// (or LoopWarning's `Warning = text`, or finishTurn's `Error, Cancelled`
// assignment) -> the matching assertion below fails.
func TestChat_SnapshotCarriesEachTurnsUsageCostWarningAndEnd(t *testing.T) {
	h := newHarness(t)
	small := llm.Usage{Input: 11, CacheRead: 22, CacheWrite: 33, Output: 44}
	big := llm.Usage{Input: 60000, CacheRead: 30001, CacheWrite: 7003, Output: 5} // prompt 97004: warns
	h.queue("a1",
		toolStep("tu_r", "knomit_repos", map[string]any{}, small), textStep("one", big), // turn 1: two requests
		step{err: errors.New("provider exploded"), resp: nil}, // turn 2: fails
		step{block: true}) // turn 3: stopped
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "first"))
	drive(t, h, "uid:501", id, s, nil)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "second"))
	drive(t, h, "uid:501", id, s, nil)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "third"))
	// Wait until the third turn is running, then stop it.
	for i := 0; i < 200; i++ {
		_, body := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
		if strings.Contains(string(body), `"running":true`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	code, _ := h.do("uid:501", http.MethodPost, "/chats/"+id+"/cancel", nil)
	require.Equal(t, http.StatusNoContent, code)
	evs := s.until("turn_error")
	require.NotEmpty(t, evs)

	turns, seq := userTurns(t, h, id)
	require.Len(t, turns, 3)

	// Turn 1: both requests summed, priced, with the one warning.
	require.Equal(t, &turnUsage{Input: 60011, CacheRead: 30023, CacheWrite: 7036, Output: 49}, turns[0].Usage)
	require.NotNil(t, turns[0].CostUSD, "a price card prices the turn")
	require.Greater(t, *turns[0].CostUSD, 0.0)
	require.Equal(t, "This chat is nearing 100,000 tokens. Past that, each message costs more. Start a new chat to save.", turns[0].Warning)
	require.Empty(t, turns[0].Error)

	// Turn 2: failed, not cancelled, no usage of its own, no warning.
	require.Contains(t, turns[1].Error, "provider exploded")
	require.False(t, turns[1].Cancelled)
	require.Nil(t, turns[1].Usage)
	require.Empty(t, turns[1].Warning, "the one warning belongs to the turn it fired in")

	// Turn 3: the person's Stop.
	require.NotEmpty(t, turns[2].Error)
	require.True(t, turns[2].Cancelled)

	require.Equal(t, 2, seq, "usage_seq counts the two requests the items include")
}

// GET /chats/{id} during a running turn must not race the loop's updates to the
// turn's summary: the response is encoded after the chat lock is released, so
// the summary the view holds has to be immutable (a new value per update).
//
// SABOTAGE: chat.go onLoopEvent LoopUsage, update the existing *turnUsage in
// place again (`u := it.Usage; ... u.Input += ...`) -> `go test -race` reports
// DATA RACE here.
func TestChat_SnapshotDuringATurnDoesNotRaceTheUsageSummary(t *testing.T) {
	h := newHarness(t)
	pause := step{resp: &llm.Response{
		Blocks: []llm.Block{{Kind: llm.BlockText, Text: "working"}}, StopReason: llm.StopPauseTurn, Usage: usage,
	}}
	steps := make([]step, 0, 20)
	for i := 0; i < 19; i++ {
		steps = append(steps, pause)
	}
	steps = append(steps, textStep("finished", usage))
	h.queue("a1", steps...)
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
				}
			}
		}()
	}
	s.until("turn_done")
	close(stop)
	wg.Wait()

	turns, seq := userTurns(t, h, id)
	require.Len(t, turns, 1)
	require.Equal(t, 20, seq)
	require.EqualValues(t, 20*usage.Input, turns[0].Usage.Input, "every response's usage is in the summary")
}

// Every live event of a turn names the view index of that turn's user item, and
// POST /turns answers it, so a client routes by index and guesses nothing.
//
// SABOTAGE: chat.go, drop "turn" from one emit (text_delta, tool_call, usage,
// approval, turn_done) or return a wrong index from startTurn -> the matching
// assertion fails.
func TestChat_EventsAndTheAcceptedTurnNameTheirTurnIndex(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000061", "alpha")
	h.queue("a1",
		textStep("first answer", usage),
		toolStep("tu_l", "knomit_learn", learnArgs(handle, "Indexed"), usage),
		textStep("second answer", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)

	post := func(text string) int {
		code, body := h.do("uid:501", http.MethodPost, "/chats/"+id+"/turns", map[string]string{"text": text})
		require.Equal(t, http.StatusAccepted, code, string(body))
		var v struct {
			Turn int `json:"turn"`
		}
		require.NoError(t, json.Unmarshal(body, &v))
		return v.Turn
	}
	turnOf := func(evs []sseEvent, name string) []int {
		var out []int
		for _, e := range evs {
			if e.Name != name {
				continue
			}
			var v struct {
				Turn *int `json:"turn"`
			}
			require.NoError(t, json.Unmarshal(e.Data, &v))
			require.NotNil(t, v.Turn, name+" carries a turn")
			out = append(out, *v.Turn)
		}
		return out
	}

	first := post("one")
	require.Equal(t, 0, first)
	evs1, _ := drive(t, h, "uid:501", id, s, nil)
	second := post("two")
	require.Greater(t, second, first)
	evs2, _ := drive(t, h, "uid:501", id, s, map[string]string{"tu_l": "allow"})

	for _, name := range []string{"text_delta", "usage", "turn_done"} {
		require.Equal(t, []int{first}, dedup(turnOf(evs1, name)), name+" of turn one")
	}
	for _, name := range []string{"text_delta", "usage", "turn_done", "tool_call", "approval"} {
		got := dedup(turnOf(evs2, name))
		require.Equal(t, []int{second}, got, name+" of turn two")
	}

	// The index is the position of the turn's user item in the snapshot.
	code, body := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code)
	var v struct {
		Items []snapTurn `json:"items"`
	}
	require.NoError(t, json.Unmarshal(body, &v))
	require.Equal(t, "two", v.Items[second].Text)
	require.Equal(t, "user", v.Items[second].Kind)
}

func dedup(xs []int) []int {
	var out []int
	for _, x := range xs {
		if len(out) == 0 || out[len(out)-1] != x {
			out = append(out, x)
		}
	}
	return out
}
