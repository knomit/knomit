package web

import (
	"net/http"
	"strconv"
	"strings"

	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// Query-parameter parsing for the API, in one place.
//
// Before this file existed each collection endpoint carried its own inline
// parser, and they had silently diverged: some 400'd on a non-integer limit,
// some fell back to the default; some rejected a negative limit, some passed
// it through to the store, where `limit <= 0` clamps to 100 — so
// `?limit=-5` returned MORE rows than the default. One shared parser per
// parameter is the only way those variants stay converged.

const (
	// defaultLimit is the page size when ?limit= is absent.
	defaultLimit = 50
	// maxLimit is the largest page any endpoint will serve. It is a hard
	// ceiling, not a clamp: asking for more is a 400. Silently returning
	// fewer rows than requested is the same silent-success failure the
	// strict parsing here exists to eliminate — a client that asked for 900
	// and got 500 has no way to tell that from "there were only 500".
	maxLimit = 500
)

// limitParam parses ?limit= with the API-wide semantics: absent → 50,
// otherwise an integer in [1, 500]. Anything else — non-integer, zero,
// negative, or above the ceiling — writes a 400 problem and returns
// ok=false, in which case the caller must return immediately without
// touching w again.
func limitParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return defaultLimit, true
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, badParam(w, r, "invalid limit value")
	}
	if n < 1 {
		return 0, badParam(w, r, "limit must be at least 1")
	}
	if n > maxLimit {
		return 0, badParam(w, r, "limit must not exceed "+strconv.Itoa(maxLimit))
	}
	return n, true
}

// offsetParam parses ?offset= : absent → 0, otherwise a non-negative
// integer. Non-integer or negative writes a 400 and returns ok=false.
func offsetParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := r.URL.Query().Get("offset")
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, badParam(w, r, "invalid offset value")
	}
	if n < 0 {
		return 0, badParam(w, r, "offset must not be negative")
	}
	return n, true
}

// badParam writes the shared 400 envelope and returns false, so callers can
// write `return 0, badParam(...)` in one line.
func badParam(w http.ResponseWriter, r *http.Request, detail string) bool {
	hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", detail, r.URL.Path)
	return false
}

// splitCSV splits a comma-separated query value, trimming whitespace around
// each item and dropping empty ones. Returns nil for "", so an absent filter
// and an all-empty one are indistinguishable to the store — which is what
// every caller wants.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// selfWithQuery returns base plus the request's raw query string, for the
// _links.self of a filterable collection: the self link of a filtered view
// must reproduce the filter, or following it lands somewhere else.
func selfWithQuery(base string, r *http.Request) string {
	if r.URL.RawQuery != "" {
		return base + "?" + r.URL.RawQuery
	}
	return base
}

// motifParams parses the motifs / motif_match query parameters shared by the
// repo and lens search + facts collections.
//
// The REST surface accepts only the tiers safe outside a deliberate reader
// session: exact (default), stem, token-2. The looser tiers are deliberately
// not named anywhere in this package — the validation is an ALLOWLIST, and the
// MN6 AST guard (internal/mcp/motif_surfaces_test.go) scans internal/web to
// keep it that way. The error message therefore lists what IS accepted and
// stays silent about what exists beyond it.
//
// motif_match without motifs parses fine and stays inert, matching the MCP
// tool and the store (SearchOptions.MotifMatch does nothing without Motifs).
func motifParams(w http.ResponseWriter, r *http.Request) (motifs []string, tier store.MotifMatchTier, ok bool) {
	qp := r.URL.Query()
	motifs = splitCSV(qp.Get("motifs"))
	switch v := qp.Get("motif_match"); v {
	case "":
		tier = store.MotifMatchExact
	case string(store.MotifMatchExact), string(store.MotifMatchStem), string(store.MotifMatchToken2):
		tier = store.MotifMatchTier(v)
	default:
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
			`invalid motif_match value (accepted: "exact", "stem", "token-2")`, r.URL.Path)
		return nil, "", false
	}
	return motifs, tier, true
}
