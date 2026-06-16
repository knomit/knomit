package web

import "net/http"

// corsMiddleware allows the given origins, handles preflight, and exposes the
// headers the SPA needs (including EventSource GETs and JSON POSTs). It is used
// only by the desktop API-only build, whose UI loads from a Wails origin such
// as "wails://localhost" and calls the API cross-origin over looknomitck TCP.
// Cloud passes no origins, so this middleware is not installed there.
//
// Requests are gated by the in-process looknomitck listener and knomit uses no
// cookies, so the allow-list is the only cross-origin control needed.
func corsMiddleware(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		set[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && set[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
