package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
)

// The MCP twin of the REST lens-motif regression. knomit_query is the
// most-used motif path there is — recall runs through it — and it fans out over
// the same mounts the REST reads do, so it needs the same widening or a
// lens-bound motif filter answers from a smaller read set than the lens has.
//
// The divergence here needs no judge merge, which is what makes it a fair test
// of the ordinary case rather than of an exotic one. A cluster key is the
// SORTED stemmed tokens of a spelling, so `config-drift` and `drift-config` are
// mechanically ONE cluster in every repo — but the representative spelling is
// ELECTED per branch from what that branch carries. Mount A carries only
// `config-drift` and elects it; mount B carries only `drift-config` and elects
// that. The store's exact tier is per-branch canonical equality, so a query for
// `config-drift` resolves on B to itself, matches no spelling whose canonical
// equals it, and B contributes nothing at all.

// seedMotifFact writes one fact carrying exactly the given motifs into the repo
// bound to ctx, and returns nothing — these tests assert on titles.
func seedMotifFact(t *testing.T, ctx context.Context, moment, title string, motifs []any) {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": moment,
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   "mission/store",
				"title":      title,
				"body":       "designer authored " + title + " with a motif.",
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{"store"},
				"confidence": 0.8,
				"sources":    1,
				"entities":   []any{"designer"},
				"motifs":     motifs,
				"refs":       []any{},
			},
		},
	}
	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Falsef(t, result.IsError, "seed failed: %s", resultText(t, result))
}

// divergentMotifLens builds the two mounts described above and returns a lens
// binding over both (write = A).
func divergentMotifLens(t *testing.T) *repos.Binding {
	t.Helper()
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)
	seedMotifFact(t, ctxA, "seed-a", "AlphaCarrier", []any{"config-drift"})
	seedMotifFact(t, ctxB, "seed-b", "BravoCarrier", []any{"drift-config"})
	return repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
}

func queryTitles(t *testing.T, text string) map[string]bool {
	t.Helper()
	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	titles := map[string]bool{}
	for _, f := range resp.Facts {
		titles[f.Title] = true
	}
	return titles
}

// THE REGRESSION, recency path. Both mounts carry the shape; both must answer.
func TestQueryFederation_MotifTermReachesEveryMount_Recent(t *testing.T) {
	b := divergentMotifLens(t)
	result, text := queryVia(t, b, map[string]any{
		"motifs": []any{"config-drift"}, "motif_match": "exact", "sort": "recent",
	})
	require.Falsef(t, result.IsError, "query failed: %s", text)

	titles := queryTitles(t, text)
	require.True(t, titles["AlphaCarrier"], "the electing mount's carrier is missing: %s", text)
	require.True(t, titles["BravoCarrier"],
		"the mount that spells the shape differently contributed nothing — the lens answered from a smaller read set than it has: %s", text)
}

// The relevance path fans out separately (queryFirstCall, RRF-fused), so it
// needs its own assertion — the same defect with a different envelope.
func TestQueryFederation_MotifTermReachesEveryMount_Relevance(t *testing.T) {
	b := divergentMotifLens(t)
	result, text := queryVia(t, b, map[string]any{
		"motifs": []any{"config-drift"}, "motif_match": "exact",
	})
	require.Falsef(t, result.IsError, "query failed: %s", text)

	titles := queryTitles(t, text)
	require.True(t, titles["AlphaCarrier"], "the electing mount's carrier is missing: %s", text)
	require.True(t, titles["BravoCarrier"],
		"the mount that spells the shape differently contributed nothing: %s", text)
}

// Naming the OTHER mount's spelling is the same query — the term names a shape,
// and which spelling the caller happened to hold does not change which facts
// carry it.
func TestQueryFederation_EitherSpellingNamesTheSameShape(t *testing.T) {
	b := divergentMotifLens(t)
	_, text := queryVia(t, b, map[string]any{
		"motifs": []any{"drift-config"}, "motif_match": "exact", "sort": "recent",
	})
	titles := queryTitles(t, text)
	require.True(t, titles["AlphaCarrier"], "asking by the read mount's spelling lost the write mount: %s", text)
	require.True(t, titles["BravoCarrier"], "asking by its own spelling lost the read mount: %s", text)
}

// A term in no cluster is passed through unchanged — its own singleton, which
// is what the store does with it too. It must not become an empty filter that
// quietly matches everything.
func TestQueryFederation_UnknownMotifTermMatchesNothing(t *testing.T) {
	b := divergentMotifLens(t)
	_, text := queryVia(t, b, map[string]any{
		"motifs": []any{"nothing-like-this"}, "motif_match": "exact", "sort": "recent",
	})
	titles := queryTitles(t, text)
	require.Empty(t, titles, "an unknown motif must select nothing, not everything: %s", text)
}

