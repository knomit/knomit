package chat

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

// Every way a turn can end is told to the client as exactly one terminal event
// (turn_done or turn_error), even when the turn ends while no events stream is
// attached: a long generation outlives a stream that dropped (a slow reader,
// a proxy, a reconnecting tab) and the stream comes back after the turn is
// over. The reconnecting stream gets the event it missed, once, right after
// ready; a stream that was attached when the turn ended must not get it again.
//
// SABOTAGE: chat.go finishTurn, skip the c.missedEnd recording (or attach's
// replay) -> the reconnecting stream sees only ready and the test fails.
func TestChat_EveryTurnEndReachesTheClientOnce(t *testing.T) {
	cut := &llm.StopError{Reason: llm.StopMaxTokens, Response: &llm.Response{StopReason: llm.StopMaxTokens, Usage: llm.Usage{Input: 5, Output: 7}}}
	for _, tc := range []struct {
		name string
		end  step
		want string
	}{
		{"max_tokens", step{err: cut}, "turn_error"},
		{"invalid tool call arguments", step{err: errors.New(`invalid tool call arguments for "knomit_learn": unexpected end of JSON input`)}, "turn_error"},
		{"finished", step{resp: &llm.Response{Blocks: []llm.Block{{Kind: llm.BlockText, Text: "ok"}}, StopReason: llm.StopEndTurn}}, "turn_done"},
	} {
		t.Run(tc.name+"/stream dropped during the turn", func(t *testing.T) {
			release := make(chan struct{})
			h := newHarness(t)
			// A first turn, so the turn under test is not turn 0: a payload
			// with no turn field would read as 0.
			h.queue("a1", textStep("first", usage), step{fn: func(llm.Snapshot) step { <-release; return tc.end }})
			id := h.create("uid:501", "a1")
			ended := endsOf(t, h, id)
			s := h.attach("uid:501", id)
			require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "first"))
			s.until("turn_done")
			waitEnd(t, ended, "the first turn's end")
			code, body := h.do("uid:501", http.MethodPost, "/chats/"+id+"/turns", map[string]string{"text": "go"})
			require.Equal(t, http.StatusAccepted, code)
			var accepted struct {
				Turn int `json:"turn"`
			}
			require.NoError(t, json.Unmarshal(body, &accepted))
			require.Greater(t, accepted.Turn, 0)
			s.close() // the stream is gone while the model runs
			// The server notices the closed connection on its own goroutine;
			// until it detaches the stream, a broadcast still "reaches" it.
			h.waitDetached(writer.String())
			close(release)
			// Wait for the end to have been SENT, not for running=false: a
			// snapshot shows running=false before finishTurn sends chat_state
			// and the end, and a stream attaching in that gap gets chat_state
			// first and the end live instead of replayed.
			waitEnd(t, ended, "the turn under test's end")

			s2 := h.attach("uid:501", id)
			got := s2.next("terminal", 3*time.Second)
			require.Equal(t, tc.want, got.Name, "the missed terminal event is replayed after ready")
			var payload struct {
				Turn int `json:"turn"`
			}
			require.NoError(t, json.Unmarshal(got.Data, &payload))
			require.Equal(t, accepted.Turn, payload.Turn, "the replayed event names the turn it ended")
			select {
			case extra := <-s2.events:
				t.Fatalf("a second event %q: the terminal event must be delivered once", extra.Name)
			case <-time.After(150 * time.Millisecond):
			}
			s2.close()
			s3 := h.attach("uid:501", id)
			select {
			case extra := <-s3.events:
				t.Fatalf("a later stream got %q: the event was already delivered", extra.Name)
			case <-time.After(150 * time.Millisecond):
			}
		})
		t.Run(tc.name+"/stream attached", func(t *testing.T) {
			h := newHarness(t)
			h.queue("a1", tc.end)
			id := h.create("uid:501", "a1")
			s := h.attach("uid:501", id)
			require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
			evs := s.until(tc.want)
			require.Equal(t, tc.want, evs[len(evs)-1].Name)
			s.close()
			s2 := h.attach("uid:501", id)
			select {
			case extra := <-s2.events:
				t.Fatalf("a later stream got %q: it was delivered to the first", extra.Name)
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}

func contains(b []byte, s string) bool { return len(s) > 0 && strings.Contains(string(b), s) }

// endsOf returns a channel that gets one value each time a turn of chat id has
// sent its terminal event (finishTurn's afterEnd seam).
func endsOf(t *testing.T, h *harness, id string) <-chan struct{} {
	t.Helper()
	c := h.mgr.find(id, writer.String())
	require.NotNil(t, c)
	ch := make(chan struct{}, 16)
	c.mu.Lock()
	c.afterEnd = func() { ch <- struct{}{} }
	c.mu.Unlock()
	return ch
}

func waitEnd(t *testing.T, ended <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s to be sent", what)
	}
}

// startTurn forgets the previous turn's undelivered end: a stream that attaches
// while the next turn runs is not handed the last turn's turn_done first, which
// a page holding a local unaccepted turn could bind to the wrong turn. The new
// turn's own end reaches it, with the new turn's index.
//
// SABOTAGE: chat.go startTurn, delete the `c.missedEnd = nil` line -> the
// attaching stream gets turn one's turn_done before ready's successor and this
// fails.
func TestChat_StartTurnClearsThePreviousTurnsMissedEnd(t *testing.T) {
	release1, release2 := make(chan struct{}), make(chan struct{})
	done := func(text string) *llm.Response {
		return &llm.Response{Blocks: []llm.Block{{Kind: llm.BlockText, Text: text}}, StopReason: llm.StopEndTurn}
	}
	h := newHarness(t)
	h.queue("a1",
		step{fn: func(llm.Snapshot) step { <-release1; return step{resp: done("one")} }},
		step{fn: func(llm.Snapshot) step { <-release2; return step{resp: done("two")} }})
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "one"))
	s.close() // turn one ends with nobody attached: its end is kept as missed
	close(release1)
	notRunning := func() bool {
		code, body := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
		return code == http.StatusOK && !strings.Contains(string(body), `"running":true`)
	}
	eventually(t, "turn one to end", notRunning)

	code, body := h.do("uid:501", http.MethodPost, "/chats/"+id+"/turns", map[string]string{"text": "two"})
	require.Equal(t, http.StatusAccepted, code)
	var second struct {
		Turn int `json:"turn"`
	}
	require.NoError(t, json.Unmarshal(body, &second))

	s2 := h.attach("uid:501", id)
	select {
	case extra := <-s2.events:
		t.Fatalf("a stream attaching during turn two got %q: turn one's end is stale", extra.Name)
	case <-time.After(150 * time.Millisecond):
	}
	close(release2)
	evs := s2.until("turn_done")
	var payload struct {
		Turn int `json:"turn"`
	}
	require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &payload))
	require.Equal(t, second.Turn, payload.Turn, "the end it gets is turn two's")
}

