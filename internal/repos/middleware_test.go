package repos_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
)

func TestRepoMiddleware_NotFound(t *testing.T) {
	m := emptyManager()

	r := chi.NewRouter()
	r.Route("/{repo}", func(r chi.Router) {
		r.Use(repos.RepoMiddleware(m))
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/unknown", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON body: %v", err)
	}
	if body["error"] == "" {
		t.Fatal("expected non-empty error field in JSON response")
	}
}

func TestRepoMiddleware_SetsContext(t *testing.T) {
	m := emptyManager()
	ri := makeRI("myrepo")
	m.Set("myrepo", ri)

	var captured *repos.RepoInstance
	r := chi.NewRouter()
	r.Route("/{repo}", func(r chi.Router) {
		r.Use(repos.RepoMiddleware(m))
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			captured = repos.RepoFromContext(req.Context())
			w.WriteHeader(http.StatusOK)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/myrepo", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if captured != ri {
		t.Fatal("middleware did not set correct RepoInstance in context")
	}
}
