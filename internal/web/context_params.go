package web

import (
	"net/http"
	"strings"

	knomitfact "knomit/internal/fact"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// contextParamPrefix is the query-string prefix of the F22 context filter:
// `context.task=t-17&context.verdict=disagree`.
const contextParamPrefix = "context."

// applyContextParams reads the F22 context filter from the query string into
// q.Context: every `context.<key>=<value>` pair must match exactly (the stored
// canonical text — a number in shortest form, true/false). It is the REST twin
// of knomit_query's `context` object and shares its store filter. Returns false
// after writing a 400 for a key that is not a legal context key, a repeated
// key, or a value that could never be stored (not one line, too long).
func applyContextParams(w http.ResponseWriter, r *http.Request, q *store.SearchOptions) bool {
	for name, vals := range r.URL.Query() {
		key, ok := strings.CutPrefix(name, contextParamPrefix)
		if !ok {
			continue
		}
		if !knomitfact.ValidContextKey(key) {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				name+": a context key must match [a-z][a-z0-9_]* and be at most 32 characters", r.URL.Path)
			return false
		}
		if len(vals) != 1 {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				name+": give each context key once", r.URL.Path)
			return false
		}
		if err := knomitfact.ValidateContextShape(map[string]any{key: vals[0]}); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", err.Error(), r.URL.Path)
			return false
		}
		if q.Context == nil {
			q.Context = map[string]string{}
		}
		q.Context[key] = vals[0]
	}
	return true
}
