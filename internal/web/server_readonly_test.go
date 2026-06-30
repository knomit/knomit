package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func stubGit() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("git"))
	})
}

func TestHandler_ReadOnly_DoesNotMountGit(t *testing.T) {
	// APIOnly avoids the SPA catch-all (which would redirect /git/* to / via
	// the embedded http.FileServer's index-file redirect), ensuring a clean 404.
	s := &Server{GitHandler: stubGit(), ReadOnly: true, APIOnly: true}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/git/core/info/refs", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("read-only /git status = %d, want 404", rec.Code)
	}
}

func TestHandler_Writable_MountsGit(t *testing.T) {
	s := &Server{GitHandler: stubGit(), ReadOnly: false}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/git/core/info/refs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("writable /git status = %d, want 200", rec.Code)
	}
}
