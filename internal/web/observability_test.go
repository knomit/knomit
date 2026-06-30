package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/metrics"
)

func TestMetricsMiddleware_CountsPanicAs500(t *testing.T) {
	reg := metrics.NewRegistry()
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)      // outer: catches the re-panic, renders 500
	r.Use(metricsMiddleware(reg, 0)) // inner: must still count the request
	r.Get("/boom/{id}", func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	})

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/boom/42", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}

	var sb strings.Builder
	reg.WriteProm(&sb)
	out := sb.String()
	// Labelled by route PATTERN and attributed to the 500 the Recoverer sent —
	// a panicking handler must not vanish from request metrics.
	want := `knomit_http_requests_total{route="/boom/{id}",method="GET",status="500"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}
}

func TestMetricsMiddleware_CountsByRoutePattern(t *testing.T) {
	reg := metrics.NewRegistry()
	r := chi.NewRouter()
	r.Use(metricsMiddleware(reg, 0))
	r.Get("/repos/{repo}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Two requests to the SAME pattern but different concrete paths must
	// collapse into one series (bounded cardinality).
	for _, p := range []string{"/repos/core", "/repos/other"} {
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest("GET", p, nil))
	}

	var sb strings.Builder
	reg.WriteProm(&sb)
	out := sb.String()
	want := `knomit_http_requests_total{route="/repos/{repo}",method="GET",status="200"} 2`
	if !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}
}

func TestMetricsMiddleware_SlowRequestLogs(t *testing.T) {
	var buf strings.Builder
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	defer func() { log.Logger = orig }()

	reg := metrics.NewRegistry()
	r := chi.NewRouter()
	r.Use(metricsMiddleware(reg, 1)) // 1ms threshold
	r.Get("/slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/slow", nil))

	if !strings.Contains(buf.String(), "slow request") {
		t.Errorf("expected a slow-request warning, got:\n%s", buf.String())
	}
}

func TestMetricsMiddleware_SlowDisabledWhenZero(t *testing.T) {
	var buf strings.Builder
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	defer func() { log.Logger = orig }()

	reg := metrics.NewRegistry()
	r := chi.NewRouter()
	r.Use(metricsMiddleware(reg, 0)) // disabled
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))

	if strings.Contains(buf.String(), "slow request") {
		t.Errorf("slow-request log fired when threshold disabled:\n%s", buf.String())
	}
}

func TestMetricsMiddleware_StreamingNotLoggedAsSlow(t *testing.T) {
	var buf strings.Builder
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	defer func() { log.Logger = orig }()

	reg := metrics.NewRegistry()
	r := chi.NewRouter()
	r.Use(metricsMiddleware(reg, 1)) // 1ms threshold
	// An SSE handler: sets text/event-stream and stays "open" past the
	// threshold. It must NOT be reported as a slow request.
	r.Get("/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(10 * time.Millisecond)
	})

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/stream", nil))

	if strings.Contains(buf.String(), "slow request") {
		t.Errorf("streaming (text/event-stream) response logged as slow:\n%s", buf.String())
	}
	// It must still be counted as a request, just not flagged slow.
	var sb strings.Builder
	reg.WriteProm(&sb)
	if !strings.Contains(sb.String(), `knomit_http_requests_total{route="/stream",method="GET",status="200"} 1`) {
		t.Errorf("streaming request was not counted:\n%s", sb.String())
	}
}
