package web

import (
	"encoding/json"
	"net/http"
	"time"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// handleRepoIndexEvents serves GET /api/v1/index-events — a SERVER-WIDE stream
// of index-state changes, one connection for every repo.
//
// It exists because the per-repo stream cannot answer the question the UI asks.
// The web app holds one events stream, scoped to the ACTIVE repo, while
// rendering an index chip for EVERY repo in its list — so a per-repo event
// reaches the chip least likely to be stale and no other. The alternative, a
// stream per repo, scales its connection count with the fleet.
//
// It carries ONLY index events. The per-repo stream keeps task, status, sync
// and push, which are all branch-scoped and have no meaning without one.
//
// It lives at a TOP-LEVEL path rather than under /repos/ because a static
// segment there would shadow a repo of the same name — this route was
// /repos/events until that was noticed. See router.go's no-static-children rule.
//
// Modelled on the client-sessions change stream, including the `ready` frame:
// a client that reconnects after missing a terminal event is stale, and a frame
// on connect is what tells it to re-read the list. Unlike that stream, this one
// carries the full payload rather than a ping, because an index event is four
// small fields and a re-read per event would be a request per progress tick.
func handleRepoIndexEvents(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Repositories unavailable",
				"the repository manager is not open on this server", r.URL.Path)
			return
		}
		stream, ok := startSSE(w)
		if !ok {
			return
		}

		// Subscribe BEFORE announcing ready: an event published between the two
		// would fall in the gap that `ready` exists to close.
		events := m.IndexEvents(r.Context())

		// One `ready` per connection. The client refetches the repo list on a
		// LATER one, because a reconnect's gap may have dropped the terminal
		// event — the same fallback the branch stream's open handler models.
		if !stream.Write("event: ready\ndata: {}\n\n") {
			return
		}

		keepalive := time.NewTicker(30 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-events:
				if !ok {
					return
				}
				ev, ok := e.(repos.IndexEvent)
				if !ok {
					continue
				}
				data, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				if !stream.Write("event: index\ndata: %s\n\n", data) {
					return
				}
			case <-keepalive.C:
				if !stream.Keepalive() {
					return
				}
			}
		}
	}
}
