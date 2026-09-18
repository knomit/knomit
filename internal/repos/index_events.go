package repos

// Index-state change events.
//
// A repo's index state is the one piece of repo status that changes with NO
// commit behind it: the background heal flips ready→indexing→ready without
// touching a branch ref, so the `status` event never fires and a UI that
// snapshotted the list mid-heal keeps showing "indexing" until something else
// forces a refetch. These events close that gap.

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/ysmood/goob"
)

// IndexEvent reports a repo's index state. The vocabulary is the repos list's,
// deliberately — `ready | indexing | error`, the same strings IndexStatus and
// the REST payload use — so a consumer can patch a list entry from an event
// without translating anything.
type IndexEvent struct {
	Repo  string `json:"repo"`
	State string `json:"state"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// indexProgressInterval bounds PROGRESS events to one per repo per second. A
// rebuild reports done/total per batch and would otherwise emit hundreds of
// events a second for a number a human reads once a second at most.
//
// Terminal and entry events (ready, error, indexing) are NEVER throttled — see
// publishIndex. Dropping one of those is not a dropped frame, it is a UI stuck
// in the state this whole mechanism exists to clear.
const indexProgressInterval = time.Second

// IndexHub fans index events in from every repo to one server-wide stream.
//
// It exists because the per-repo TaskHub cannot answer the question the UI
// actually asks. The web app holds ONE events stream, for the ACTIVE repo, but
// renders an index chip for EVERY repo in its list — so a per-repo event
// reaches the chip of the one repo whose chip was least likely to be stale.
// Rather than open a stream per repo (which scales with the fleet and was
// explicitly not wanted), the instances publish here as well and the app holds
// one extra stream for all of them.
type IndexHub struct {
	ob *goob.Observable
}

// NewIndexHub returns a hub whose lifetime is ctx.
func NewIndexHub(ob *goob.Observable) *IndexHub { return &IndexHub{ob: ob} }

// Subscribe returns the stream of IndexEvents until ctx ends. A nil hub yields
// a nil channel — one that blocks forever rather than one that is already
// closed, because a closed channel would end an SSE handler's loop the instant
// it started and the browser would reconnect in a tight loop.
func (h *IndexHub) Subscribe(ctx context.Context) goob.Events {
	if h == nil || h.ob == nil {
		return nil
	}
	return h.ob.Subscribe(ctx)
}

// publish is nil-safe: a Manager built without a hub (most tests) simply
// broadcasts nothing, and every caller stays unconditional.
func (h *IndexHub) publish(ev IndexEvent) {
	if h == nil || h.ob == nil {
		return
	}
	h.ob.Publish(ev)
}

// indexPublisher is the per-instance throttle state.
type indexPublisher struct {
	lastProgress atomic.Int64 // unix nanos of the last PROGRESS event
}

// publishIndex is THE chokepoint for index events. Every index-state change
// goes through it, and nothing else may publish an IndexEvent.
//
// WHY A CHOKEPOINT rather than a call at each site: index state changes in five
// places today, ALL of them inside Manager.openOne — the synchronous
// (DisableBackgroundSync) branch's two exits, and the background heal's
// markIndexing plus its two exits — and a sixth will be added by someone who
// does not know this event exists. Publishing inside the mark* methods makes
// the event a property of the STATE CHANGE rather than of remembering to
// announce it, the same reason every branch-ref mutation goes through
// notifyCommit rather than each mutation emitting its own.
//
// A SIXTH site now exists, outside this package: the manual rebuild endpoint
// (handleStartRebuild) brackets its rebuild with MarkIndexRebuildStart and
// MarkIndexRebuildDone, which are exported aliases for these same marks. It
// used to touch index state not at all, so IndexStatus read "ready" throughout
// a rebuild; that was a separate defect and is fixed. The chokepoint still
// holds — the rebuild publishes only by marking — but the single-writer
// property it relies on is now enforced by that endpoint's 409, not by there
// being only one code path. Read handleStartRebuild before adding a seventh.
//
// It also inherits the stuck-indexing incident's guarantee for free. That
// incident was a heal exit path that reached neither markIndexReady nor
// markIndexFailed, pinning the UI at "indexing" forever; the repair made every
// exit path mark. Because the event rides ON the mark, "every exit path marks"
// and "every exit path announces" are ONE rule that cannot drift apart, rather
// than two that have to be kept in sync.
//
// It reads IndexStatus() rather than taking the state as an argument, so the
// event can never describe something other than what a reader polling the REST
// endpoint would see.
func (ri *RepoInstance) publishIndex(throttled bool) {
	state, done, total := ri.IndexStatus()

	if throttled {
		// Progress only. Terminal and entry events never reach here.
		now := time.Now().UnixNano()
		last := ri.indexPub.lastProgress.Load()
		if now-last < int64(indexProgressInterval) {
			return
		}
		// CAS so two concurrent progress callbacks cannot both pass the window.
		if !ri.indexPub.lastProgress.CompareAndSwap(last, now) {
			return
		}
	} else {
		// A terminal or entry event supersedes the throttle window: the next
		// progress event after it should not be suppressed because a
		// now-irrelevant tick happened to be recent.
		ri.indexPub.lastProgress.Store(0)
	}

	ev := IndexEvent{Repo: ri.Name(), State: state, Done: done, Total: total}
	// Per-repo stream, for a client watching one repo.
	if ri.hub != nil {
		ri.hub.broadcastIndex(ev)
	}
	// Server-wide stream, for the fleet-wide chip.
	ri.indexHub.publish(ev)
}
