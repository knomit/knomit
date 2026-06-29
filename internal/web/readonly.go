package web

import (
	"net/http"
	"regexp"

	"knomit/internal/web/hal"
)

// mcpRoutePattern matches the MCP dispatch endpoint and its subtree, which is
// POST-for-reads and must bypass the read-only method gate. Read-only-ness for
// MCP is enforced by tool filtering in mcp.NewServer instead. Paths are matched
// without the APIBase prefix (the gate runs inside the API router).
var mcpRoutePattern = regexp.MustCompile(`/branches/[^/]+/mcp(/|$)`)

// isMutatingRequest reports whether a request would mutate state and therefore
// must be rejected in read-only mode. Mutating HTTP methods are gated unless
// the path is the MCP dispatch route.
func isMutatingRequest(method, path string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return !mcpRoutePattern.MatchString(path)
	default:
		return false
	}
}

// readOnlyGate rejects mutating requests with 403 problem+json. Mounted only
// when the server is in read-only mode.
func readOnlyGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMutatingRequest(r.Method, r.URL.Path) {
			hal.WriteProblem(w, http.StatusForbidden, "Read-only instance",
				"this knomit instance is running in read-only (demo) mode; mutations are disabled",
				r.URL.Path)
			return
		}
		next.ServeHTTP(w, r)
	})
}
