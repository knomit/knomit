package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testOrigin = "wails://localhost"

// allowedMethods returns the methods advertised in the preflight response for
// an allowed origin.
func allowedMethods(t *testing.T) string {
	t.Helper()
	h := corsMiddleware([]string{testOrigin})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/repos/x/origin/upstream", nil)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPatch)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Header().Get("Access-Control-Allow-Methods")
}

// Regression: the UI's setOriginUpstream issues a cross-origin PATCH
// (web/src/api.ts), which the desktop build's preflight must allow — otherwise
// the browser blocks the request and "change upstream" silently fails.
func TestCORS_PreflightAllowsPATCH(t *testing.T) {
	methods := allowedMethods(t)
	if !strings.Contains(methods, http.MethodPatch) {
		t.Errorf("Allow-Methods = %q, must include PATCH", methods)
	}
	// The other verbs the UI uses cross-origin must remain allowed.
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		if !strings.Contains(methods, m) {
			t.Errorf("Allow-Methods = %q, must include %s", methods, m)
		}
	}
}

func TestCORS_DisallowedOriginGetsNoHeaders(t *testing.T) {
	h := corsMiddleware([]string{testOrigin})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin for disallowed origin = %q, want empty", got)
	}
}
