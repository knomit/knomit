package web

import (
	"net/http"

	"knomit/internal/web/hal"
)

// handleAPIRoot serves GET /api/v1 — the discoverable HAL entry
// point. The response carries links to /repos and the OpenAPI spec.
func handleAPIRoot(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"_links": hal.LinkMap{
				"self":    {Href: b.APIRoot()},
				"repos":   {Href: b.Repos()},
				"version": {Href: b.APIRoot() + "/version"},
				"openapi": {Href: b.APIRoot() + "/openapi.yaml"},
				// The server's own log, as a stream. Refused (403) on a
				// read-only instance, so the link is an offer the demo
				// declines rather than a promise it keeps.
				"logs": {Href: b.APIRoot() + "/logs/events"},
			},
		}
		hal.WriteHAL(w, http.StatusOK, body)
	}
}
