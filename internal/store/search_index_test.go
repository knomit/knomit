package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

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
