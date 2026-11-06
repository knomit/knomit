package chat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

// untilChat returns the events up to and including the first one named name
// that is tagged with chat id.
func untilChat(s *stream, id, name string) []sseEvent {
	s.t.Helper()
	for {
		e := s.next(name, 10*time.Second)
		if e.Name == name && chatOfEvent(e) == id {
			return []sseEvent{e}
		}
	}
}

func noMore(t *testing.T, s *stream, d time.Duration, what string) {
	t.Helper()
	select {
	case extra := <-s.events:
		t.Fatalf("%s: got %q %s", what, extra.Name, extra.Data)
	case <-time.After(d):
	}
}

// ONE stream carries the events of several chats, each tagged with its chat
// id, and the payload keeps its turn index and the rest of its fields.
//
// SABOTAGE: chat.go sendLocked, drop the tagChat call -> every event has chat
// "" and this fails.
func TestPageStream_RoutesSeveralChatsByID(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", textStep("from a", usage))
	h.queue("a2", textStep("from b", usage))
	a := h.create("uid:501", "a1")
	b := h.create("uid:501", "a2")
	s := h.attachPage("uid:501")

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", a, "go"))
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", b, "go"))

	seen := map[string]map[string]string{a: {}, b: {}}
	ends := 0
	for ends < 2 {
		e := s.next("end", 10*time.Second)
		id := chatOfEvent(e)
		require.Contains(t, []string{a, b}, id, "event %s is tagged with one of the chats", e.Name)
		var v struct {
			Text string `json:"text"`
			Turn *int   `json:"turn"`
		}
		require.NoError(t, json.Unmarshal(e.Data, &v))
		if e.Name == "text_delta" {
			seen[id]["text"] = v.Text
			require.NotNil(t, v.Turn, "the turn index stays in the payload")
		}
		if e.Name == "turn_done" {
			ends++
		}
	}
	require.Equal(t, "from a", seen[a]["text"])
	require.Equal(t, "from b", seen[b]["text"])
}

// Another principal's page stream never sees this caller's chats.
func TestPageStream_OtherPrincipalSeesNothing(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", textStep("hi", usage))
	id := h.create("uid:501", "a1")
	mine := h.attachPage("uid:501")
	other := h.attachPage("uid:1000")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	untilChat(mine, id, "turn_done")
	noMore(t, other, 200*time.Millisecond, "the other principal's stream")
}

// The chat_state event tells the rail when the row changed.
func TestPageStream_ChatStateTracksTheRow(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", textStep("hi", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	var states []string
	for {
		e := s.next("state", 10*time.Second)
		if e.Name == "chat_state" {
			var v rowState
			require.NoError(t, json.Unmarshal(e.Data, &v))
			states = append(states, v.State)
			if v.State == "idle" {
				require.Equal(t, 1, v.Turns)
				break
			}
		}
	}
	require.Equal(t, "running", states[0])
	require.Equal(t, "idle", states[len(states)-1])
}

// A chat lives while ANY page stream of its owner is attached, and ends the
// grace after the last one closes. Two streams are two tabs.
//
// Two independent guards keep the chat alive when one of two tabs closes:
// detachPage starts the clocks only when the owner's set is empty, and
// graceFired re-checks the count. This end-to-end test fails only when BOTH
// are broken; the tests in pagestream_grace_test.go each fail on one guard.
//
// SABOTAGE: mount.go detachPage `if empty {` -> `if true {` AND chat.go
// graceFired, skip the pageCount return -> the chat ends and this fails.
func TestPageStream_ChatLivesWhileAnyStreamIsAttached(t *testing.T) {
	shortGrace(t, 250*time.Millisecond)
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	s1 := h.attachPage("uid:501")
	s2 := h.attachPage("uid:501")

	s1.close()
	time.Sleep(600 * time.Millisecond) // more than two graces
	code, _ := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code, "the second stream keeps the chat alive")

	s2.close()
	eventually(t, "the chat to end after the last stream closed", func() bool {
		code, _ := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
		return code == http.StatusNotFound
	})
}

