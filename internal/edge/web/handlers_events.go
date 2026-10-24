// HAL SSE endpoint for branch-scoped real-time task progress and status updates.
// Clients connect to /api/v1/repos/{repo}/branches/{branch}/events.
package web

import (
	"context"
	"encoding/json"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"time"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
)

// handleHALEvents handles GET /api/v1/repos/{repo}/branches/{branch}/events.
// SSE stream for branch task progress and head-commit status updates.
func handleHALEvents() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		branch := httpx.BranchFromContext(r.Context())

		stream, ok := httpx.StartSSE(w)
		if !ok {
			return
		}

		// Subscribe and obtain snapshot atomically before sending the initial status.
		events, snapshot := ri.TaskHub().Subscribe(r.Context())

		// Snapshot the initial head commit. Best-effort, deliberately (user
		// ruling 2026-10-24: the SSE stream is the one read that does not
		// answer 503): WithRead's error is dropped, so a store that cannot be
		// reached opens the stream with head "" rather than refusing it. The
		// stream has already started, so there is no status left to answer.
		var headCommit func(ctx context.Context, branch string) (string, error)
		ri.WithRead(func(svc *store.Service) {
			if svc != nil {
				headCommit = svc.HeadCommit
			}
		})
		head := ""
		if headCommit != nil {
			head, _ = headCommit(r.Context(), branch)
		}
		if !stream.Write("event: status\ndata: {\"head\":\"%s\"}\n\n", head) {
			return
		}

		// Replay snapshot (reconnect recovery).
		for _, ev := range snapshot {
			data, _ := json.Marshal(ev)
			if !stream.Write("event: task\ndata: %s\n\n", data) {
				return
			}
		}

		// Keepalive to prevent proxy/browser timeouts.
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
				sent := true
				switch ev := e.(type) {
				case repos.TaskEvent:
					data, _ := json.Marshal(ev)
					sent = stream.Write("event: task\ndata: %s\n\n", data)
				case repos.StatusEvent:
					sent = stream.Write("event: status\ndata: {\"head\":\"%s\"}\n\n", ev.Head)
				case repos.IndexEvent:
					// The index state changes with no commit behind it, so the
					// `status` event above never fires for it. A client watching
					// one repo gets it here; the fleet-wide chip uses
					// GET /api/v1/repo-events instead.
					data, _ := json.Marshal(ev)
					sent = stream.Write("event: index\ndata: %s\n\n", data)
				case repos.SyncEvent:
					data, _ := json.Marshal(ev)
					sent = stream.Write("event: %s\ndata: %s\n\n", ev.Status, data)
				case repos.PushEvent:
					data, _ := json.Marshal(ev)
					sent = stream.Write("event: %s\ndata: %s\n\n", ev.Status, data)
				case repos.TriggerEvent:
					// The hub is per repo, so every branch stream receives
					// every fire; a fire belongs to the agent branch it
					// happened on and goes ONLY to that branch's stream.
					if ev.Branch != branch {
						continue
					}
					data, _ := json.Marshal(ev)
					sent = stream.Write("event: trigger\ndata: %s\n\n", data)
				default:
					continue
				}
				if !sent {
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
