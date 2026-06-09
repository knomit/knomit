package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/retrieval"
)

// configurableEmbedder is a BatchEmbedder whose ID() and Dim() are settable,
// for exercising NeedsRebuild's embedding-identity gate. Vectors are
// deterministic and `dim`-wide so they satisfy facts_vec.
type configurableEmbedder struct {
	id  string
	dim int
}

func (e *configurableEmbedder) embed() []float32 {
	out := make([]float32, e.dim)
	for i := range out {
		out[i] = float32(i%5) / 5.0
	}
	return out
}

func (e *configurableEmbedder) EmbedQuery(string) ([]float32, error) { return e.embed(), nil }
func (e *configurableEmbedder) EmbedDocument(string, string) ([]float32, error) {
	return e.embed(), nil
}
func (e *configurableEmbedder) Dim() int   { return e.dim }
func (e *configurableEmbedder) ID() string { return e.id }
func (e *configurableEmbedder) Thresholds() retrieval.Thresholds {
	return retrieval.Defaults()
}
func (e *configurableEmbedder) EmbedDocuments(titles, _ []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.embed()
	}
	return out, nil
}

// TestNeedsRebuildDetectsModelChange verifies that, with the schema version
// current, NeedsRebuild gates on the embedding identity: a matching
// model-id/dim is clean, while a model-id or dim mismatch forces a rebuild.
func TestNeedsRebuildDetectsModelChange(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	si := svc.Search().(*searchIndex)

	// Persist a current schema version + a matching embedding identity.
	emb := &configurableEmbedder{id: "embeddinggemma", dim: 768}
	svc.SetEmbedder(emb)
	_, err = si.rh.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('graph_schema_version', ?)`, GraphSchemaVersion)
	require.NoError(t, err)
	seedEmbedIdentity(t, si, "embeddinggemma", 768)

	stale, err := si.NeedsRebuild(ctx)
	require.NoError(t, err)
	require.False(t, stale, "matching model-id and dim must be clean")

	// Model-id change → stale.
	emb.id = "nomic-v1.5"
	stale, err = si.NeedsRebuild(ctx)
	require.NoError(t, err)
	require.True(t, stale, "model-id change must force a rebuild")

	// Back to matching id, but dim change → stale.
	emb.id = "embeddinggemma"
	emb.dim = 1024
	stale, err = si.NeedsRebuild(ctx)
	require.NoError(t, err)
	require.True(t, stale, "dim change must force a rebuild")
}

// TestRebuild_BumpsGraphSchemaVersion verifies that a successful Rebuild
// writes meta.graph_schema_version to the current expected value, signalling
// that the graph layout has been updated to match this binary.
func TestRebuild_BumpsGraphSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	require.NoError(t, svc.Search().(*searchIndex).Rebuild(context.Background(), "main", nil))

	si := svc.Search().(*searchIndex)
	var version string
	require.NoError(t, si.rh.db.QueryRow(`SELECT value FROM meta WHERE key = 'graph_schema_version'`).Scan(&version))
	require.Equal(t, GraphSchemaVersion, version)
}

// TestRebuildGraph_WritesEdgePerRefEvent verifies that after a full
// rebuild, the total DERIVED_FROM edge count equals the total number of
// ref-events in commit_log (added/modified rows × number of local refs
// per blob). Two D versions both ref'ing E should produce 2 edges.
func TestRebuildGraph_WritesEdgePerRefEvent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// Two versions of D, both ref'ing E.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/e.md"}), "init d", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d v2", 0.85, []string{"kb/e.md"}), "update d", "")
	require.NoError(t, err)

	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	si := svc.Search().(*searchIndex)
	var count int
	require.NoError(t, si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE type = ?`, EdgeDerivedFrom).Scan(&count))
	require.Equal(t, 2, count, "expected one DERIVED_FROM edge per (D version) referencing E")
}

