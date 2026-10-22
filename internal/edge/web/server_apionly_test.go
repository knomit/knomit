package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAPIOnly_NoSPA_AndCORS verifies the desktop build's server mode: the SPA
// routes are omitted (unknown routes return an API-consistent problem+json 404)
// and the configured Wails origin is allowed via CORS.
func TestAPIOnly_NoSPA_AndCORS(t *testing.T) {
	s := &Server{
		Manager:     newTestManagerWithRepos(t),
		APIOnly:     true,
		CORSOrigins: []string{"wails://localhost"},
	}
	h := s.Handler()

	// SPA root is not served — API-only returns problem+json, not HTML/text.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET / in API-only: got %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("GET / in API-only content-type: got %q, want application/problem+json", ct)
	}

	// CORS preflight from the Wails origin is allowed.
	req := httptest.NewRequest(http.MethodOptions, APIBase+"/repos", nil)
	req.Header.Set("Origin", "wails://localhost")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status: got %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "wails://localhost" {
		t.Errorf("ACAO: got %q, want wails://localhost", got)
	}
}

// TestServeUI_DefaultMode_NoCORS_SPARoutes verifies cloud defaults are
// unchanged: SPA routes mounted (GET / handled by the SPA, not the API-only
// problem+json 404) and no CORS headers when no origins are configured.
func TestServeUI_DefaultMode_NoCORS_SPARoutes(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)} // APIOnly false, no CORS
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if ct := rec.Header().Get("Content-Type"); ct == "application/problem+json" {
		t.Error("default mode GET / must be handled by the SPA, not the API-only 404")
	}

	// No Origin reflection when no CORS origins are configured.
	req := httptest.NewRequest(http.MethodGet, APIBase+"/repos/missing", nil)
	req.Header.Set("Origin", "wails://localhost")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("default mode must not set ACAO, got %q", got)
	}
}
