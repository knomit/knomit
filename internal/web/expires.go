package web

import (
	"net/http"
	"strconv"
	"time"

	knomitfact "knomit/internal/fact"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// applyExpiryParams reads the F03 expiry filters from the query string into q
// and stamps q.Now with the request's ONE clock, which the handler then uses
// for every row's `expired` marker. Nothing is ever hidden by default: without
// these params expired facts come back like any other, marked.
//
//	expired=true|false   true: expires <= now. false: not expired — INCLUDES
//	                     facts with no expires (absent means never).
//	expires_before=T     expires < T. EXCLUDES facts with no expires.
//	expires_after=T      expires > T. EXCLUDES facts with no expires.
//
// "Expiring within w" is expires_after=<now>&expires_before=<now+w>. Returns
// false after writing a 400 for a malformed value.
func applyExpiryParams(w http.ResponseWriter, r *http.Request, q *store.SearchOptions, now time.Time) bool {
	q.Now = now
	qp := r.URL.Query()
	if v := qp.Get("expired"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				"expired must be true or false", r.URL.Path)
			return false
		}
		q.Expired = &b
	}
	for _, p := range []struct {
		key string
		dst *time.Time
	}{{"expires_before", &q.ExpiresBefore}, {"expires_after", &q.ExpiresAfter}} {
		v := qp.Get(p.key)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				p.key+" must be an RFC 3339 timestamp with an offset, e.g. 2026-10-01T00:00:00Z", r.URL.Path)
			return false
		}
		*p.dst = t
	}
	return true
}

// expiredAt is the row marker: true when expires is at or before now, judged
// exactly as the store's filter judges it (whole seconds).
func expiredAt(expires string, now time.Time) bool { return knomitfact.IsExpiredAt(expires, now) }
