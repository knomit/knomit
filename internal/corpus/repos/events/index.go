package events

// Index-state change events.
//
// A repo's index state is the one piece of repo status that changes with NO
// commit behind it: the index job flips indexing→ready without touching a
// branch ref, so the `status` event never fires and a UI that snapshotted the
// list mid-heal keeps showing "indexing" until something else forces a
// refetch. These events close that gap.

import (
	"context"

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

// RepoEventHub fans REPO-LEVEL events in from every repo to one server-wide
// stream. Its kinds are IndexEvent and RemoteEvent.
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

// RepoEventPublisher is the publishing half of a RepoEventHub. The Manager
// keeps it in an unexported field; consumers hold the *RepoEventHub, which can
// only Subscribe. TestEventPublishers_NotExposed keeps it that way.
type RepoEventPublisher struct {
	h *RepoEventHub
}

// NewRepoEventHub returns a hub over ob and its publisher.
func NewRepoEventHub(ob *goob.Observable) (*RepoEventHub, *RepoEventPublisher) {
	h := &RepoEventHub{ob: ob}
	return h, &RepoEventPublisher{h: h}
}

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

// Publish is nil-safe: a Manager built without a hub (most tests) simply
// broadcasts nothing, and every caller stays unconditional. ev is one of the
// kinds named on RepoEventHub; the SSE handler drops anything else.
func (p *RepoEventPublisher) Publish(ev any) {
	if p == nil || p.h == nil || p.h.ob == nil {
		return
	}
	p.h.ob.Publish(ev)
}
