package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/metrics"
)

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
