package mcp

import (
	"context"
	"fmt"
	"testing"

	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/store"

	"github.com/stretchr/testify/require"
)

// fakeSearcher is a one-method stand-in for store.FactQuery. It records the
// options it was handed so a test can assert the SHAPE of the query (whole
// branch, band floor, donated vector), not only what the function does with
// the rows that come back.
type fakeSearcher struct {
	results []store.SearchResult
	gotOpts store.SearchOptions
	calls   int
}

func (f *fakeSearcher) Search(_ context.Context, _ string, q store.SearchOptions) ([]store.SearchResult, error) {
	f.calls++
	f.gotOpts = q
	return f.results, nil
}

// hit builds one search row. cosine is given in the 0-1 form a human reasons
// in; it is stored as cosine*100 because that is what the real store returns
// (search_query.go: Score = c.score * 100.0). Feeding the fake the raw cosine
// instead would make every band comparison pass for the wrong reason.
func hit(path, title string, cosine float64, entities ...string) store.SearchResult {
	return store.SearchResult{
		FactWithBody: store.FactWithBody{
			FactRecord: store.FactRecord{
				Path:     path,
				Title:    title,
				Entities: entities,
			},
		},
		Score: cosine * 100.0,
	}
}

// thresholdSets is every model whose band this stage must behave identically
// under: the nomic-era fallback returned by EmbedderThresholds(nil), and the
// model actually shipped. A test pinned only to params.Defaults() is testing a
// model knomit does not run — which is how the original (SimilarTo, Dedup)
// band looked sane while being unusable in production.
//
// The shipped geometry comes from params.ForModel, NOT from
// embeddings.Lookup: internal/mcp stays free of the cgo parent, and reading the
// real calibrated values means a re-sweep reaches these tests instead of
// leaving a retyped copy to rot.
func thresholdSets(t *testing.T) []struct {
	name string
	th   params.Thresholds
} {
	t.Helper()
	shipped, ok := params.ForModel(params.DefaultModelID)
	require.True(t, ok, "shipped model %q must carry calibrated thresholds", params.DefaultModelID)
	return []struct {
		name string
		th   params.Thresholds
	}{
		{"nomic-fallback", params.Defaults()},
		{params.DefaultModelID + "-shipped", shipped},
	}
}

// inBand returns a cosine strictly inside (ReflectNovelty, Dedup) for the given
// model, derived from the band itself so the case holds under any calibration.
func inBand(th params.Thresholds) float64 { return (th.ReflectNovelty + th.Dedup) / 2 }

const (
	// testDFCeiling is the generic-entity cutoff the cases run against. The
	// production value comes from synthesize.DFCeiling(liveFacts); the exact
	// number does not matter here, only which side of it each entity sits.
	testDFCeiling = 12
	genericDF     = testDFCeiling + 1
	specificDF    = 2
)

func TestFindSameSubjectCandidates(t *testing.T) {
	incoming := fact.Fact{
		Title:    "Ramp's AI index shows enterprise adoption plateauing",
		Body:     "Ramp's spend data puts paid AI adoption flat quarter over quarter.",
		Entities: []string{"Ramp", "AI Index", "MCP"},
	}
	entityDF := map[string]int{
		"Ramp":     specificDF,
		"AI Index": specificDF,
		"MCP":      genericDF, // generic: cannot anchor a refusal on its own
	}

	for _, ts := range thresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			band := inBand(ts.th)

			cases := []struct {
				name     string
				f        fact.Fact
				results  []store.SearchResult
				wantPath string
				wantEnts []string
			}{
				{
					name:    "(a) incoming fact has no entities is never refused",
					f:       fact.Fact{Title: incoming.Title, Body: incoming.Body},
					results: []store.SearchResult{hit("kb/a.md", "A", band, "Ramp")},
				},
				{
					name:    "(b) hit at the auto-merge floor belongs to applyDedupMerge",
					f:       incoming,
					results: []store.SearchResult{hit("kb/a.md", "A", ts.th.Dedup, "Ramp")},
				},
				{
					name:     "(c) hit in band sharing one non-generic entity is a candidate",
					f:        incoming,
					results:  []store.SearchResult{hit("kb/a.md", "A", band, "Ramp")},
					wantPath: "kb/a.md",
					wantEnts: []string{"Ramp"},
				},
				{
					name:    "(d) hit in band sharing no entity has no anchor",
					f:       incoming,
					results: []store.SearchResult{hit("kb/a.md", "A", band, "Coursera")},
				},
				{
					name:    "(e) hit at the band floor is too far to be the same subject",
					f:       incoming,
					results: []store.SearchResult{hit("kb/a.md", "A", ts.th.ReflectNovelty, "Ramp")},
				},
				{
					name:    "(f) hit in band sharing only a generic entity is not refused",
					f:       incoming,
					results: []store.SearchResult{hit("kb/a.md", "A", band, "MCP")},
				},
				{
					name:     "(g) generic and non-generic shared: only the non-generic anchors",
					f:        incoming,
					results:  []store.SearchResult{hit("kb/a.md", "A", band, "MCP", "Ramp")},
					wantPath: "kb/a.md",
					wantEnts: []string{"Ramp"},
				},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					q := &fakeSearcher{results: tc.results}
					got, err := findSameSubjectCandidates(
						context.Background(), q, "agent/test", tc.f, nil, ts.th, entityDF, testDFCeiling, defaultPageSize)
					require.NoError(t, err)

					if tc.wantPath == "" {
						require.Empty(t, got, "must not refuse: no anchor or outside the band")
						return
					}
					require.Len(t, got, 1)
					require.Equal(t, tc.wantPath, got[0].Path)
					require.Equal(t, tc.wantEnts, got[0].SharedEntities)
					require.InDelta(t, band, got[0].Similarity, 1e-9,
						"Similarity must be reported as a cosine, not the store's cosine*100")
				})
			}
		})
	}
}

