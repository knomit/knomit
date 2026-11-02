package httpx

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"knomit/internal/edge/web/hal"
)

// DeclaredParams is the set of query parameters ONE GET route declares: the
// route answers 400 to any other (user ruling 2026-11-02: a parameter a REST
// route does not declare is refused, never silently ignored). Each set is the
// route's OpenAPI parameter list, aliases and paging included; the knowledge
// base's and the management API's tests compare the two, so a parameter added
// to one and not the other fails the build.
//
// It lives here, not in api/kb, because both api/kb and api/manage enforce it
// and an api package exports only Mount.
type DeclaredParams struct {
	names    map[string]bool
	prefixes []string // a family such as context.<key>
}

// DeclareParams builds a DeclaredParams. A name ending in ".<key>" (OpenAPI's
// spelling of a family, e.g. "context.<key>") declares every parameter that
// starts with the part before "<key>". No names declares none: every query
// parameter is refused.
func DeclareParams(names ...string) DeclaredParams {
	d := DeclaredParams{names: map[string]bool{}}
	for _, n := range names {
		if p, ok := strings.CutSuffix(n, "<key>"); ok {
			d.prefixes = append(d.prefixes, p)
			continue
		}
		d.names[n] = true
	}
	return d
}

// Has reports whether d declares the parameter k.
func (d DeclaredParams) Has(k string) bool {
	if d.names[k] {
		return true
	}
	for _, p := range d.prefixes {
		if strings.HasPrefix(k, p) && len(k) > len(p) {
			return true
		}
	}
	return false
}

// Names renders d back into OpenAPI's spelling (a family as "<prefix><key>"),
// sorted: what a test compares with the route's spec parameters.
func (d DeclaredParams) Names() []string {
	out := make([]string, 0, len(d.names)+len(d.prefixes))
	for n := range d.names {
		out = append(out, n)
	}
	for _, p := range d.prefixes {
		out = append(out, p+"<key>")
	}
	sort.Strings(out)
	return out
}

// RefuseUnknown answers 400 "Invalid parameter" for the first (in name order)
// query parameter d does not declare, with the FactSet routes' own detail
// text (`unknown parameter "<name>"`), and reports whether the request may
// proceed. A query string that does not parse (a bad %-escape, a `;`
// separator) is a 400 too: r.URL.Query() drops such a pair without a word,
// which would let it through as if it had never been sent.
func (d DeclaredParams) RefuseUnknown(w http.ResponseWriter, r *http.Request) bool {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeMalformed(w, r, err)
		return false
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !d.Has(k) {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", "unknown parameter "+strconv.Quote(k), r.URL.Path)
			return false
		}
	}
	return true
}

// RefuseMalformed answers a query string that does not parse with the same
// 400 RefuseUnknown gives it, for a route that refuses unknown names itself
// (the materialize sub-resources, with their own text).
func RefuseMalformed(w http.ResponseWriter, r *http.Request) bool {
	if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
		writeMalformed(w, r, err)
		return false
	}
	return true
}

func writeMalformed(w http.ResponseWriter, r *http.Request, err error) {
	hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", "malformed query string: "+err.Error(), r.URL.Path)
}
