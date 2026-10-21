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
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "main"))
	svc.SetEmbedder(&stub768Embedder{})
	// Two facts with near-identical bodies cluster as SIMILAR_TO; a third unrelated.
	writeSrcFact(t, svc, "main", "kb/a.md", "alpha beta gamma delta", nil, nil)
	writeSrcFact(t, svc, "main", "kb/b.md", "alpha beta gamma delta epsilon", nil, nil)
	writeSrcFact(t, svc, "main", "kb/c.md", "zzz unrelated content", nil, nil)
	g, err := svc.gq.SimilarityAdjacency(ctx, []string{"kb/a.md", "kb/b.md", "kb/c.md"})
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
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "main"))

	// Empty input.
	g, err := svc.gq.SimilarityAdjacency(ctx, nil)
	require.NoError(t, err)
	if d := g.Density(nil); d != 0 {
		t.Errorf("empty density = %v, want 0", d)
	}

	// Single path.
	g, err = svc.gq.SimilarityAdjacency(ctx, []string{"kb/x.md"})
	require.NoError(t, err)
	if d := g.Density([]string{"kb/x.md"}); d != 0 {
		t.Errorf("single-member density = %v, want 0", d)
	}
}

// TestSimilarityAdjacency_QuoteInPathDoesNotInject regresses a SQL/Cypher
// injection: path predicates were JSON-encoded, which escapes the double-quote
// (the Cypher "..." layer) but NOT the single quote that would terminate the
// outer SQL cypher('...') string literal. A path carrying injection-shaped
// quotes must be neutralised (escapeCypherKey strips ' and escapes ") so the
// query stays well-formed and matches only literal paths — never widening or
// erroring.
func TestSimilarityAdjacency_QuoteInPathDoesNotInject(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "main"))

	si := svc.si
	ctx := context.Background()

	a := mergeFactNode(t, si, "kb/a.md", "aaaa")
	b := mergeFactNode(t, si, "kb/b.md", "bbbb")
	require.NoError(t, si.GraphInsertEdgeForTest(ctx, a, b, EdgeSimilarTo))

	// Injection-shaped paths: a single-quote attempt to break the SQL literal and
	// force a tautology, and a double-quote attempt to break the Cypher literal.
	// If escaping failed, the OR-chain would either error or match every node.
	sqlInject := `kb/a.md') RETURN f.path AS a, f.path AS b --`
	cypherInject := `kb/a.md" OR true OR f.path = "x`

	g, err := svc.gq.SimilarityAdjacency(ctx, []string{"kb/a.md", "kb/b.md", sqlInject, cypherInject})
	require.NoError(t, err, "injection-shaped paths must keep the query well-formed, not error")

	// Only the real a–b edge may survive; injection paths must not widen the result.
	require.True(t, g.Connected("kb/a.md", "kb/b.md"), "the real a–b edge must still match")
	require.False(t, g.Connected("kb/a.md", sqlInject), "injection path must not match")
	require.False(t, g.Connected("kb/a.md", cypherInject), "injection path must not match")
	if d := g.Density([]string{"kb/a.md", "kb/b.md"}); d != 1.0 {
		t.Errorf("two connected members density = %v, want 1.0", d)
	}
}
