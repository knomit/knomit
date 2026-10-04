package repos

// Index-state change events.
//
// A repo's index state is the one piece of repo status that changes with NO
// commit behind it: the index job flips indexing→ready without touching a
// branch ref, so the `status` event never fires and a UI that snapshotted the
// list mid-heal keeps showing "indexing" until something else forces a
// refetch. These events close that gap.

import (
	"context"
	"time"

	"github.com/ysmood/goob"
)

// IndexEvent reports a repo's index state. The vocabulary is the repos list's,
// deliberately — `ready | indexing | error`, the same strings Status().Index
// and the REST payload use — so a consumer can patch a list entry from an
// event without translating anything. Reason says why an `error` is one
// ("indexing cancelled", or the job's own error).
type IndexEvent struct {
	Repo   string `json:"repo"`
	State  string `json:"state"`
	Done   int    `json:"done"`
	Total  int    `json:"total"`
	Reason string `json:"reason,omitempty"`
}

// indexProgressInterval bounds PROGRESS events to one per repo per second. A
// rebuild reports done/total per batch and would otherwise emit hundreds of
// events a second for a number a human reads once a second at most.
//
// Terminal and entry events (ready, error, indexing) are NEVER throttled — see
// publishIndex. Dropping one of those is not a dropped frame, it is a UI stuck
// in the state this whole mechanism exists to clear.
const indexProgressInterval = time.Second

// RepoEventHub fans REPO-LEVEL events in from every repo to one server-wide
// stream. IndexEvent is its first and currently only kind.
//
// It exists because the per-repo TaskHub cannot answer the question the UI
// actually asks. The web app holds ONE events stream, for the ACTIVE repo, but
// renders an index chip for EVERY repo in its list — so a per-repo event
// reaches the chip of the one repo whose chip was least likely to be stale.
// Rather than open a stream per repo (which scales with the fleet and was
// explicitly not wanted), the machines publish here as well and the app holds
// one extra stream for all of them.
type RepoEventHub struct {
	ob *goob.Observable
}

// NewRepoEventHub returns a hub whose lifetime is ctx.
func NewRepoEventHub(ob *goob.Observable) *RepoEventHub { return &RepoEventHub{ob: ob} }

// Subscribe returns the stream of repo events until ctx ends. A nil hub yields
// a nil channel — one that blocks forever rather than one that is already
// closed, because a closed channel would end an SSE handler's loop the instant
// it started and the browser would reconnect in a tight loop.
func (h *RepoEventHub) Subscribe(ctx context.Context) goob.Events {
	if h == nil || h.ob == nil {
		return nil
	}
	return h.ob.Subscribe(ctx)
}

// publish is nil-safe: a Manager built without a hub (most tests) simply
// broadcasts nothing, and every caller stays unconditional.
func (h *RepoEventHub) publish(ev IndexEvent) {
	if h == nil || h.ob == nil {
		return
	}
	h.ob.Publish(ev)
}

// indexPublisher is the per-machine progress throttle. Driver-owned.
type indexPublisher struct {
	lastProgress time.Time // when the last PROGRESS event went out
}

// publishIndex emits one IndexEvent describing s. Its only caller is the
// machine's publish(), the one publish point: index state is DERIVED from the
// machine (cursor + Index result) and an event goes out only after a
// transition, so "every exit path marks" and "every exit path announces" are
// one rule rather than two kept in sync. Nothing else constructs an
// IndexEvent, and a closed machine publishes nothing.
//
// Progress events are THROTTLED (one per repo per second); entry and terminal
// events never are, and each resets the window so the next progress event is
// not suppressed by a now-irrelevant tick.
func (m *Machine) publishIndex(s Status, throttled bool) {
	now := time.Now()
	if throttled {
		if now.Sub(m.pub.lastProgress) < indexProgressInterval {
			return
		}
		m.pub.lastProgress = now
	} else {
		m.pub.lastProgress = time.Time{}
	}
	ev := IndexEvent{Repo: m.r.Name(), State: s.Index.State, Done: s.Index.Done, Total: s.Index.Total, Reason: s.Index.Reason}
	// Per-repo stream, for a client watching one repo.
	if m.r.hub != nil {
		m.r.hub.broadcastIndex(ev)
	}
	// Server-wide stream, for the fleet-wide chip.
	m.r.repoEventHub.publish(ev)
}
