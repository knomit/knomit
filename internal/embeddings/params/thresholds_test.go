package params

import "testing"

// TestDefaults pins the historical nomic-era values. These are the fallback
// when no embedder is configured, so they must stay stable; a change here is a
// behavioural change to dedup/search/graph for embeddings-disabled deployments.
func TestDefaults(t *testing.T) {
	d := Defaults()
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"Dedup", d.Dedup, 0.92},
		{"ReflectNovelty", d.ReflectNovelty, 0.85},
		{"SimilarTo", d.SimilarTo, 0.60},
		{"SearchFloor", d.SearchFloor, 0.40},
		{"RerankHigh", d.RerankHigh, 0.70},
		{"RerankLow", d.RerankLow, 0.50},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("Defaults().%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestDefaultsRerankOrdering guards the invariant the rerank tiers rely on.
func TestDefaultsRerankOrdering(t *testing.T) {
	d := Defaults()
	if !(d.RerankHigh > d.RerankLow) {
		t.Errorf("RerankHigh (%v) must exceed RerankLow (%v)", d.RerankHigh, d.RerankLow)
	}
}
