package web

import (
	"fmt"
	"net/http"
	"time"
)

// sseWriteTimeout bounds a single SSE write. Generous: it guards against a
// client that has stopped reading entirely, not a latency budget, and a
// legitimately slow network must not be mistaken for a dead one.
const sseWriteTimeout = 30 * time.Second

// sseStream is the writer every Server-Sent Events handler in this package
// goes through. It exists so the rule below is stated once and cannot be
// half-applied by the next stream someone adds.
//
// THE RULE: bound every write with a deadline, AND check every error.
//
// The deadline, because the servers run with WriteTimeout 0 — deliberately,
// since any non-zero value would cut every SSE stream at the timeout — and the
// event hubs never block on a slow subscriber. Nothing else bounds a write, so
// a client that stops draining its socket wedges the handler's Fprintf
// indefinitely, holding the goroutine and the subscription while events pile up
// behind it.
//
// The error check, because a deadline WITHOUT one inverts the failure rather
// than fixing it: the deadline fires, the write fails, and a loop that ignores
// the error spins on a dead connection at full event rate. A handler that
// returns is what ends the subscription — net/http cancels the request context
// on return, which is what every hub's Subscribe(ctx) is waiting on.
//
// Refusing to start when SetWriteDeadline itself fails is the safe direction,
// but it is not free: http.NewResponseController reaches the connection only
// through Unwrap, and both middleware.Compress and metricsMiddleware wrap the
// ResponseWriter, so a wrapper that stopped unwrapping would turn this into a
// silently dead endpoint. That is why each stream has a test that runs over a
// real server through the whole chain — those are what catch it, not this
// comment.
type sseStream struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	flusher http.Flusher
	// dead latches on the first failure so a caller that cannot return
	// immediately still stops writing. Without it, "check the error" would
	// only help the handlers shaped to return on it.
	dead bool
}

// startSSE sets the SSE headers and returns the stream. ok is false when it has
// already written the error response, which is the same 500 the handlers wrote
// individually before.
func startSSE(w http.ResponseWriter) (*sseStream, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	// no-transform and X-Accel-Buffering are what keep a stream a STREAM once
	// there is an intermediary, and neither is optional in practice.
	//
	// An SSE response is text/*, so a proxy that compresses by content type
	// will happily gzip it — and a compressor buffers. The whole stream then
	// sits in that buffer instead of arriving: measured through code-server's
	// Express `compression` (the maintainer reaches knomit via Tailscale →
	// code-server → /proxy/<port>/), a browser's Accept-Encoding got 10 bytes
	// in 20s — the gzip header, nothing else — where the same URL without
	// Accept-Encoding delivered 39 frames in 6s.
	//
	// `no-transform` is the standard way to forbid that (RFC 9111 §5.2.2.6),
	// and it is exactly what compression's shouldTransform tests for. It must
	// sit at a comma boundary to match, which is why it is a separate
	// directive here rather than glued to no-cache.
	//
	// X-Accel-Buffering: no is the nginx-family equivalent, for proxies that
	// buffer without compressing. Non-standard, ignored by everything that
	// does not know it, and the one header that reaches the other big class of
	// intermediary.
	//
	// The failure this prevents is silent and looks like the app: every frame
	// is written, flushed and accepted, the connection stays open, and the
	// browser simply never receives anything. A stream with a poll fallback
	// (Sessions) degrades to the poll and hides it; one without (Logs) shows
	// an empty pane forever.
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	return &sseStream{w: w, rc: http.NewResponseController(w), flusher: flusher}, true
}

// Write bounds one write, checks it, flushes, and reports whether the stream is
// still usable. CALLERS MUST RETURN ON false — that is what ends the
// subscription. Once it has returned false it returns false for good, so a
// caller that cannot return early is merely writing nothing rather than
// blocking.
//
// format is a FORMAT STRING: pass the payload as an argument, never as the
// format, or a '%' inside a log line or a commit message becomes a verb.
func (s *sseStream) Write(format string, args ...any) bool {
	if s.dead {
		return false
	}
	if err := s.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil {
		s.dead = true
		return false
	}
	if _, err := fmt.Fprintf(s.w, format, args...); err != nil {
		s.dead = true
		return false
	}
	s.flusher.Flush()
	return true
}

// Keepalive writes the comment frame that stops a proxy cutting an idle
// stream. It is bounded like any other write: on a stream whose client has
// gone, the keepalive is usually the write that finds out.
func (s *sseStream) Keepalive() bool {
	return s.Write(": keepalive\n\n")
}
