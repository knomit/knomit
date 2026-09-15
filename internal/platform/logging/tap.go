package logging

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"
)

// subscriberBuffer is how many lines one subscriber may fall behind before the
// tap starts dropping for it.
//
// Bounded deliberately, and this is the whole safety property of the type. The
// obvious implementation — goob.Observable, which internal/repos/hub.go and the
// sessions hub both use — buffers per subscriber into an UNBOUNDED slice
// (goob's NewPipe: "Events uses an internal buffer so it won't block Write").
// That is fine for the handful of task events those hubs carry. It is not fine
// here: a browser left on a debug-level server, or one whose connection stalls,
// would grow the server's heap by every line the process logs, with no ceiling
// and no signal. So the fan-out is hand-rolled, capped, and what the cap costs
// is counted.
//
// 1024 is roughly a second of a very chatty server, which is far more slack
// than an EventSource that is merely slow needs, and small enough that a
// thousand stalled subscribers could not matter.
const subscriberBuffer = 1024

// Tap is an io.Writer for the zerolog writer chain that retains the last N
// complete lines AND fans each new line out to live subscribers. It is what
// GET /api/v1/logs/events serves: the ring is the backlog a newly-opened tab
// replays, and the fan-out is everything after that.
//
// The ring is modelled on crashdump.RingWriter (split on newline, drop oldest,
// always report len(p) consumed so a MultiLevelWriter never sees a short
// write). Safe for concurrent use.
//
// It taps the LOG, not the file: a web tab can only ever show a running
// server, and a bare `knomit serve` may have no file sink at all.
type Tap struct {
	ctx context.Context

	mu    sync.Mutex
	max   int
	lines []string
	subs  map[*Subscription]struct{}
}

// NewTap returns a Tap retaining at most max lines. A max <= 0 disables
// retention while still satisfying io.Writer and still feeding subscribers —
// a stream with no backlog, which is what a caller asking for zero means.
//
// ctx bounds every subscription: when it ends, so do they.
func NewTap(ctx context.Context, max int) *Tap {
	return &Tap{ctx: ctx, max: max, subs: map[*Subscription]struct{}{}}
}

// Max returns the retention limit, so the endpoint can tell a client how deep
// the backlog it just received could possibly have been.
func (t *Tap) Max() int { return t.max }

// Write splits p on newlines, retains each non-empty record and publishes it.
// It always reports len(p) consumed.
func (t *Tap) Write(p []byte) (int, error) {
	t.mu.Lock()
	for rec := range bytes.SplitSeq(p, []byte{'\n'}) {
		if len(rec) == 0 {
			continue
		}
		line := string(rec)
		if t.max > 0 {
			t.lines = append(t.lines, line)
		}
		// Under the same lock as the append, which is what makes Subscribe's
		// snapshot-plus-channel atomic: a line cannot slip between a
		// subscriber's backlog and its live stream, and so cannot be missed
		// by both or delivered by both.
		for sub := range t.subs {
			sub.deliver(line)
		}
	}
	if over := len(t.lines) - t.max; t.max > 0 && over > 0 {
		t.lines = t.lines[over:]
	}
	t.mu.Unlock()
	return len(p), nil
}

// Lines returns a copy of the retained lines, oldest first.
func (t *Tap) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.lines...)
}

// Subscribers is the live subscription count. For tests and diagnostics.
func (t *Tap) Subscribers() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.subs)
}

// Subscription is one client's bounded view of the live log.
type Subscription struct {
	ch      chan string
	dropped atomic.Uint64
}

// Lines is the live channel. It is closed only by the tap going away; a reader
// should select on it together with its own context.
func (s *Subscription) Lines() <-chan string { return s.ch }

// Dropped is how many lines this subscription has missed because it was not
// reading fast enough. Monotonic. The endpoint reports increases to the client
// as a `dropped` event, so a viewer never presents a gap as continuity.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// deliver is non-blocking BY CONSTRUCTION: the tap holds its lock and sits in
// the logging chain, so blocking here would block whatever was logging.
// Called with t.mu held.
func (s *Subscription) deliver(line string) {
	select {
	case s.ch <- line:
	default:
		s.dropped.Add(1)
	}
}

// Subscribe returns a live subscription AND the current backlog, taken
// together under one lock so no line falls between them. The subscription ends
// when ctx or the tap's own context does.
//
// The backlog is returned rather than pushed down the channel so a caller can
// tell "history" from "live" — the endpoint sends the backlog, then `ready`,
// then streams.
func (t *Tap) Subscribe(ctx context.Context) (*Subscription, []string) {
	sub := &Subscription{ch: make(chan string, subscriberBuffer)}

	t.mu.Lock()
	t.subs[sub] = struct{}{}
	snapshot := append([]string(nil), t.lines...)
	t.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
		case <-t.ctx.Done():
		}
		t.mu.Lock()
		delete(t.subs, sub)
		t.mu.Unlock()
	}()

	return sub, snapshot
}

// Writer returns the writer to put in the logging chain — NOT the Tap itself.
//
// zerolog hands every writer in a MultiLevelWriter the same raw JSON event;
// only the ConsoleWriter wrappers format. So a Tap wired in bare would receive
// JSON, while the viewer's console parser expects `<stamp> <LVL> <message>`.
// This wrapper is what makes the bytes the format the viewer parses — with
// fileTimeFormat (RFC3339, space-free) for the reason fileTimeFormat itself
// documents: a stamp containing a space shifts the level out of the second
// token and silently disables the viewer's level filter.
//
// NoColor because ANSI escapes would reach the browser as garbage inside the
// level token.
func (t *Tap) Writer() io.Writer {
	return zerolog.ConsoleWriter{Out: t, NoColor: true, TimeFormat: fileTimeFormat}
}
