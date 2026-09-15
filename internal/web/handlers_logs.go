package web

import (
	"encoding/json"
	"net/http"
	"time"

	"knomit/internal/platform/logging"
	"knomit/internal/web/hal"
)

// logLine is one log line on the wire. JSON-encoded rather than interpolated
// into the frame: a log line is arbitrary text, and a line containing a
// newline followed by "event:" would otherwise forge an SSE frame of its own.
type logLine struct {
	Line string `json:"line"`
}

// logReady tells the client the backlog is complete, and how deep it could
// have been — so a viewer can say "this is everything the server kept" rather
// than leaving the reader to guess whether the log really started there.
type logReady struct {
	Retained int `json:"retained"`
	Max      int `json:"max"`
}

// logDropped reports lines this subscriber missed by being too slow. It exists
// because the alternative — resuming quietly — presents a discontinuous log as
// a continuous one, which is the one thing a log viewer must never do.
type logDropped struct {
	Count uint64 `json:"count"`
}

// handleLogEvents serves GET /api/v1/logs/events — the Manage Logs tab's
// source. On connect it replays the tap's retained lines oldest-first, sends
// `ready`, then streams each new line; `dropped` reports a gap; a `: keepalive`
// comment every 30s keeps proxies from cutting an idle stream.
//
// It taps the running process's LOG, not the log file. The desktop app tails
// the file because it wants history from before the window opened, including a
// previous run's failed startup; a web tab can only ever show a RUNNING server,
// and a bare `knomit serve` may have no file sink at all.
//
// Served from the ordinary API mount on purpose: the desktop reaches it through
// the same base /config.js injects for everything else, so the log stream is
// part of the API rather than a second, desktop-only transport.
//
// 403 under ReadOnly. The read-only demo is public and the server's own log is
// not part of what is on show — it is the one read on this server that is
// refused rather than redacted, because there is no useful redaction of free
// text. 503 when no tap is wired, exactly as the sessions endpoints do without
// their store.
func handleLogEvents(tap *logging.Tap, readOnly bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if readOnly {
			hal.WriteProblem(w, http.StatusForbidden, "Read-only instance",
				"this knomit instance is running in read-only (demo) mode; the server log is not served",
				r.URL.Path)
			return
		}
		if tap == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Logs unavailable",
				"this server was built without a log tap", r.URL.Path)
			return
		}
		stream, ok := startSSE(w)
		if !ok {
			return
		}

		// Backlog and live channel together, under the tap's lock: a line
		// published between the two would otherwise be missed by both or sent
		// twice, and neither is distinguishable from a gap in the log.
		sub, backlog := tap.Subscribe(r.Context())

		// send is the only thing this handler adds to sseStream.Write: the
		// marshal, and the decision that a payload we cannot encode is skipped
		// rather than fatal. The bound itself lives in sse.go — and it matters
		// most here, because the log is the highest-rate stream this server
		// has, so a client that stops draining backs up fastest.
		send := func(event string, payload any) bool {
			data, err := json.Marshal(payload)
			if err != nil {
				return true // a line we cannot encode is skipped, not fatal
			}
			return stream.Write("event: %s\ndata: %s\n\n", event, data)
		}

		for _, line := range backlog {
			if !send("line", logLine{Line: line}) {
				return
			}
		}
		if !send("ready", logReady{Retained: len(backlog), Max: tap.Max()}) {
			return
		}

		// Reported as a DELTA the client can add up, and tracked here rather
		// than sent from the tap, because drops are a property of this one
		// subscription falling behind.
		var reported uint64
		reportDrops := func() bool {
			if d := sub.Dropped(); d > reported {
				if !send("dropped", logDropped{Count: d - reported}) {
					return false
				}
				reported = d
			}
			return true
		}

		keepalive := time.NewTicker(30 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case line := <-sub.Lines():
				if !send("line", logLine{Line: line}) {
					return
				}
				// After the line, not before: a drop that happened while this
				// one was queued belongs after the last line that survived, so
				// the marker lands where the gap actually is.
				if !reportDrops() {
					return
				}
			case <-keepalive.C:
				// An idle stream is also where a stalled client catches up, so
				// this is the other place a gap can become reportable.
				if !reportDrops() {
					return
				}
				if !stream.Keepalive() {
					return
				}
			}
		}
	}
}
