package kb

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/events"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/store"
)

// lensContentEvent is the ONE event GET /lenses/{lens}/events sends: a
// member's content changed. Head is the member branch's index watermark, so a
// read issued after the event sees the change.
type lensContentEvent struct {
	Repo   string `json:"repo"`
	ID     string `json:"id"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
}

// lensEventMember is one watched (repo, branch) of the lens.
type lensEventMember struct {
	ri     *repos.RepoInstance
	branch string
	// last is the head last reported (or the head at connect): a notice goes
	// out only when the settled watermark moves past it.
	last string
}

// settledHead is the member branch's index watermark, or "" when the index is
// not settled at it (a sync is rewriting the branch's rows) or the store
// cannot be read. "" never produces a notice: the sync in progress publishes
// its own status when it completes, and that pass reports the new head.
func (m *lensEventMember) settledHead(ctx context.Context) string {
	head := ""
	_ = m.ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		wm, _, settled, err := svc.HistoryQuery().SettledWatermark(ctx, m.branch)
		if err == nil && settled {
			head = wm
		}
	})
	return head
}

// handleHALLensEvents serves GET /lenses/{lens}/events: one notice per lens
// member whose CONTENT changed, and nothing else (no status, task, index,
// sync or log events).
//
//   - WHEN: after the member's index has absorbed the commit. The trigger is
//     the member's TaskHub StatusEvent, which the commit observer publishes
//     only after IndexManager().SyncLocked returns; the head sent is the
//     branch's SETTLED index watermark (HistoryQuery.SettledWatermark), so a
//     read issued on the notice sees the change.
//   - WHICH BRANCH: each member at the branch the lens binding resolved for it
//     (reads[].branch, or the repo's read branch), the write repo included.
//   - COALESCING: per member, without timers. A status event only marks the
//     member dirty and wakes the writer (a one-slot channel); the writer
//     drains every dirty member and sends a notice only when its settled
//     watermark moved past the last one sent. A burst therefore yields one
//     notice per member carrying the latest head, however many commits it
//     held, and a slow client never queues more than one pending wake.
//   - MEMBERSHIP: the stream serves the membership resolved at connect, and
//     ENDS when any lens changes (LensRegistry.Changed closes on every
//     committed Update/Rename/Delete). The client's EventSource
//     reconnects onto the new membership and refreshes once on reopen.
//   - NO Last-Event-ID: like every other stream here, events carry no id and
//     nothing is replayed. A reconnecting client refetches instead (the web
//     refreshes its lens list on every reopen).
func handleHALLensEvents(mgr *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !lensEventsParams.RefuseUnknown(w, r) {
			return
		}
		b := repos.BindingFromContext(r.Context())
		var changed <-chan struct{}
		if mgr != nil {
			if reg := mgr.LensRegistry(); reg != nil {
				changed = reg.Changed()
			}
		}

		stream, ok := httpx.StartSSE(w)
		if !ok {
			return
		}
		ctx := r.Context()

		// One member per distinct (repo, branch): the write repo also appears
		// among the reads.
		var members []*lensEventMember
		seen := map[string]bool{}
		for _, t := range b.Reads() {
			if t.RI == nil {
				continue
			}
			key := t.RI.ID() + "\x00" + t.Branch
			if seen[key] {
				continue
			}
			seen[key] = true
			members = append(members, &lensEventMember{ri: t.RI, branch: t.Branch})
		}

		var (
			mu    sync.Mutex
			dirty = make(map[int]bool, len(members))
			wake  = make(chan struct{}, 1)
		)
		// Subscribe BEFORE reading each baseline head, so a commit indexed in
		// between is either in the baseline or delivered as a status event.
		for i, m := range members {
			hub := m.ri.TaskHub()
			if hub == nil {
				continue
			}
			updates, _ := hub.Subscribe(ctx)
			go func(i int) {
				for {
					select {
					case <-ctx.Done():
						return
					case e, ok := <-updates:
						if !ok {
							return
						}
						if _, isStatus := e.(events.StatusEvent); !isStatus {
							continue
						}
						mu.Lock()
						dirty[i] = true
						mu.Unlock()
						select {
						case wake <- struct{}{}:
						default:
						}
					}
				}
			}(i)
		}
		for _, m := range members {
			m.last = m.settledHead(ctx)
		}
		// Flush the headers now (an SSE comment, not an event): the stream has
		// nothing to say until a member changes, and a client only counts it
		// open once the response has started.
		if !stream.Keepalive() {
			return
		}

		keepalive := time.NewTicker(30 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				// The lens (or another one) changed: end, so the client
				// reconnects onto the current membership.
				return
			case <-wake:
				mu.Lock()
				due := make([]int, 0, len(dirty))
				for i := range dirty {
					due = append(due, i)
				}
				clear(dirty)
				mu.Unlock()
				for _, i := range due {
					m := members[i]
					head := m.settledHead(ctx)
					if head == "" || head == m.last {
						continue
					}
					m.last = head
					data, _ := json.Marshal(lensContentEvent{
						Repo: m.ri.Name(), ID: federate.ID12(m.ri.ID()), Branch: m.branch, Head: head,
					})
					if !stream.Write("event: content\ndata: %s\n\n", data) {
						return
					}
				}
			case <-keepalive.C:
				if !stream.Keepalive() {
					return
				}
			}
		}
	}
}
