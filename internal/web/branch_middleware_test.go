package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestBranchMiddleware_DecodesColonToSlash(t *testing.T) {
	r := chi.NewRouter()
	r.With(BranchMiddleware).Get("/b/{branch}", func(w http.ResponseWriter, req *http.Request) {
		got := BranchFromContext(req.Context())
		if _, err := w.Write([]byte(got)); err != nil {
			t.Fatalf("write: %v", err)
		}
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/b/agent:test", nil)
	r.ServeHTTP(rec, req)

	if rec.Body.String() != "agent/test" {
		t.Errorf("got %q, want %q", rec.Body.String(), "agent/test")
	}
}

func TestBranchMiddleware_PassesThroughPlainNames(t *testing.T) {
	r := chi.NewRouter()
	r.With(BranchMiddleware).Get("/b/{branch}", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(BranchFromContext(req.Context())))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/b/main", nil)
	r.ServeHTTP(rec, req)
	if rec.Body.String() != "main" {
		t.Errorf("got %q", rec.Body.String())
	}
}

func TestBranchFromContext_PanicsWhenMissing(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic when branch is not in context")
		}
	}()
	_ = BranchFromContext(httptest.NewRequest(http.MethodGet, "/", nil).Context())
}
