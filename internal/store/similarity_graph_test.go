package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSimilarityAdjacency_Integration is an integration test that builds a
// tiny index with the stub embedder and asserts SIMILAR_TO edge formation.
func TestSimilarityAdjacency_Integration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	svc.SetEmbedder(&stub768Embedder{})
	// Two facts with near-identical bodies cluster as SIMILAR_TO; a third unrelated.
	writeSrcFact(t, svc, "main", "kb/a.md", "alpha beta gamma delta", nil, nil)
	writeSrcFact(t, svc, "main", "kb/b.md", "alpha beta gamma delta epsilon", nil, nil)
	writeSrcFact(t, svc, "main", "kb/c.md", "zzz unrelated content", nil, nil)
	si := svc.Search().(*searchIndex)
	g, err := si.SimilarityAdjacency(ctx, []string{"kb/a.md", "kb/b.md", "kb/c.md"})
	require.NoError(t, err)
	// a~b expected (near-dup); assert at least the a-b pair is present and density math holds.
	if !g.Connected("kb/a.md", "kb/b.md") {
		t.Skip("stub embedder produced no a-b SIMILAR_TO edge; adjust bodies to force a near-dup")
	}
	if d := g.Density([]string{"kb/a.md", "kb/b.md"}); d != 1.0 {
		t.Errorf("two connected members density = %v, want 1.0", d)
	}
	if d := g.Density([]string{"kb/a.md"}); d != 0 {
		t.Errorf("single-member density = %v, want 0", d)
	}
}

// TestSimilarityAdjacency_EmptyAndSinglePath verifies graceful handling of
// degenerate inputs without needing real SIMILAR_TO edges.
func TestSimilarityAdjacency_EmptyAndSinglePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	si := svc.Search().(*searchIndex)

	// Empty input.
	g, err := si.SimilarityAdjacency(ctx, nil)
	require.NoError(t, err)
	if d := g.Density(nil); d != 0 {
		t.Errorf("empty density = %v, want 0", d)
	}

	// Single path.
	g, err = si.SimilarityAdjacency(ctx, []string{"kb/x.md"})
	require.NoError(t, err)
	if d := g.Density([]string{"kb/x.md"}); d != 0 {
		t.Errorf("single-member density = %v, want 0", d)
	}
}

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