// A binding over ONE mount is skipped by the widening, and the answer is the
// repo's own. This is NOT a free assertion: widening a single mount would not
// be a no-op — ClustersUnder groups `config-drift` and `drift-config` into one
// cluster (same sorted tokens) while the store's exact tier resolves a bare
// term through per-spelling canonicals, so a widened term would reach a sibling
// the repo read directly does not. A lens over one repo must answer exactly
// like that repo, which is what this pins.
func TestQueryFederation_MotifWideningIsSkippedForOneMount(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	seedMotifFact(t, ctxA, "seed-a", "AlphaCarrier", []any{"config-drift"})
	seedMotifFact(t, ctxA, "seed-a2", "AlphaOther", []any{"drift-config"})
	b := repos.NewBindingForTest(repoA, repos.ReadTarget{RI: repoA, Branch: "agent/test"})

	_, text := queryVia(t, b, map[string]any{
		"motifs": []any{"config-drift"}, "motif_match": "exact", "sort": "recent",
	})
	titles := queryTitles(t, text)
	require.True(t, titles["AlphaCarrier"], "the repo's own resolution was disturbed: %s", text)
	require.False(t, titles["AlphaOther"],
		"a single mount answered for a cluster sibling — the widening ran where a lens must answer exactly like the repo it wraps: %s", text)
}

// And the other side of that boundary, stated so it cannot be read as an
// accident: over TWO mounts the union IS the vocabulary, so a term reaches
// every spelling the union calls one shape — looser, at the exact tier, than a
// single unrebuilt repo would be. That is the federation semantics, not a leak.
func TestQueryFederation_TwoMountsResolveThroughTheUnionsVocabulary(t *testing.T) {
	b := divergentMotifLens(t)
	_, text := queryVia(t, b, map[string]any{
		"motifs": []any{"config-drift"}, "motif_match": "exact", "sort": "recent",
	})
	titles := queryTitles(t, text)
	require.Len(t, titles, 2, "both spellings are one shape once a union exists: %s", text)
}

// §9.1 ON THE WIDENING PATH: a mount that cannot be read while a motif term is
// being resolved fails the WHOLE query.
//
// This is the rule everywhere else in the fan-out, but the widening is where it
// bites hardest and where it was going unpinned: a lens must never answer a
// motif query from a smaller read set than it has, and an unreadable mount is
// precisely a mount whose contribution is unknown. Answering anyway would hand
// back a plausible, smaller result with nothing on it to say so — the failure
// mode the whole no-federation-metadata decision is built around.
//
// The mount is broken by pinning it to a branch that does not exist, which is
// the realistic version of this (a lens outliving a deleted branch), and the
// message has to NAME it — that is what federate.MotifReadError carries.
//
// WHICH ASSERTION IS LOAD-BEARING, because it is none of the obvious ones. A
// broken mount is read TWICE on this path — once to resolve the term, once by
// the fan-out — so under a mutant where the widening never touches it the query
// still errors, from RecentFacts, and IsError stays true. Nor does asserting
// the branch discriminate: the fan-out's own message is
// `RecentFacts: branch "no/such/branch": branch not found`, which carries it
// verbatim. And the repo NAME is worthless here — these fixtures name every
// repo "test", four characters that also sit inside the healthy mount's
// agent/test.
//
// Only federate.MotifReadError's own prefix can distinguish them, because only
// the widening produces it. That is what is asserted, and the write-mount-only
// mutant dies on exactly that line.
func TestQueryFederation_MotifWideningFailureFailsTheWholeQuery(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, _ := fedRepo(t)
	seedMotifFact(t, ctxA, "seed-a", "AlphaCarrier", []any{"config-drift"})

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "no/such/branch"},
	)

	for _, sort := range []string{"recent", "relevance"} {
		t.Run(sort, func(t *testing.T) {
			result, text := queryVia(t, b, map[string]any{
				"motifs": []any{"config-drift"}, "motif_match": "exact", "sort": sort,
			})
			require.Truef(t, result.IsError,
				"an unreadable mount must fail the query, not shrink the read set silently: %s", text)
			// The widening's own error, which nothing else on this path can
			// produce — this is the line that says the term was resolved
			// against the whole read set rather than part of it.
			require.Containsf(t, text, "motif vocabulary read failed on mount",
				"the failure must come from the widening, not merely from the later fan-out: %s", text)
			// ...and it must be diagnosable: which mount, at which branch.
			require.Containsf(t, text, repoB.Name()+"@no/such/branch",
				"the failure must name the mount and its branch: %s", text)
		})
	}
}
