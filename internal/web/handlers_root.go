package web

import (
	"net/http"

	"knomit/internal/web/hal"
)

// handleAPIRoot serves GET /api/v1 — the discoverable HAL entry
// point. The response carries links to /repos and the OpenAPI spec.
//
// readOnly takes one link away rather than changing the shape: the log stream
// is refused outright on the demo (the server's own log is not part of what is
// on show, and free text has no useful redaction), and a link that is always a
// 403 makes discovery promise something the server will not do.
func handleAPIRoot(b hal.URLBuilder, readOnly bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		links := hal.LinkMap{
			"self":    {Href: b.APIRoot()},
			"repos":   {Href: b.Repos()},
			"version": {Href: b.APIRoot() + "/version"},
			"openapi": {Href: b.APIRoot() + "/openapi.yaml"},
		}
		if !readOnly {
			links["logs"] = hal.Link{Href: b.APIRoot() + "/logs/events"}
		}
		hal.WriteHAL(w, http.StatusOK, map[string]any{"_links": links})
	}
}
