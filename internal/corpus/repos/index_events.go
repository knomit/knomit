package repos

// Index-state change publishing: the machine's half of the events.IndexEvent
// stream (the event types and both hubs live in repos/events).

import (
	"time"

	"knomit/internal/corpus/repos/events"
)

// indexProgressInterval bounds PROGRESS events to one per repo per second. A
// rebuild reports done/total per batch and would otherwise emit hundreds of
// events a second for a number a human reads once a second at most.
//
// Terminal and entry events (ready, error, indexing) are NEVER throttled — see
// publishIndex. Dropping one of those is not a dropped frame, it is a UI stuck
// in the state this whole mechanism exists to clear.
const indexProgressInterval = time.Second

// indexPublisher is the per-machine progress throttle. Driver-owned.
type indexPublisher struct {
	lastProgress time.Time // when the last PROGRESS event went out
}

// publishIndex emits one events.IndexEvent describing s. Its only caller is the
// machine's publish(), the one publish point: index state is DERIVED from the
// machine (cursor + Index result) and an event goes out only after a
// transition, so "every exit path marks" and "every exit path announces" are
// one rule rather than two kept in sync. Nothing else constructs an
// events.IndexEvent, and a closed machine publishes nothing.
//
// Progress events are THROTTLED (one per repo per second); entry and terminal
// events never are, and each resets the window so the next progress event is
// not suppressed by a now-irrelevant tick.
func (m *Machine) publishIndex(s events.Status, throttled bool) {
	now := time.Now()
	if throttled {
		if now.Sub(m.pub.lastProgress) < indexProgressInterval {
			return
		}
		m.pub.lastProgress = now
	} else {
		m.pub.lastProgress = time.Time{}
	}
	ev := events.IndexEvent{Repo: m.r.Name(), State: s.Index.State, Done: s.Index.Done, Total: s.Index.Total, Reason: s.Index.Reason}
	// Per-repo stream, for a client watching one repo.
	if m.r.hub != nil {
		m.r.hub.BroadcastIndex(ev)
	}
	// Server-wide stream, for the fleet-wide chip.
	m.r.repoEventHub.Publish(ev)
}