// A chat created while a page stream is attached is covered at once: no grace
// clock ends it. One created with none attached still follows the grace.
func TestPageStream_CreateWhileAttachedIsCoveredAndWithoutIsNot(t *testing.T) {
	shortGrace(t, 200*time.Millisecond)
	h := newHarness(t)
	bare := h.create("uid:501", "a1") // no page stream yet
	s := h.attachPage("uid:501")      // attaching adopts it
	covered := h.create("uid:501", "a2")
	time.Sleep(500 * time.Millisecond)
	for _, id := range []string{bare, covered} {
		code, _ := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
		require.Equal(t, http.StatusOK, code, id)
	}
	s.close()
	eventually(t, "the server to see the stream gone", func() bool { return h.mgr.pageCount(writer.String()) == 0 })
	alone := h.create("uid:501", "a1") // created with no stream attached
	for _, id := range []string{bare, covered, alone} {
		id := id
		eventually(t, "chat to end by grace", func() bool {
			code, _ := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
			return code == http.StatusNotFound
		})
	}
}

// A chat nobody ever attached to still ends after the grace, though the owner's
// other chats are covered by a stream of ANOTHER owner.
func TestPageStream_AnotherOwnersStreamDoesNotCover(t *testing.T) {
	shortGrace(t, 150*time.Millisecond)
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	_ = h.attachPage("uid:1000")
	eventually(t, "the chat to end", func() bool {
		code, _ := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
		return code == http.StatusNotFound
	})
}

// DELETE ends a chat at once while page streams are attached, and the stream
// is told, tagged with the chat, and stays open for the others.
func TestPageStream_DeleteEndsAtOnceAndTheStreamStays(t *testing.T) {
	h := newHarness(t)
	a := h.create("uid:501", "a1")
	b := h.create("uid:501", "a2")
	s := h.attachPage("uid:501")
	code, _ := h.do("uid:501", http.MethodDelete, "/chats/"+a, nil)
	require.Equal(t, http.StatusNoContent, code)
	untilChat(s, a, "chat_end")
	code, _ = h.do("uid:501", http.MethodGet, "/chats/"+a, nil)
	require.Equal(t, http.StatusNotFound, code)
	code, _ = h.do("uid:501", http.MethodGet, "/chats/"+b, nil)
	require.Equal(t, http.StatusOK, code, "the other chat lives and the stream is open")
}

// A turn end no page stream received is delivered once, to the next page
// stream, right after ready, and only for the chat it belongs to.
//
// SABOTAGE: chat.go sendLocked, never record missedEnd (or adoptPage, never
// replay it) -> the reconnecting stream sees nothing and this fails.
func TestPageStream_MissedTurnEndIsDeliveredOncePerChat(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.queue("a1", step{fn: func(llm.Snapshot) step { <-release; return textStep("ok", usage) }})
	h.queue("a2", textStep("quick", usage))
	a := h.create("uid:501", "a1")
	b := h.create("uid:501", "a2")
	s := h.attachPage("uid:501")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", a, "go"))
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", b, "go"))
	untilChat(s, b, "turn_done") // b ended with a stream attached: nothing is missed for b
	s.close()
	time.Sleep(50 * time.Millisecond)
	close(release)
	eventually(t, "a's turn to end", func() bool {
		_, body := h.do("uid:501", http.MethodGet, "/chats/"+a, nil)
		return !strings.Contains(string(body), `"running":true`)
	})

	s2 := h.attachPage("uid:501")
	got := s2.next("missed end", 3*time.Second)
	require.Equal(t, "turn_done", got.Name)
	require.Equal(t, a, chatOfEvent(got), "the missed end is a's, tagged with a")
	noMore(t, s2, 200*time.Millisecond, "a second event after the replay")
	s2.close()
	s3 := h.attachPage("uid:501")
	noMore(t, s3, 200*time.Millisecond, "a later stream")
}

// A stream attached when the turn ended took the end, so it is not replayed.
func TestPageStream_AnEndTakenByAStreamIsNotReplayed(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", textStep("ok", usage))
	id := h.create("uid:501", "a1")
	s := h.attachPage("uid:501")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	untilChat(s, id, "turn_done")
	s2 := h.attachPage("uid:501")
	noMore(t, s2, 200*time.Millisecond, "the end was already delivered")
}
