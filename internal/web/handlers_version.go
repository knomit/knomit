package web

import (
	"net/http"

	"knomit/internal/version"
	"knomit/internal/web/hal"
)

// handleVersion serves GET /api/v1/version — the build version of the running
// server. Reads directly from internal/version (global build metadata injected
// at link time), so it needs nothing from the Server.
func handleVersion(b hal.URLBuilder, readOnly bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"version":   version.Version,
			"commit":    version.Commit,
			"full":      version.String(),
			"read_only": readOnly,
			"_links": hal.LinkMap{
				"self": {Href: b.APIRoot() + "/version"},
			},
		}
		hal.WriteHAL(w, http.StatusOK, body)
	}
}
