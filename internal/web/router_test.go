package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIRouter_UnknownRepoReturns404Problem(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

func TestServerHandler_MountsAPIRouter(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	h := s.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, APIBase+"/repos/missing", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}
