package diag

import (
	"net/http"
	"strings"
	"testing"
)

func TestMetricsEndpoint_ServesRuntimeGauges(t *testing.T) {
	rr := serve(t, NewServer(Options{}), "GET", "/metrics")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"knomit_goroutines", "knomit_mem_alloc_bytes", "knomit_gc_total"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %s:\n%s", want, body)
		}
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain (Prometheus exposition)", ct)
	}
}