// TestRebuild_RepopulatesDomainAndEntityJunctions regresses the bug where
// rebuildFacts cascades-deleted fact_domains / fact_entities rows via the
// INSERT OR REPLACE INTO facts trigger but never re-populated them, leaving
// search filters (which read the junction tables) returning zero results
// while stats (which read f.domain / f.entities JSON) continued to see the
// data. Both sources of truth must agree after a rebuild.
func TestRebuild_RepopulatesDomainAndEntityJunctions(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// Build two facts with multiple domain & entity tags each.
	mkBody := func(title string, conf float64, domains, entities []string) string {
		f := fact.NewFact("placeholder.md")
		f.Title = title
		f.Confidence = conf
		f.Sources = 1
		f.Domain = domains
		f.Entities = entities
		f.Type = fact.Observation
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		return out
	}

	_, err = svc.Facts().WriteFact(ctx, branch,
		"kb/alpha.md", mkBody("Alpha", 0.9, []string{"ai", "AI agents"}, []string{"Anthropic", "Claude"}),
		"init alpha", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch,
		"kb/beta.md", mkBody("Beta", 0.8, []string{"ai"}, []string{"OpenAI"}),
		"init beta", "")
	require.NoError(t, err)

	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	si := svc.Search().(*searchIndex)

	// fact_domains must contain one row per (fact, domain-value) pair.
	var domainRows int
	require.NoError(t, si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fact_domains`).Scan(&domainRows))
	require.Equal(t, 3, domainRows, "fact_domains must be re-populated after rebuild (alpha:2 + beta:1)")

	// Specifically: 'ai' should match both facts via the search filter path.
	results, err := svc.Search().Search(ctx, branch, SearchOptions{Domain: []string{"ai"}})
	require.NoError(t, err)
	require.Len(t, results, 2, "search by domain=ai must find both facts after rebuild")

	// fact_entities must also be re-populated.
	var entityRows int
	require.NoError(t, si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fact_entities`).Scan(&entityRows))
	require.Equal(t, 3, entityRows, "fact_entities must be re-populated after rebuild (alpha:2 + beta:1)")

	results, err = svc.Search().Search(ctx, branch, SearchOptions{Entities: []string{"Anthropic"}})
	require.NoError(t, err)
	require.Len(t, results, 1, "search by entity=Anthropic must find alpha after rebuild")
}

// TestUpsert_DuplicateEntitiesAndDomains_NoError verifies that when a
// FactRecord contains duplicate entries in Entities and/or Domain,
// the upsert does not fail on a UNIQUE constraint violation, but instead
// deduplicates via INSERT OR IGNORE, leaving one row per unique pair.
func TestUpsert_DuplicateEntitiesAndDomains_NoError(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// Create a FactRecord with duplicate entities and domains.
	rec := FactRecord{
		Path:       "kb/test-dupes.md",
		Title:      "Test with duplicates",
		BlobHash:   "abc123def456",
		Kind:       "epistemic",
		Type:       "observation",
		Domain:     []string{"ai", "machine-learning", "ai"},      // "ai" appears twice
		Entities:   []string{"Claude", "Anthropic", "Claude"},     // "Claude" appears twice
		Confidence: 0.9,
		Sources:    1,
		Refs:       []string{},
	}

	si := svc.Search().(*searchIndex)

	// upsert should succeed without constraint violation.
	err = si.upsert(ctx, branch, "test-commit-hash", rec)
	require.NoError(t, err, "upsert with duplicate entities and domains must not fail")

	// Verify fact was inserted.
	var factID int64
	err = si.rh.db.QueryRowContext(ctx,
		`SELECT id FROM facts WHERE path = ?`, rec.Path).Scan(&factID)
	require.NoError(t, err, "fact must be inserted")
	require.Greater(t, factID, int64(0))

	// Verify fact_entities contains exactly 2 rows (one per unique entity).
	var entityCount int
	err = si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fact_entities WHERE fact_id = ?`, factID).Scan(&entityCount)
	require.NoError(t, err)
	require.Equal(t, 2, entityCount, "fact_entities should have exactly 2 rows for 2 unique entities (Claude, Anthropic)")

	// Verify both entities are present (not just a lucky count).
	var entities []string
	rows, err := si.rh.db.QueryContext(ctx,
		`SELECT entity FROM fact_entities WHERE fact_id = ? ORDER BY entity`, factID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var entity string
		require.NoError(t, rows.Scan(&entity))
		entities = append(entities, entity)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"Anthropic", "Claude"}, entities)

	// Verify fact_domains contains exactly 2 rows (one per unique domain).
	var domainCount int
	err = si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fact_domains WHERE fact_id = ?`, factID).Scan(&domainCount)
	require.NoError(t, err)
	require.Equal(t, 2, domainCount, "fact_domains should have exactly 2 rows for 2 unique domains (ai, machine learning)")

	// Verify both domains are present.
	var domains []string
	rows, err = si.rh.db.QueryContext(ctx,
		`SELECT domain FROM fact_domains WHERE fact_id = ? ORDER BY domain`, factID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var domain string
		require.NoError(t, rows.Scan(&domain))
		domains = append(domains, domain)
	}
	require.NoError(t, rows.Err())
	// Domains are stored canonicalised (de-hyphenized): "machine-learning" → "machine learning".
	require.Equal(t, []string{"ai", "machine learning"}, domains)
}
