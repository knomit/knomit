package metrics

import (
	"strings"
	"testing"
)

func TestCounter_DedupsByName(t *testing.T) {
	r := NewRegistry()
	a := r.Counter("knomit_x_total", "X.")
	b := r.Counter("knomit_x_total", "X.")
	if a != b {
		t.Fatal("Counter must return the same instance for a repeated name")
	}
	a.Inc()
	b.Add(2)
	var sb strings.Builder
	r.WriteProm(&sb)
	if !strings.Contains(sb.String(), "knomit_x_total 3") {
		t.Errorf("counter value wrong:\n%s", sb.String())
	}
}

func TestGauge_RendersTypeAndValue(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("knomit_live", "Live things.")
	g.Set(42)
	var sb strings.Builder
	r.WriteProm(&sb)
	out := sb.String()
	if !strings.Contains(out, "# TYPE knomit_live gauge") || !strings.Contains(out, "knomit_live 42") {
		t.Errorf("gauge render wrong:\n%s", out)
	}
}

func TestHistogram_RendersCumulativeBucketsSumCount(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("knomit_dur_seconds", "Durations.", []float64{0.1, 1, 10})
	h.Observe(0.05) // <= 0.1
	h.Observe(0.5)  // <= 1
	h.Observe(5)    // <= 10
	h.Observe(100)  // only +Inf

	var sb strings.Builder
	r.WriteProm(&sb)
	out := sb.String()

	if !strings.Contains(out, "# TYPE knomit_dur_seconds histogram") {
		t.Errorf("missing histogram TYPE:\n%s", out)
	}
	// Cumulative: le=0.1 has 1, le=1 has 2, le=10 has 3, +Inf has 4.
	for _, want := range []string{
		`knomit_dur_seconds_bucket{le="0.1"} 1`,
		`knomit_dur_seconds_bucket{le="1"} 2`,
		`knomit_dur_seconds_bucket{le="10"} 3`,
		`knomit_dur_seconds_bucket{le="+Inf"} 4`,
		`knomit_dur_seconds_count 4`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "knomit_dur_seconds_sum ") {
		t.Errorf("missing _sum line:\n%s", out)
	}
}

func TestLabeledCounter_RendersLabels(t *testing.T) {
	r := NewRegistry()
	c := r.CounterVec("knomit_requests_total", "Requests.", "route", "status")
	c.With("/facts/*", "200").Inc()
	c.With("/facts/*", "200").Inc()
	c.With("/search", "500").Inc()

	var sb strings.Builder
	r.WriteProm(&sb)
	out := sb.String()
	if !strings.Contains(out, `knomit_requests_total{route="/facts/*",status="200"} 2`) {
		t.Errorf("labeled counter wrong:\n%s", out)
	}
	if !strings.Contains(out, `knomit_requests_total{route="/search",status="500"} 1`) {
		t.Errorf("second series wrong:\n%s", out)
	}
}

func TestDefault_HasRuntimeGauges(t *testing.T) {
	var sb strings.Builder
	Default().WriteProm(&sb)
	out := sb.String()
	for _, want := range []string{"knomit_goroutines", "knomit_mem_alloc_bytes", "knomit_gc_total"} {
		if !strings.Contains(out, want) {
			t.Errorf("default registry missing runtime gauge %s", want)
		}
	}
}
