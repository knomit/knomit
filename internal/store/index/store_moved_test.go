package index

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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

// TestFormatMethodologySection_NonEmpty verifies the prompt-section
// formatter renders only ranked bullet lines (title + path + score),
// with no leading heading and with bodies omitted. Each call site
// supplies its own heading wording.
func TestFormatMethodologySection_NonEmpty(t *testing.T) {
	matches := []MethodologyMatch{
		{Path: "kb/meta/reasoning/m1.md", Title: "M1 title", Body: "M1 body about evidence weighting.", Score: 0.87},
		{Path: "kb/meta/reasoning/m2.md", Title: "M2 title", Body: "M2 body about pitfall detection.", Score: 0.62},
	}
	got := FormatMethodologySection(matches)
	require.NotContains(t, got, "Applicable methodology",
		"formatter must not emit a heading; callers own framing")
	require.NotContains(t, got, "Existing methodology")
	require.Contains(t, got, "M1 title")
	require.Contains(t, got, "kb/meta/reasoning/m1.md")
	require.Contains(t, got, "score=0.87")
	require.Contains(t, got, "score=0.62")
	require.NotContains(t, got, "M1 body about evidence weighting.")
	require.Equal(t, 2, strings.Count(got, "•"))
}

// TestFormatMethodologySection_Empty returns empty string for empty input
// so callers can omit the entire section.
func TestFormatMethodologySection_Empty(t *testing.T) {
	require.Equal(t, "", FormatMethodologySection(nil))
	require.Equal(t, "", FormatMethodologySection([]MethodologyMatch{}))
}
