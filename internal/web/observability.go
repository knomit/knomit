package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/obs/crashdump"
	"knomit/internal/obs/metrics"
	"knomit/internal/obs/reqinfo"
)

const (
	// maxLoggedQuery caps the raw query string in the slow-request warning.
	// Query strings carry user input (knomit_query text, long filter lists) and
	// are unbounded, so one pathological request must not dominate the log.
	maxLoggedQuery = 256
	// truncationSuffix marks a value the logger shortened, so a reader never
	// mistakes a cut-off query for the whole one.
	truncationSuffix = "...[truncated]"
)

// isStreamingResponse reports whether the handler produced a Server-Sent Events
// stream (SSE endpoints and the MCP streamable HTTP transport both set
// Content-Type: text/event-stream). Such responses are open for the lifetime of
// the subscription, so their elapsed time is not a latency signal.
func isStreamingResponse(w http.ResponseWriter) bool {
	return strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
}

// strIfSet adds key=val to the event, or leaves the event untouched when val is
// empty. Slow-request warnings are read by eye; a line padded with empty keys
// buries the fields that actually differ.
func strIfSet(ev *zerolog.Event, key, val string) *zerolog.Event {
	if val == "" {
		return ev
	}
	return ev.Str(key, val)
}

// truncateForLog shortens an unbounded, user-supplied value and marks the cut.
// The cut is pulled back to a rune boundary: a query string may carry raw UTF-8,
// and slicing mid-rune would emit invalid bytes into the JSON log.
func truncateForLog(s string) string {
	if len(s) <= maxLoggedQuery {
		return s
	}
	cut := s[:maxLoggedQuery]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + truncationSuffix
}

// urlParam reads a chi route parameter, tolerating a request that never went
// through a chi router (direct middleware use in tests, unmatched paths).
func urlParam(r *http.Request, key string) string {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		return ""
	}
	return rctx.URLParam(key)
}

// reportPanic writes a crash bundle for a recovered HTTP handler panic, then
// re-panics so the outer chi Recoverer still produces the 500 response. It must
// be registered INSIDE (after) middleware.Recoverer. http.ErrAbortHandler is
// passed through untouched per the net/http convention.
func reportPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec != http.ErrAbortHandler {
					pattern := chi.RouteContext(r.Context()).RoutePattern()
					if pattern == "" {
						pattern = r.URL.Path
					}
					crashdump.ReportRecovered("http:"+pattern, rec)
				}
				panic(rec) // let chi's Recoverer render the response
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// metricsMiddleware records a per-request counter and, when slowMS > 0, logs a
// WARN for any request slower than that threshold. Requests are labelled by the
// chi route PATTERN (e.g. /repos/{repo}), never the concrete path, so metric
// cardinality stays bounded regardless of how many distinct facts/repos exist.
func metricsMiddleware(reg *metrics.Registry, slowMS int) func(http.Handler) http.Handler {
	if reg == nil {
		reg = metrics.Default()
	}
	reqs := reg.CounterVec("knomit_http_requests_total", "HTTP requests by route, method, and status.", "route", "method", "status")
	slow := time.Duration(slowMS) * time.Millisecond

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			// Carry a mutable annotation into the handler so layers that only
			// learn what the request IS after parsing it — the MCP server, which
			// reads the tool name out of the JSON-RPC body — can report back to
			// this middleware. Attached unconditionally, including when the slow
			// log is off, so annotating is never conditional on config.
			ctx, info := reqinfo.NewContext(r.Context())
			r = r.WithContext(ctx)

			// Record in a defer so a panicking handler is still counted: the
			// panic unwinds through here on its way to reportPanic/the chi
			// Recoverer, which render the 500. No WriteHeader ran, so ww.Status()
			// is 0; we attribute it to the 500 the Recoverer will send.
			panicked := false
			defer func() {
				elapsed := time.Since(start)
				pattern := chi.RouteContext(r.Context()).RoutePattern()
				if pattern == "" {
					pattern = "other" // unmatched paths collapse to one series
				}
				status := ww.Status()
				switch {
				case panicked:
					status = http.StatusInternalServerError
				case status == 0:
					status = http.StatusOK // handler returned without WriteHeader
				}
				reqs.With(pattern, r.Method, strconv.Itoa(status)).Inc()

				// Streaming responses (SSE event streams, MCP streamable HTTP)
				// are long-lived by design — they stay open for the whole
				// subscription — so their elapsed time is meaningless as a
				// latency signal and would log a spurious WARN on every normal
				// disconnect. Exclude them from the slow-request log.
				if slow > 0 && elapsed > slow && !isStreamingResponse(ww) {
					ev := log.Warn().
						Str("route", pattern).
						Str("method", r.Method).
						Int("status", status).
						Dur("elapsed", elapsed).
						Str("path", r.URL.Path)
					// Everything below is omitted when absent: a plain REST line
					// stays as short as it was before these fields existed.
					ev = strIfSet(ev, "query", truncateForLog(r.URL.RawQuery))
					ev = strIfSet(ev, "req_id", middleware.GetReqID(r.Context()))
					for _, key := range []string{"repo", "branch", "lens"} {
						ev = strIfSet(ev, key, urlParam(r, key))
					}
					ev = strIfSet(ev, "remote", r.RemoteAddr)
					ev = strIfSet(ev, "ua", r.UserAgent())
					ev = strIfSet(ev, "mcp_session", r.Header.Get("Mcp-Session-Id"))
					ev = strIfSet(ev, "mcp_tool", info.Tool())
					ev.Msg("slow request")
				}
			}()

			// Mark a panic and re-raise so reportPanic/Recoverer still fire. This
			// recover runs BEFORE the recording defer above (LIFO: registered
			// later), so `panicked` is set when that defer reads it.
			defer func() {
				if rec := recover(); rec != nil {
					panicked = true
					panic(rec)
				}
			}()
			next.ServeHTTP(ww, r)
		})
	}
}
