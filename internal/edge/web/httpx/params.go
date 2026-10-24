package httpx

import "net/http"

// SelfWithQuery returns base plus the request's raw query string, for the
// _links.self of a filterable collection: the self link of a filtered view
// must reproduce the filter, or following it lands somewhere else.
func SelfWithQuery(base string, r *http.Request) string {
	if r.URL.RawQuery != "" {
		return base + "?" + r.URL.RawQuery
	}
	return base
}
