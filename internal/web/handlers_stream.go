// Server-Sent Events (SSE) endpoint for real-time task progress and
// status updates. Clients connect to /api/v1/{repo}/events and receive task
// lifecycle events plus periodic head-commit heartbeats.
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"knomit/internal/repos"
)

// handleEvents handles GET /api/v1/{repo}/events — SSE endpoint for real-time updates.
func handleEvents() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		// Subscribe returns both a channel and a snapshot of recent events,
		// providing reconnection recovery without maintaining client-side state.
		events, snapshot := ri.Hub.Subscribe(r.Context())

		// Snapshot the initial head commit under RLock — GS may be swapped concurrently.
		ri.RLock()
		head, _ := ri.GS.HeadCommit()
		ri.RUnlock()
		fmt.Fprintf(w, "event: status\ndata: {\"head\":\"%s\"}\n\n", head)

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
