package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Phase 1 populated FactRecord.Motifs on the WRITE path and stopped there: no
// read path selected the column, so every reader saw an empty list while the
// data sat correctly on disk and in the junction. That failure mode is silent
// by construction — the field exists, the writes work, the junction tests pass,
// and every surface built on top ships empty with a green suite. This file is
// the fail-closed check that each read path actually carries motifs through.
//
// One test per SCANNER, not one per surface: the scanners are where the column
// list and the Scan() argument list have to agree, and a surface that stops
// using one scanner for another would otherwise silently lose coverage.
//
// Every SearchOptions here sets an explicit non-zero Limit. A bare
// store.SearchOptions{} carries Limit 0, which becomes a literal `LIMIT 0` and
// makes any assertion about returned rows pass vacuously
// (gotchas/store/testing/searchoptions-zero-limit/71123f5f).

// motifEnvEmbedded is motifEnv plus a stub embedder, so the vector search path
// (and therefore its own scanner) is reachable. Without an embedder Search
// falls back to the filter path and the vector scanner is never exercised.
func motifEnvEmbedded(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc, "main"
}

// wantMotifs is the fixture list, in written order. Order is asserted, not
// sorted away: motifs are stored AS WRITTEN (MN3) and the cap trims from the
// end, so a read path that reorders them changes which one survives a later
// merge.
var wantMotifs = []string{"silent-fallback", "config-drift"}

func TestMotifsReadBack_GetByPath(t *testing.T) {
	svc, branch := motifEnv(t)
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", wantMotifs)

	got, err := svc.FactQuery().GetByPath(context.Background(), branch, "kb/alpha/one.md")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, wantMotifs, got.Motifs)
}

func TestMotifsReadBack_GetByPath_MotiflessIsEmptyNotNullString(t *testing.T) {
	svc, branch := motifEnv(t)
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", nil)

	got, err := svc.FactQuery().GetByPath(context.Background(), branch, "kb/alpha/one.md")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got.Motifs,
		"a motif-less fact must read back as no motifs — not as the string \"null\" "+
			"decoded into a one-element list, which is what an unguarded json_each "+
			"over a NULL column produces")
}

func TestMotifsReadBack_SearchFilterPath(t *testing.T) {
	svc, branch := motifEnv(t)
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", wantMotifs)

	// No Text/QueryVec/QueryByPath — the text-less branch of Search, which
	// scans through scanFactWithBodyFromRowsWithCommittedAt.
	res, err := svc.FactQuery().Search(context.Background(), branch, SearchOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, wantMotifs, res[0].Motifs)
}

func TestMotifsReadBack_SearchVectorPath(t *testing.T) {
	svc, branch := motifEnvEmbedded(t)
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", wantMotifs)
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", nil)

	// QueryByPath drives the KNN branch, whose metadata rows are scanned by
	// scanFactRecordFromRowsWithCommittedAt — a different scanner from the
	// filter path above, with its own column list to keep in agreement.
	res, err := svc.FactQuery().Search(context.Background(), branch, SearchOptions{
		QueryByPath: "kb/alpha/two.md",
		Limit:       10,
	})
	require.NoError(t, err)

	byPath := map[string][]string{}
	for _, r := range res {
		byPath[r.Path] = r.Motifs
	}
	require.Contains(t, byPath, "kb/alpha/one.md",
		"the vector path must return the motif-carrying fact, or this test asserts nothing")
	require.Equal(t, wantMotifs, byPath["kb/alpha/one.md"])
}

func TestMotifsReadBack_RecentFacts(t *testing.T) {
	svc, branch := motifEnv(t)
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", wantMotifs)

	entries, total, err := svc.FactQuery().RecentFacts(context.Background(), branch,
		SearchOptions{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, entries, 1)
	require.Equal(t, wantMotifs, entries[0].Motifs)
}

func TestMotifsReadBack_RecentFactsSearch(t *testing.T) {
	svc, branch := motifEnvEmbedded(t)
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", wantMotifs)

	// Text non-empty routes RecentFacts through recentFactsSearch, which runs
	// its own SELECT and its own inline scan rather than reusing either.
	entries, _, err := svc.FactQuery().RecentFacts(context.Background(), branch,
		SearchOptions{Text: "body", Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, entries,
		"the search path must return rows, or this test asserts nothing")

	var found bool
	for _, e := range entries {
		if e.Path == "kb/alpha/one.md" {
			found = true
			require.Equal(t, wantMotifs, e.Motifs)
		}
	}
	require.True(t, found, "fixture fact missing from the search path result")
}
