package web

import (
	"encoding/json"
	"net/http"
	"time"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// handleRepoEvents serves GET /api/v1/repo-events — a SERVER-WIDE stream of
// REPO-LEVEL events, one connection for every repo.
//
// WHAT BELONGS ON THIS STREAM. A repo-event is about the repo AS AN ENTRY IN
// THE REPO LIST — its existence, its availability, or a whole-repo state. It is
// never about content inside a branch. Apply that test, not a list.
//
// Worked both ways, because the test is only useful if it decides cases:
//
//   - `index` BELONGS. The index chip is rendered per repo, in the list, for
//     repos the user is not looking at. Index state is a property of the repo,
//     not of any branch being viewed.
//   - a HEAD CHANGE DOES NOT. A commit is meaningful only relative to a branch;
//     "the head moved" says nothing without naming which. It stays on the
//     per-branch stream, with task progress and sync/push results.
//
// Future kinds follow from the test rather than from this list, but as
// illustration: a repo becoming unavailable, archived, restored, appearing, or
// its origin failing at the repo level.
//
// THE `created` CASE, decided so it does not get two homes: the PROGRESS of a
// creation belongs to the job stream at /repo-creates/{id}/events, which ends
// when the job does; a repo APPEARING in the list — or disappearing, or
// changing availability — is a repo-event, on a stream that does not end.
//
// It exists because the per-repo stream cannot answer the question the UI asks.
// The web app holds one branch-scoped events stream, for the ACTIVE repo, while
// rendering an index chip for EVERY repo in its list — so a per-repo event
// reaches the chip least likely to be stale and no other. The alternative, a
// stream per repo, scales its connection count with the fleet.
//
// One `event:` line per kind. `index` is the first; adding another is a new
// case in the switch below and a new entry in the OpenAPI description, not a
// new stream.
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
func handleRepoEvents(m *repos.Manager) http.HandlerFunc {
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
		events := m.RepoEvents(r.Context())

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