// Turn one's end is sent after finishTurn released the lock, so turn two can
// start in that gap. The end that nobody took is then turn one's, not the
// current turn's: it must not be parked for a page that attaches while turn two
// runs. The seam starts turn two exactly there, with no stream attached, so the
// interleaving is the same every run.
//
// SABOTAGE: chat.go sendLocked, change the park condition `terminal && turn ==
// c.turnAt && !c.running` to `terminal` (and adoptPage's to `c.missedEnd !=
// nil`) -> turn one's turn_done is parked after turn two started, the attaching
// stream gets it, and this fails.
func TestChat_AnEndThatLandsAfterTheNextTurnStartedIsNotParked(t *testing.T) {
	release := make(chan struct{})
	done := func(text string) *llm.Response {
		return &llm.Response{Blocks: []llm.Block{{Kind: llm.BlockText, Text: text}}, StopReason: llm.StopEndTurn}
	}
	h := newHarness(t)
	h.queue("a1",
		textStep("one", usage),
		step{fn: func(llm.Snapshot) step { <-release; return step{resp: done("two")} }})
	id := h.create("uid:501", "a1")
	ended := endsOf(t, h, id)
	c := h.mgr.find(id, writer.String())
	started := make(chan int, 1)
	c.mu.Lock()
	c.betweenRunAndEnd = func() {
		c.mu.Lock()
		c.betweenRunAndEnd = nil // once
		c.mu.Unlock()
		turn, err := c.startTurn("two", "")
		require.NoError(t, err)
		started <- turn
	}
	c.mu.Unlock()

	// No stream is attached: nothing can take turn one's end.
	code, body := h.do("uid:501", http.MethodPost, "/chats/"+id+"/turns", map[string]string{"text": "one"})
	require.Equal(t, http.StatusAccepted, code, string(body))
	var first struct {
		Turn int `json:"turn"`
	}
	require.NoError(t, json.Unmarshal(body, &first))
	var second int
	select {
	case second = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn two never started in the gap")
	}
	require.Greater(t, second, first.Turn)
	// Turn one's end goes out after the seam returns. A stream attached before
	// that gets it LIVE (correctly: it names turn one), which this test would
	// misread as a parked end handed over on attach; so attach only once it
	// has been sent to nobody.
	waitEnd(t, ended, "turn one's end")

	s := h.attach("uid:501", id)
	select {
	case extra := <-s.events:
		t.Fatalf("a stream attaching during turn two got %q: turn one's end is stale", extra.Name)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	evs := s.until("turn_done")
	var payload struct {
		Turn int `json:"turn"`
	}
	require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &payload))
	require.Equal(t, second, payload.Turn, "the end it gets is turn two's")
}
