package web

import (
	"net/http"

	"knomit/internal/web/hal"
)

// handleV2APIRoot serves GET /api/v1-new — the discoverable HAL entry
// point. The response carries links to /repos and the OpenAPI spec.
func handleV2APIRoot(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"_links": hal.LinkMap{
				"self":    {Href: b.APIRoot()},
				"repos":   {Href: b.Repos()},
				"openapi": {Href: b.APIRoot() + "/openapi.yaml"},
			},
		}
		hal.WriteHAL(w, http.StatusOK, body)
	}
}
