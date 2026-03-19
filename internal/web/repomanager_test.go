package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestNewRepoManager_Empty(t *testing.T) {
	rm := NewRepoManager()
	if rm == nil {
		t.Fatal("expected non-nil RepoManager")
	}
	count := 0
	rm.ForEach(func(_ string, _ *RepoInstance) { count++ })
	if count != 0 {
		t.Fatalf("expected 0 repos, got %d", count)
	}
}

func TestSetAndGet(t *testing.T) {
	rm := NewRepoManager()
	ri := &RepoInstance{Name: "alpha"}
	rm.Set("alpha", ri)

	got := rm.Get("alpha")
	if got != ri {
		t.Fatal("Get did not return the instance that was Set")
	}
}

func TestGet_Unknown(t *testing.T) {
	rm := NewRepoManager()
	if rm.Get("nope") != nil {
		t.Fatal("expected nil for unknown repo")
	}
}

func TestReplace_ReturnsOld(t *testing.T) {
	rm := NewRepoManager()
	old := &RepoInstance{Name: "old"}
	rm.Set("r", old)

	replacement := &RepoInstance{Name: "new"}
	got := rm.Replace("r", replacement)
	if got != old {
		t.Fatal("Replace should return the previous instance")
	}
	if rm.Get("r") != replacement {
		t.Fatal("Replace should install the new instance")
	}
}

func TestReplace_NoOld(t *testing.T) {
	rm := NewRepoManager()
	ri := &RepoInstance{Name: "first"}
	got := rm.Replace("r", ri)
	if got != nil {
		t.Fatal("Replace with no prior entry should return nil")
	}
	if rm.Get("r") != ri {
		t.Fatal("Replace should install the instance even when there was no old one")
	}
}

func TestForEach(t *testing.T) {
	rm := NewRepoManager()
	rm.Set("a", &RepoInstance{Name: "a"})
	rm.Set("b", &RepoInstance{Name: "b"})
	rm.Set("c", &RepoInstance{Name: "c"})

	var names []string
	rm.ForEach(func(name string, _ *RepoInstance) {
		names = append(names, name)
	})
	sort.Strings(names)
	if len(names) != 3 || names[0] != "a" || names[1] != "b" || names[2] != "c" {
		t.Fatalf("ForEach visited unexpected set: %v", names)
	}
}

func TestRepoFromContext_Roundtrip(t *testing.T) {
	ri := &RepoInstance{Name: "test"}
	ctx := WithRepoInstance(context.Background(), ri)
	got := RepoFromContext(ctx)
	if got != ri {
		t.Fatal("roundtrip through context failed")
	}
}

func TestRepoFromContext_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when RepoInstance not in context")
		}
	}()
	RepoFromContext(context.Background())
}

func TestRepoMiddleware_NotFound(t *testing.T) {
	rm := NewRepoManager()

	r := chi.NewRouter()
	r.Route("/{repo}", func(r chi.Router) {
		r.Use(repoMiddleware(rm))
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
	rm := NewRepoManager()
	ri := &RepoInstance{Name: "myrepo"}
	rm.Set("myrepo", ri)

	var captured *RepoInstance
	r := chi.NewRouter()
	r.Route("/{repo}", func(r chi.Router) {
		r.Use(repoMiddleware(rm))
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			captured = RepoFromContext(req.Context())
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