// The band is the whole POINT of this stage: today's dedup searches only the
// incoming fact's own category directory, which is exactly why one event filed
// under two categories never collides. A Path here would reintroduce that.
func TestFindSameSubjectCandidates_SearchesWholeBranchAtTheBandFloor(t *testing.T) {
	for _, ts := range thresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			q := &fakeSearcher{}
			f := fact.Fact{Title: "T", Body: "B", Entities: []string{"Ramp"}}
			vec := []float32{0.1, 0.2}

			_, err := findSameSubjectCandidates(
				context.Background(), q, "agent/test", f, vec, ts.th,
				map[string]int{"Ramp": specificDF}, testDFCeiling, defaultPageSize)
			require.NoError(t, err)

			require.Equal(t, "", q.gotOpts.Path, "must search the whole branch, not a category dir")
			require.Equal(t, ts.th.ReflectNovelty, q.gotOpts.MinSimilarity)
			require.Equal(t, "T B", q.gotOpts.Text)
			require.Equal(t, vec, q.gotOpts.QueryVec, "must reuse the donated vector, not re-embed")
			require.Equal(t, defaultPageSize, q.gotOpts.Limit)
		})
	}
}

// Both "no anchor available" branches fail OPEN. With embeddings disabled
// EmbedderThresholds(nil) hands back the NOMIC band while no model is running,
// so evaluating candidates at all would be judging against a model that is not
// there. Nothing is refused today only because an empty vector map makes the
// store return no rows before scoring — an accident, not a guard.
func TestFindSameSubjectCandidates_NoEmbedderNeverRefuses(t *testing.T) {
	q := &fakeSearcher{results: []store.SearchResult{
		hit("kb/a.md", "A", inBand(params.Defaults()), "Ramp"),
	}}
	got, err := findSameSubjectCandidates(
		context.Background(), q, "agent/test",
		fact.Fact{Title: "T", Body: "B", Entities: []string{"Ramp"}},
		nil, params.Thresholds{}, map[string]int{"Ramp": specificDF}, testDFCeiling, defaultPageSize)

	require.NoError(t, err)
	require.Empty(t, got, "a zero threshold set means no calibrated band; refuse nothing")
	require.Zero(t, q.calls, "must not even search when there is no band to judge against")
}

// dfCeiling is shared with the motif df band rather than re-derived, so a
// corpus-size rule lives in one place. These pin the two limbs of
// max(floor, percent% of N) at the sizes where each one wins.
func TestDFCeilingSharesTheMotifBandRule(t *testing.T) {
	require.Equal(t, 12, dfCeilingForFacts(0), "floor wins on an empty corpus")
	require.Equal(t, 12, dfCeilingForFacts(200), "2% of 200 is 4; the floor still wins")
	require.Equal(t, 20, dfCeilingForFacts(1000), "2% of 1000 is 20; the ratio wins")
}

func TestSameSubjectCandidateRendersPathTitleEntitiesAndSimilarity(t *testing.T) {
	c := sameSubjectCandidate{
		Path:           "kb/business/companies/ramp/ai-index/abc.md",
		Title:          "Ramp AI Index: adoption flat",
		SharedEntities: []string{"Ramp", "AI Index"},
		Similarity:     0.71,
	}
	require.Equal(t,
		"kb/business/companies/ramp/ai-index/abc.md — Ramp AI Index: adoption flat (shared: Ramp, AI Index; similarity 0.71)",
		fmt.Sprint(c))
}
