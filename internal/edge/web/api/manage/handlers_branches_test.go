package manage

import "testing"

// TestIndexPercent pins the API's done/total → percent mapping: "ready" is
// always 100; "indexing"/"error" report done/total with a total==0 guard and
// a 100 clamp.
func TestIndexPercent(t *testing.T) {
	cases := []struct {
		state     string
		done, tot int
		want      int
	}{
		{"ready", 0, 0, 100},  // ready is always complete, even with no progress recorded
		{"ready", 3, 10, 100}, // ready overrides any stale progress
		{"indexing", 0, 0, 0}, // unknown total
		{"indexing", 0, 100, 0},
		{"indexing", 50, 100, 50},
		{"indexing", 568, 568, 100},
		{"indexing", 600, 568, 100}, // clamp: never exceed 100
		{"error", 25, 100, 25},      // error reports how far it got
	}
	for _, c := range cases {
		if got := indexPercent(c.state, c.done, c.tot); got != c.want {
			t.Errorf("indexPercent(%q, %d, %d) = %d, want %d", c.state, c.done, c.tot, got, c.want)
		}
	}
}
