package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestV2Router_UnknownRouteReturns404Problem(t *testing.T) {
	s := &Server{}
	r := s.NewV2Router()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/nonsense", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

func TestV2Router_MethodNotAllowedReturnsProblem(t *testing.T) {
	s := &Server{}
	r := s.NewV2Router()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/unknown", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 404 or 405", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}
