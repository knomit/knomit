package chat

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func graceOf(c *chat) *time.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.grace
}

// Closing one of two page streams must not start the chat's clock at all.
//
// SABOTAGE: mount.go detachPage `if empty {` -> `if true {` -> a clock is
// running after the first close and this fails (graceFired would only save
// the chat later).
func TestPageStream_DetachStartsNoClockWhileAnotherStreamIsAttached(t *testing.T) {
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	s1 := h.mgr.attachPage(writer.String())
	s2 := h.mgr.attachPage(writer.String())
	require.Nil(t, graceOf(c), "attached: no clock")
	h.mgr.detachPage(s1)
	require.Nil(t, graceOf(c), "one stream left: still no clock")
	h.mgr.detachPage(s2)
	require.NotNil(t, graceOf(c), "the last stream closed: the clock runs")
}

// A clock that runs out while a stream is attached must not end the chat.
//
// SABOTAGE: chat.go graceFired, drop the pageCount check (always end) -> the
// chat ends and this fails.
func TestPageStream_FiredClockSparesAChatWithAStream(t *testing.T) {
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	s := h.mgr.attachPage(writer.String())
	defer h.mgr.detachPage(s)
	c.mu.Lock()
	c.grace = time.NewTimer(time.Hour) // the timer that is firing
	c.mu.Unlock()
	c.graceFired()
	require.NotNil(t, h.mgr.find(id, writer.String()), "a stream is attached: the chat lives")
	require.Nil(t, graceOf(c), "the fired clock is cleared")
}

// The last stream closes after graceFired began and before it finished: the
// chat must end up with a clock (or ended), never with neither.
//
// SABOTAGE: chat.go graceFired, move the `c.grace = nil` block after the
// pageCount check (the original order, with the hook after the check) ->
// startGrace sees the fired timer and returns, the chat keeps no clock and
// this fails.
func TestPageStream_StreamClosingWhileTheClockFiresLeavesAClock(t *testing.T) {
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	s := h.mgr.attachPage(writer.String())
	c.mu.Lock()
	c.grace = time.NewTimer(time.Hour) // the timer that is firing
	c.afterGraceStep = func() { h.mgr.detachPage(s) }
	c.mu.Unlock()
	c.graceFired()
	if h.mgr.find(id, writer.String()) != nil {
		require.NotNil(t, graceOf(c), "no stream and no clock: the chat would leak")
	}
}

// A replay to a stream the manager already dropped (its channel closed by a
// broadcast that found it full) must not panic, and must keep the end for the
// next stream.
//
// SABOTAGE: chat.go adoptPage, send on s.ch directly instead of
// m.deliverTo -> "send on closed channel" panic and this fails.
func TestPageStream_ReplayToADroppedStreamDoesNotPanic(t *testing.T) {
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	owner := writer.String()
	s := &pageSub{owner: owner, ch: make(chan []byte, 1)}
	h.mgr.pageMu.Lock()
	h.mgr.pages = map[string]map[*pageSub]struct{}{owner: {s: {}}}
	h.mgr.pageMu.Unlock()
	require.True(t, h.mgr.broadcast(owner, []byte("a")))  // fills the buffer
	require.False(t, h.mgr.broadcast(owner, []byte("b"))) // full: dropped, channel closed
	c.mu.Lock()
	c.missedEnd = []byte("event: turn_done\ndata: {}\n\n")
	c.mu.Unlock()
	require.NotPanics(t, func() { c.adoptPage(s) })
	c.mu.Lock()
	kept := c.missedEnd != nil
	c.mu.Unlock()
	require.True(t, kept, "the end was not delivered, so the next stream still gets it")
}
