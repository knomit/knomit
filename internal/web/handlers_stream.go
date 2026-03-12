// Server-Sent Events (SSE) endpoint for real-time task progress and
// status updates. Clients connect to /api/v1/events and receive task
// lifecycle events plus periodic head-commit heartbeats.
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// handleEvents handles GET /api/v1/events — SSE endpoint for real-time updates.
func handleEvents(gs GitStore, idx SearchIndex, hub *TaskHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		events, snapshot := hub.Subscribe(r.Context())

		// Send initial status event.
		head, _ := gs.HeadCommit()
		fmt.Fprintf(w, "event: status\ndata: {\"head\":\"%s\"}\n\n", head)

		// Replay snapshot (reconnect recovery).
		for _, ev := range snapshot {
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: task\ndata: %s\n\n", data)
		}
		flusher.Flush()

		// Periodic heartbeat pushes the current HEAD commit so the UI
		// detects external changes (e.g. MCP writes).
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-events:
				if !ok {
					return
				}
				ev, isTask := e.(TaskEvent)
				if !isTask {
					continue
				}
				data, _ := json.Marshal(ev)
				fmt.Fprintf(w, "event: task\ndata: %s\n\n", data)
				// After task completion, immediately push HEAD so the UI
				// refreshes without waiting for the next heartbeat.
				if ev.Status == "done" || ev.Status == "error" {
					head, _ := gs.HeadCommit()
					fmt.Fprintf(w, "event: status\ndata: {\"head\":\"%s\"}\n\n", head)
				}
				flusher.Flush()
			case <-ticker.C:
				head, _ := gs.HeadCommit()
				fmt.Fprintf(w, "event: status\ndata: {\"head\":\"%s\"}\n\n", head)
				flusher.Flush()
			}
		}
	}
}
