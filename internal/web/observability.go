package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"

	"knomit/internal/crashdump"
	"knomit/internal/metrics"
)

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
			next.ServeHTTP(ww, r)

			elapsed := time.Since(start)
			pattern := chi.RouteContext(r.Context()).RoutePattern()
			if pattern == "" {
				pattern = "other" // unmatched paths collapse to one series
			}
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK // handler returned without WriteHeader
			}
			reqs.With(pattern, r.Method, strconv.Itoa(status)).Inc()

			if slow > 0 && elapsed > slow {
				log.Warn().
					Str("route", pattern).
					Str("method", r.Method).
					Int("status", status).
					Dur("elapsed", elapsed).
					Msg("slow request")
			}
		})
	}
}
