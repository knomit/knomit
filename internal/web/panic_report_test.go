package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"knomit/internal/obs/crashdump"
)

func TestReportPanicMiddleware_WritesBundleAndStill500s(t *testing.T) {
	dir := t.TempDir()
	crashdump.SetGlobalReporter(crashdump.New(dir, nil))
	defer crashdump.SetGlobalReporter(nil)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer) // outer: produces the 500 response
	r.Use(reportPanic)          // inner: captures a crash bundle, re-panics
	r.Get("/boom/{id}", func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	})

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/boom/42", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (chi Recoverer must still produce the response)", rr.Code)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read crash dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d crash bundles, want 1", len(entries))
	}
	// Labelled by route PATTERN, not the concrete /boom/42 path.
	if !strings.Contains(entries[0].Name(), "boom") {
		t.Errorf("bundle filename does not reflect the route: %s", entries[0].Name())
	}
}
