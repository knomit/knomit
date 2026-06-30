package runtimeobs

import (
	"net/http"
	"strings"
	"testing"
)

func TestRegistry_CounterRendersProm(t *testing.T) {
	reg := NewRegistry()
	c := reg.NewCounter("knomit_requests_total", "Total requests.")
	c.Inc()
	c.Add(4)

	var sb strings.Builder
	reg.WriteProm(&sb)
	out := sb.String()

	if !strings.Contains(out, "# HELP knomit_requests_total Total requests.") {
		t.Errorf("missing HELP line:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE knomit_requests_total counter") {
		t.Errorf("missing TYPE line:\n%s", out)
	}
	if !strings.Contains(out, "knomit_requests_total 5") {
		t.Errorf("counter value wrong:\n%s", out)
	}
}

func TestRegistry_GaugeReflectsCollector(t *testing.T) {
	reg := NewRegistry()
	g := reg.NewGauge("knomit_goroutines", "Live goroutines.")
	reg.AddCollector(func() { g.Set(7) })

	var sb strings.Builder
	reg.WriteProm(&sb)

	if !strings.Contains(sb.String(), "knomit_goroutines 7") {
		t.Errorf("collector did not run before render:\n%s", sb.String())
	}
}

func TestMetricsEndpoint_ServesRuntimeGauges(t *testing.T) {
	s := NewServer(Options{})
	rr := serve(t, s, "GET", "/metrics")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// Runtime gauges are always present, even before any app instrumentation.
	for _, want := range []string{"knomit_goroutines", "knomit_mem_alloc_bytes", "knomit_gc_total"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %s:\n%s", want, body)
		}
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain (Prometheus exposition)", ct)
	}
}
