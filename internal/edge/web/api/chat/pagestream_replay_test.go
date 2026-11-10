package chat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
