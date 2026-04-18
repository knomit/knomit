// HAL SSE endpoint for branch-scoped real-time task progress and status updates.
// Clients connect to /api/v1/repos/{repo}/branches/{branch}/events.
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// handleHALEvents handles GET /api/v1/repos/{repo}/branches/{branch}/events.
// SSE stream for branch task progress and head-commit status updates.
func handleHALEvents(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}
		branch := BranchFromContext(r.Context())

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		// Subscribe and obtain snapshot atomically before sending the initial status.
		events, snapshot := ri.TaskHub().Subscribe(r.Context())

		// Snapshot the initial head commit.
		var branches store.BranchIndex
		ri.WithRead(func(svc *store.Service) {
			if svc != nil {
				branches = svc.Branches()
			}
		})
		if branches != nil {
			head, _ := branches.HeadCommit(r.Context(), branch)
			fmt.Fprintf(w, "event: status\ndata: {\"head\":\"%s\"}\n\n", head)
		} else {
			fmt.Fprintf(w, "event: status\ndata: {\"head\":\"\"}\n\n")
		}

		// Replay snapshot (reconnect recovery).
		for _, ev := range snapshot {
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: task\ndata: %s\n\n", data)
		}
		flusher.Flush()

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
				switch ev := e.(type) {
				case repos.TaskEvent:
					data, _ := json.Marshal(ev)
					fmt.Fprintf(w, "event: task\ndata: %s\n\n", data)
				case repos.StatusEvent:
					fmt.Fprintf(w, "event: status\ndata: {\"head\":\"%s\"}\n\n", ev.Head)
				case repos.SyncEvent:
					data, _ := json.Marshal(ev)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Status, data)
				case repos.PushEvent:
					data, _ := json.Marshal(ev)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Status, data)
				default:
					continue
				}
				flusher.Flush()
			case <-keepalive.C:
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	}
}
