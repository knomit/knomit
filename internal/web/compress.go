package web

import (
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"knomit/internal/web/hal"
)

// compressibleTypes is the COMPLETE set of response content types knomit
// compresses, shared by the API router and the static routes so the two
// cannot drift apart.
//
// Complete is the load-bearing word. Passing any types to middleware.Compress
// REPLACES chi's built-in list rather than extending it, so a type left out
// here is served uncompressed — silently, with a 200 and every test green.
// That is precisely how the API went uncompressed for its entire life: chi's
// defaults carry application/json, and every knomit body is
// application/hal+json or application/problem+json. The comment beside the
// middleware claimed compression was mandatory; the wire said otherwise.
// See kb/gotchas/web/http/chi-compress-allowlist/72631d2b.md.
//
// chi matches the media type EXACTLY after cutting at ";" — no lowercasing,
// no trimming. The two types that are ours are referenced from the hal
// package rather than retyped, so the allowlist and the writers cannot
// disagree about a character.
//
// Two rules for anyone editing this list:
//
//   - NEVER add text/event-stream, and never reach for a "text/*" wildcard to
//     shorten the list. A compressor buffers, and a buffered SSE stream is a
//     stream that never arrives: measured at 10 bytes in 20 s through an
//     intermediary that gzipped one, where the same URL without
//     Accept-Encoding delivered 39 frames in 6 s. internal/web/sse.go sets
//     no-transform to fend off intermediaries; this list is what fends off
//     our own compressor. TestSSE_IsNeverCompressed guards it.
//     See kb/gotchas/web/sse/proxy-compression-stalls-streams/a762359e.md.
//   - Do not add already-compressed types — woff2, png, jpeg, gzip. They do
//     not get smaller, and the CPU is spent whether they shrink or not.
var compressibleTypes = []string{
	"text/html",
	"text/css",
	"text/plain",
	"text/javascript",
	"application/javascript",
	"application/json",
	hal.ContentType,        // application/hal+json
	hal.ProblemContentType, // application/problem+json
	"application/yaml",     // GET /api/v1/openapi.yaml — 173 KB of it
	"text/yaml",            // GET /api/v1/ontologies/... (sent with a charset)
	"image/svg+xml",
}

// compressor returns the response compressor used at both mount points: the
// API router (NewAPIRouter) and the static/SPA routes (Server.Handler).
//
// Exactly one of these may wrap any given request. Two would gzip the body
// twice — unreadable by every client — and would also put a second writer
// between sse.go's http.NewResponseController and the real connection.
//
// Level 5 is chi's suggested default: most of the ratio, little of the cost.
func compressor() func(http.Handler) http.Handler {
	return middleware.Compress(5, compressibleTypes...)
}
