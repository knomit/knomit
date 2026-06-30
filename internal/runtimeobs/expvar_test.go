package runtimeobs

import (
	"encoding/json"
	"strings"
	"testing"

	"knomit/internal/metrics"
)

func TestExpvarMirrorsMetrics(t *testing.T) {
	// Record a metric, then ensure /debug/vars exposes it under the knomit key.
	metrics.Default().Counter("knomit_expvar_probe_total", "probe").Inc()
	NewServer(Options{}) // publishes the metrics expvar (idempotent)

	rr := serve(t, NewServer(Options{}), "GET", "/debug/vars")
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var vars map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &vars); err != nil {
		t.Fatalf("/debug/vars not JSON: %v", err)
	}
	raw, ok := vars["knomit"]
	if !ok {
		t.Fatalf("/debug/vars missing the knomit metrics key; keys=%v", keysOf(vars))
	}
	if !strings.Contains(string(raw), "knomit_expvar_probe_total") {
		t.Errorf("knomit expvar does not include the recorded metric:\n%s", raw)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
