package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// newTokenDFFixture builds a fresh service+branch for TokenDF tests.
func newTokenDFFixture(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc, "main"
}

// testFactBodyWithDomainAndEntities builds a fact body that carries explicit
// domain and entity tags, using fact.SerializeFact so the format matches the parser.
func testFactBodyWithDomainAndEntities(title string, domains, entities []string) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Confidence = 0.7
	f.Sources = 1
	f.Domain = domains
	f.Entities = entities
	f.Type = fact.Observation
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// TestTokenDF_DomainDF verifies that TokenDF counts live facts tagged with a
// specific domain (canonical form stored at index time).
func TestTokenDF_DomainDF(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTokenDFFixture(t)

	_, err := svc.Facts().WriteFact(ctx, branch, "kb/a.md",
		testFactBodyWithDomainAndEntities("fact a", []string{"store"}, nil), "init a", "")
	require.NoError(t, err)

	_, err = svc.Facts().WriteFact(ctx, branch, "kb/b.md",
		testFactBodyWithDomainAndEntities("fact b", []string{"store"}, nil), "init b", "")
	require.NoError(t, err)

	_, err = svc.Facts().WriteFact(ctx, branch, "kb/c.md",
		testFactBodyWithDomainAndEntities("fact c", []string{"auth"}, nil), "init c", "")
	require.NoError(t, err)

	n, err := svc.Search().TokenDF(ctx, branch, "store", "domain")
	require.NoError(t, err)
	require.Equal(t, 2, n, "two live facts tagged domain=store")

	n, err = svc.Search().TokenDF(ctx, branch, "auth", "domain")
	require.NoError(t, err)
	require.Equal(t, 1, n, "one live fact tagged domain=auth")

	n, err = svc.Search().TokenDF(ctx, branch, "embeddings", "domain")
	require.NoError(t, err)
	require.Equal(t, 0, n, "zero live facts tagged domain=embeddings")
}

// TestTokenDF_DomainCanonical verifies that TokenDF matches the canonical form
// stored at write time. The indexer canonicalizes "AI-Governance" → "ai governance"
// so the query must use the canonical form.
func TestTokenDF_DomainCanonical(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTokenDFFixture(t)

	// Write a fact with domain "AI-Governance"; the indexer will canonicalize it
	// to "ai governance" (casefold + de-hyphenize). We query with the canonical form.
	_, err := svc.Facts().WriteFact(ctx, branch, "kb/gov.md",
		testFactBodyWithDomainAndEntities("governance fact", []string{"AI-Governance"}, nil), "init gov", "")
	require.NoError(t, err)

	// Canonical form stored: "ai governance"
	n, err := svc.Search().TokenDF(ctx, branch, "ai governance", "domain")
	require.NoError(t, err)
	require.Equal(t, 1, n, "TokenDF must match the canonical form stored by the indexer")
}

// TestTokenDF_EntityNOCASE verifies that entity lookup is case-insensitive
// (fact_entities uses COLLATE NOCASE; authored form is stored as-is).
func TestTokenDF_EntityNOCASE(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTokenDFFixture(t)

	_, err := svc.Facts().WriteFact(ctx, branch, "kb/e.md",
		testFactBodyWithDomainAndEntities("anthropic fact", nil, []string{"Anthropic"}), "init e", "")
	require.NoError(t, err)

	n, err := svc.Search().TokenDF(ctx, branch, "anthropic", "entity")
	require.NoError(t, err)
	require.Equal(t, 1, n, "entity lookup must be case-insensitive (COLLATE NOCASE)")

	n, err = svc.Search().TokenDF(ctx, branch, "ANTHROPIC", "entity")
	require.NoError(t, err)
	require.Equal(t, 1, n, "uppercase entity must also match via NOCASE")
}

// TestTokenDF_Liveness verifies that retracted facts are not counted.
func TestTokenDF_Liveness(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTokenDFFixture(t)

	_, err := svc.Facts().WriteFact(ctx, branch, "kb/s1.md",
		testFactBodyWithDomainAndEntities("store fact 1", []string{"store"}, nil), "init s1", "")
	require.NoError(t, err)

	_, err = svc.Facts().WriteFact(ctx, branch, "kb/s2.md",
		testFactBodyWithDomainAndEntities("store fact 2", []string{"store"}, nil), "init s2", "")
	require.NoError(t, err)

	// Sanity: both live → count 2.
	n, err := svc.Search().TokenDF(ctx, branch, "store", "domain")
	require.NoError(t, err)
	require.Equal(t, 2, n, "both live: count must be 2")

	// Retract one.
	_, err = svc.Facts().DeleteFact(ctx, branch, "kb/s1.md", "retract s1")
	require.NoError(t, err)

	// After retraction: count drops to 1.
	n, err = svc.Search().TokenDF(ctx, branch, "store", "domain")
	require.NoError(t, err)
	require.Equal(t, 1, n, "after retraction: count must drop to 1")
}

// TestTokenDF_InvalidKind verifies that an unrecognized kind returns a non-nil error.
func TestTokenDF_InvalidKind(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTokenDFFixture(t)

	_, err := svc.Search().TokenDF(ctx, branch, "x", "bogus")
	require.Error(t, err, "invalid kind must return an error")
	require.Contains(t, err.Error(), "invalid kind")
}
