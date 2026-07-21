package testenv

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// Compile-time assertion that DeterministicEmbedder satisfies the production
// store.BatchEmbedder interface. If this fails to compile, the stub is out
// of sync with the real interface.
var _ store.BatchEmbedder = (*DeterministicEmbedder)(nil)

// TestDeterministicEmbedder_SameInputSameOutput asserts that the stub embedder
// is deterministic and produces 768-dim vectors compatible with the facts_vec
// vec0 schema. EmbedDocument and EmbedDocuments must agree for the same inputs.
func TestDeterministicEmbedder_SameInputSameOutput(t *testing.T) {
	t.Log("Scenario: embed the same docs twice via EmbedDocument and EmbedDocuments, vectors must be identical and 768-dim")
	e := &DeterministicEmbedder{}
	require.Equal(t, 768, e.Dim())

	v1, err := e.EmbedDocument(context.Background(), "hello", "body")
	require.NoError(t, err)
	require.Len(t, v1, 768)

	v2, err := e.EmbedDocument(context.Background(), "hello", "body")
	require.NoError(t, err)
	require.Equal(t, v1, v2, "same doc must produce identical vectors across calls")

	batch1, err := e.EmbedDocuments(context.Background(), []string{"hello", "world"}, []string{"body", "other"})
	require.NoError(t, err)
	require.Len(t, batch1, 2)
	require.Len(t, batch1[0], 768)
	require.Equal(t, v1, batch1[0], "EmbedDocument and EmbedDocuments[0] must agree")

	batch2, err := e.EmbedDocuments(context.Background(), []string{"hello", "world"}, []string{"body", "other"})
	require.NoError(t, err)
	require.Equal(t, batch1, batch2, "EmbedDocuments must be deterministic")
}

// TestDeterministicEmbedder_DifferentInputsDifferentVectors asserts that
// different texts produce different vectors (otherwise the stub would be
// useless for search ranking tests).
func TestDeterministicEmbedder_DifferentInputsDifferentVectors(t *testing.T) {
	t.Log("Scenario: different texts produce different vectors")
	e := &DeterministicEmbedder{}
	a, _ := e.EmbedQuery(context.Background(), "alpha")
	b, _ := e.EmbedQuery(context.Background(), "beta")
	require.NotEqual(t, a, b, "distinct inputs must map to distinct vectors")
}
