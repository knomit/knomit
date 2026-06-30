package metrics

import "testing"

func TestRegistry_Snapshot(t *testing.T) {
	r := NewRegistry()
	r.Counter("c_total", "").Add(3)
	r.Gauge("g", "").Set(5)
	r.CounterVec("v_total", "", "route").With("/x").Inc()
	h := r.Histogram("h_seconds", "", []float64{1})
	h.Observe(0.5)

	snap := r.Snapshot()

	if snap["c_total"] != int64(3) {
		t.Errorf("counter snapshot = %v, want 3", snap["c_total"])
	}
	if snap["g"] != int64(5) {
		t.Errorf("gauge snapshot = %v, want 5", snap["g"])
	}
	vec, ok := snap["v_total"].(map[string]int64)
	if !ok || vec[`route="/x"`] != 1 {
		t.Errorf("vec snapshot = %v, want {route=\"/x\": 1}", snap["v_total"])
	}
	hist, ok := snap["h_seconds"].(map[string]any)
	if !ok || hist["count"] != int64(1) {
		t.Errorf("histogram snapshot = %v, want count 1", snap["h_seconds"])
	}
}

func TestRegistry_SnapshotRunsCollectors(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("live", "")
	r.AddCollector(func() { g.Set(99) })
	if got := r.Snapshot()["live"]; got != int64(99) {
		t.Errorf("collector did not run before snapshot: %v", got)
	}
}
