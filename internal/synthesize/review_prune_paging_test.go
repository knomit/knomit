package synthesize

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// Prune paging.
//
// Prune was the last unbounded payload path. Its clusters are deliberately NOT
// chunked — splitting one across work items would silently forbid merges across
// the boundary, so a duplicate pair that straddled the split would simply never
// be found — which left cluster size bounded only by what Louvain happens to
// produce. That held for real corpora (2–13 facts observed) but is a property
// of the data, not a guarantee, and unlike distill there was no chunkFacts
// backstop underneath it.
//
// Paging is the resolution rather than a workaround: it splits the DELIVERY and
// keeps the DECISION whole, which is exactly what chunking could not do. Prune
// therefore has more to gain from paging than distill did.

func bigPruneFacts(n, bodyBytes int) []factForLLM {
	facts := make([]factForLLM, 0, n)
	for i := 0; i < n; i++ {
		facts = append(facts, factForLLM{
			File:       fmt.Sprintf("kb/architecture/prune/fact-%03d.md", i),
			Title:      fmt.Sprintf("prune fact %03d", i),
			Body:       strings.Repeat("y", bodyBytes),
			Type:       "observation",
			Domain:     []string{"architecture"},
			Confidence: 0.7,
			Sources:    1,
		})
	}
	return facts
}

// TestRenderPruneWorkItem_FactsAreStructural is the prerequisite: a payload
// serialized into the prompt STRING cannot be sliced, so prune could not page
// until its facts moved beside the prompt the way distill's did.
func TestRenderPruneWorkItem_FactsAreStructural(t *testing.T) {
	facts := []factForLLM{{
		File:       "kb/architecture/prune/only.md",
		Title:      "the only fact",
		Body:       "PRUNE-SENTINEL-must-not-appear-inside-the-prompt",
		Type:       "observation",
		Domain:     []string{"architecture"},
		Confidence: 0.9,
		Sources:    1,
	}}

	content, err := RenderPruneWorkItem(facts, "kb")
	require.NoError(t, err)

	require.NotContains(t, content.Prompt, "PRUNE-SENTINEL-must-not-appear-inside-the-prompt",
		"prune fact bodies must not be serialized into the prompt string")
	require.NotEmpty(t, content.Facts, "prune must carry its facts as a structured field")

	var round []factForLLM
	require.NoError(t, json.Unmarshal([]byte(content.Facts), &round))
	require.Len(t, round, 1)
	require.Equal(t, "PRUNE-SENTINEL-must-not-appear-inside-the-prompt", round[0].Body,
		"splitting facts out of the prompt must not drop or alter them")
}

// TestPaging_LargePruneClusterPagesAndRequiresToken exercises the whole path on
// a cluster too big for one tool result: it must page, and answering it early
// must be refused.
//
// Driven through reviewResultPage and RequireCompletion directly rather than a
// live session, because a unit store has no similarity edges and therefore
// produces no prune clusters at all (Louvain returns singletons and
// filterSmallClusters removes them) — the cluster shape under test cannot be
// reached from StartSession here.
func TestPaging_LargePruneClusterPagesAndRequiresToken(t *testing.T) {
	facts := bigPruneFacts(40, 3*1024)
	content, err := RenderPruneWorkItem(facts, "kb")
	require.NoError(t, err)

	res := &PipelineResult{
		SessionID: "sess",
		Item: &PipelineItem{
			ID: 7, Type: "prune",
			Prompt: content.Prompt, ResponseSchema: content.ResponseSchema,
			Facts: content.Facts, FactsJSON: content.Facts,
		},
	}

	first, err := reviewResultPage(res, 1)
	require.NoError(t, err)
	require.Greater(t, first.Item.Pages, 1, "a 40-fact prune cluster must span several pages")
	require.True(t, first.Item.MoreAvailable)
	require.Empty(t, first.Item.CompletionToken)

	// Pages must partition the cluster: a prune decision is made per path, so a
	// dropped page means a fact silently never judged.
	var seen []string
	for p := 1; p <= first.Item.Pages; p++ {
		page, perr := reviewResultPage(res, p)
		require.NoError(t, perr)
		var pf []factForLLM
		require.NoError(t, json.Unmarshal(page.Item.Facts, &pf))
		for _, f := range pf {
			seen = append(seen, f.File)
		}
	}
	require.Len(t, seen, len(facts), "pages must cover every fact in the cluster exactly once")

	item := &store.PipelineWorkItem{ID: 7, StepType: "prune", FactsJSON: content.Facts}

	require.Error(t, reviewStrategy{}.RequireCompletion(item, ""),
		"a multi-page prune item answered without a token must be rejected")

	last, err := reviewResultPage(res, first.Item.Pages)
	require.NoError(t, err)
	require.NotEmpty(t, last.Item.CompletionToken)
	require.NoError(t, reviewStrategy{}.RequireCompletion(item, last.Item.CompletionToken),
		"the token from the final page must be accepted")
}

// TestPaging_TokenRequiredOnlyWhereItIsIssued is the wedge guard, and the reason
// the paged-step list is explicit rather than inferred.
//
// RequireCompletion computes page count from item.FactsJSON, while the wire
// pages item.Facts — what Render chose to ship beside the prompt. For reflect
// and discover those disagree: their payloads are interpolated into the prompt,
// so reviewResultPage always returns a single page and never issues a token —
// but their stored FactsJSON is still JSON that a fact-shaped unmarshal will
// happily accept (hypothesisTransition even has a "path" field, which maps onto
// factForLLM.File). Requiring a token from them would demand something the
// agent was never given, and the item could never be answered at all.
func TestPaging_TokenRequiredOnlyWhereItIsIssued(t *testing.T) {
	// Oversized payloads in each non-paged step's own shape.
	transitions := make([]hypothesisTransition, 0, 400)
	for i := 0; i < 400; i++ {
		transitions = append(transitions, hypothesisTransition{
			Path:         fmt.Sprintf("kb/architecture/hyp/%03d.md", i),
			OriginalType: "hypothesis",
			Action:       "promoted",
			Detail:       strings.Repeat("d", 200),
		})
	}
	reflectJSON, err := json.Marshal(transitions)
	require.NoError(t, err)
	require.Greater(t, len(reflectJSON), maxPageFactBytes,
		"precondition: the reflect payload must be big enough to page if anything paged it")

	discoverJSON, err := json.Marshal(DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge:    BridgeSeedSet{Token: "x", Kind: BridgeEntity, Members: bigPruneFacts(40, 3*1024)},
	})
	require.NoError(t, err)
	require.Greater(t, len(discoverJSON), maxPageFactBytes)

	for _, tc := range []struct {
		step    string
		payload string
	}{
		{"reflect", string(reflectJSON)},
		{"discover", string(discoverJSON)},
	} {
		item := &store.PipelineWorkItem{ID: 11, StepType: tc.step, FactsJSON: tc.payload}
		require.NoErrorf(t, reviewStrategy{}.RequireCompletion(item, ""),
			"%s does not page, so it must never demand a token the agent was never issued", tc.step)
	}
}
