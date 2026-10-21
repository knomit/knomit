package index

import "testing"

// TestSimilarityGraph_DensityAndConnected is a pure unit test of the
// SimilarityGraph value type using a hand-built adjacency map. This ensures
// the Density/Connected math is covered independently of edge formation.
func TestSimilarityGraph_DensityAndConnected(t *testing.T) {
	// Build a graph with a↔b and b↔c edges (not a↔c).
	g := SimilarityGraph{adj: map[string]map[string]struct{}{
		"a": {"b": {}},
		"b": {"a": {}, "c": {}},
		"c": {"b": {}},
	}}

	// Connected checks (symmetric).
	if !g.Connected("a", "b") {
		t.Error("expected a-b connected")
	}
	if !g.Connected("b", "a") {
		t.Error("expected b-a connected (symmetric)")
	}
	if !g.Connected("b", "c") {
		t.Error("expected b-c connected")
	}
	if g.Connected("a", "c") {
		t.Error("expected a-c NOT connected")
	}
	if g.Connected("a", "z") {
		t.Error("expected a-z NOT connected (unknown node)")
	}

	// Density: 3 members, 3 possible pairs, 2 edges (a-b, b-c) → 2/3.
	members := []string{"a", "b", "c"}
	want := 2.0 / 3.0
	if d := g.Density(members); d != want {
		t.Errorf("Density(%v) = %v, want %v", members, d, want)
	}

	// Subset: only a-b → density 1.0.
	if d := g.Density([]string{"a", "b"}); d != 1.0 {
		t.Errorf("Density([a,b]) = %v, want 1.0", d)
	}

	// Subset: only a-c → density 0.0.
	if d := g.Density([]string{"a", "c"}); d != 0.0 {
		t.Errorf("Density([a,c]) = %v, want 0.0", d)
	}

	// Degenerate: zero members.
	if d := g.Density(nil); d != 0 {
		t.Errorf("Density(nil) = %v, want 0", d)
	}

	// Degenerate: one member.
	if d := g.Density([]string{"a"}); d != 0 {
		t.Errorf("Density([a]) = %v, want 0", d)
	}
}

// TestNewSimilarityGraph_ConnectedAndDensity verifies the exported constructor:
// symmetric adjacency, self-pair exclusion, idempotency on duplicates, and
// Density math.
func TestNewSimilarityGraph_ConnectedAndDensity(t *testing.T) {
	pairs := [][2]string{
		{"a", "b"},
		{"b", "c"},
		{"a", "b"}, // duplicate — idempotent
		{"x", "x"}, // self-pair — ignored
	}
	g := NewSimilarityGraph(pairs)

	// Symmetric connectivity.
	if !g.Connected("a", "b") {
		t.Error("expected a-b connected")
	}
	if !g.Connected("b", "a") {
		t.Error("expected b-a connected (symmetric)")
	}
	if !g.Connected("b", "c") {
		t.Error("expected b-c connected")
	}
	// Non-edge.
	if g.Connected("a", "c") {
		t.Error("expected a-c NOT connected")
	}
	// Self-pair was ignored.
	if g.Connected("x", "x") {
		t.Error("expected x-x NOT connected (self-pair excluded)")
	}

	// Density over 3 members with 2 edges (a-b, b-c) → 2/3.
	members := []string{"a", "b", "c"}
	const want = 2.0 / 3.0
	const epsilon = 1e-9
	if d := g.Density(members); d-want > epsilon || want-d > epsilon {
		t.Errorf("Density(%v) = %v, want %v", members, d, want)
	}

	// Density over empty/single.
	if d := g.Density(nil); d != 0 {
		t.Errorf("Density(nil) = %v, want 0", d)
	}
	if d := g.Density([]string{"a"}); d != 0 {
		t.Errorf("Density([a]) = %v, want 0", d)
	}
}

// TestNewSimilarityGraph_EmptyPairs verifies a nil/empty pairs slice produces
// an empty graph with all queries returning false/0.
func TestNewSimilarityGraph_EmptyPairs(t *testing.T) {
	g := NewSimilarityGraph(nil)
	if g.Connected("a", "b") {
		t.Error("empty graph must not have edges")
	}
	if d := g.Density([]string{"a", "b"}); d != 0 {
		t.Errorf("empty graph Density = %v, want 0", d)
	}
}
