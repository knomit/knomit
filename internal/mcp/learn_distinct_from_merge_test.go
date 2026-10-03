package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// F-R2 (first mission): a counter-hypothesis worded close to the hypothesis it
// counters was always folded into it by learn's dedup merge, distinct_from or
// not, and one of the two predictions was lost behind a "wrote 1 fact". The
// merge stage now declines a match the call names in distinct_from, and a
// hypothesis-into-hypothesis merge says which prediction it dropped.
// distinct_from stays request-only: never written into the fact.

const dfCategory = "forecast/launch/month"

func dfFact(typ, title, body string, conf float64, distinctFrom []any) map[string]any {
	f := map[string]any{
		"topic": "decisions", "category": dfCategory, "title": title, "body": body,
		"type": typ, "domain": []any{}, "confidence": conf, "sources": 1,
		"entities": []any{"Anthropic", "Form S-1", "EDGAR"}, "refs": []any{},
	}
	if distinctFrom != nil {
		f["distinct_from"] = distinctFrom
	}
	return f
}

// dfSeed writes the original hypothesis (seed marker, cosine 1.0 with itself)
// and returns its path.
func dfSeed(t *testing.T, ctx context.Context, emb store.BatchEmbedder, typ string) string {
	t.Helper()
	r, err := LearnHandler(emb)(ctx, sameSubjectLearnReqMulti("seed", dfFact(typ,
		"Anthropic IPO launch happens in October 2026",
		"The "+seedMarker+" predicts an October launch.", 0.55, nil)))
	require.NoError(t, err)
	require.False(t, r.IsError, "seed: %s", resultText(t, r))
	return mergedFactPath(t, r)
}

func dfRead(t *testing.T, svc *store.Service, path string) string {
	t.Helper()
	rd, err := svc.Facts().ReadFact(context.Background(), "agent/test", path, nil)
	require.NoError(t, err)
	return string(rd.Content)
}

func dfLearn(t *testing.T, ctx context.Context, emb store.BatchEmbedder, f map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	r, err := LearnHandler(emb)(ctx, sameSubjectLearnReqMulti("probe", f))
	require.NoError(t, err)
	return r
}

// (a) A counter-hypothesis above the Dedup floor WITH distinct_from naming the
// original lands at its own new path; the original is untouched; the written
// file carries no distinct_from.
//
// SABOTAGE S5a (no namedIn check in the merge stage) → merged → red.
// SABOTAGE S5d (distinct_from persisted) → the key is in the file → red.
func TestLearnHandler_DistinctFromKeepsCounterHypothesisSeparate(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		for _, conf := range []float64{0.33, 0.7} {
			t.Run(fmt.Sprintf("%s/conf=%g", ts.name, conf), func(t *testing.T) {
				svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, ts.th.Dedup+(1-ts.th.Dedup)*0.5)
				seed := dfSeed(t, ctx, emb, "hypothesis")
				seedBefore := dfRead(t, svc, seed)
				before := liveFactCount(t, svc)

				r := dfLearn(t, ctx, emb, dfFact("hypothesis",
					"Anthropic IPO launch happens in November 2026",
					"A "+probeMarker+" counter: the launch slips to November.", conf, []any{seed}))
				require.False(t, r.IsError, resultText(t, r))
				got := mergedFactPath(t, r)
				require.NotEqual(t, seed, got, "the counter must land at its own path")
				require.Equal(t, before+1, liveFactCount(t, svc))
				require.Equal(t, seedBefore, dfRead(t, svc, seed), "the original hypothesis is untouched")
				counter := dfRead(t, svc, got)
				require.Contains(t, counter, "November 2026")
				require.NotContains(t, counter, "distinct_from", "distinct_from is request-only, never stored")
				require.NotContains(t, resultText(t, r), "merged into")
			})
		}
	}
}

// (b) WITHOUT distinct_from the counter still merges (the merge stage is not
// switched off), and the response says which prediction was kept and which
// DROPPED, in notes and in summary. When the counter is the more confident,
// the ORIGINAL was dropped and the seed path now holds the counter.
//
// SABOTAGE S5c (the generic note instead of kept/dropped) → red.
func TestLearnHandler_HypothesisMergeSaysWhatWasDropped(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		for _, tc := range []struct {
			conf                 float64
			kept, dropped        string
			keptTitle, dropTitle string
		}{
			{0.33, "existing", "incoming", "October", "November"},
			{0.7, "incoming", "existing", "November", "October"},
		} {
			t.Run(fmt.Sprintf("%s/conf=%g", ts.name, tc.conf), func(t *testing.T) {
				svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, ts.th.Dedup+(1-ts.th.Dedup)*0.5)
				seed := dfSeed(t, ctx, emb, "hypothesis")
				before := liveFactCount(t, svc)

				r := dfLearn(t, ctx, emb, dfFact("hypothesis",
					"Anthropic IPO launch happens in November 2026",
					"A "+probeMarker+" counter: the launch slips to November.", tc.conf, nil))
				require.False(t, r.IsError, resultText(t, r))
				require.Equal(t, seed, mergedFactPath(t, r), "without distinct_from it still merges")
				require.Equal(t, before, liveFactCount(t, svc))

				var res struct {
					Notes   []string `json:"notes"`
					Summary string   `json:"summary"`
				}
				require.NoError(t, json.Unmarshal([]byte(resultText(t, r)), &res))
				require.Len(t, res.Notes, 1)
				note := res.Notes[0]
				require.Contains(t, note, "merged into existing hypothesis "+seed)
				require.Contains(t, note, "kept the "+tc.kept+" prediction \"Anthropic IPO launch happens in "+tc.keptTitle)
				require.Contains(t, note, "DROPPED the "+tc.dropped+" prediction \"Anthropic IPO launch happens in "+tc.dropTitle)
				require.Contains(t, note, "distinct_from")
				require.Contains(t, res.Summary, note, "the summary carries the note too")
				require.Contains(t, dfRead(t, svc, seed), "# Anthropic IPO launch happens in "+tc.keptTitle)
			})
		}
	}
}

// (c) The merge stage declines ONLY the named path: a duplicate whose
// distinct_from names some OTHER existing fact still merges, and a
// non-hypothesis duplicate naming the match lands at its own path.
//
// SABOTAGE S5b (skip the merge for any non-empty distinct_from) → the first
// call does not merge → red.
func TestLearnHandler_DistinctFromDeclinesOnlyTheNamedPath(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, ts.th.Dedup+(1-ts.th.Dedup)*0.5)
			seed := dfSeed(t, ctx, emb, "observation")
			other := mergedFactPath(t, dfLearn(t, ctx, emb, sameSubjectFact("gotchas", "unrelated/area",
				"Something else entirely", "Nothing to do with the launch.", []any{"Elsewhere"})))

			r := dfLearn(t, ctx, emb, dfFact("observation",
				"Anthropic IPO launch happens in October 2026",
				"A "+probeMarker+" restatement.", 0.6, []any{other}))
			require.False(t, r.IsError, resultText(t, r))
			require.Equal(t, seed, mergedFactPath(t, r), "naming a different path does not stop the merge")

			before := liveFactCount(t, svc)
			r = dfLearn(t, ctx, emb, dfFact("observation",
				"Anthropic IPO launch happens in October 2026",
				"A "+probeMarker+" deliberate second record.", 0.6, []any{seed}))
			require.False(t, r.IsError, resultText(t, r))
			require.NotEqual(t, seed, mergedFactPath(t, r))
			require.Equal(t, before+1, liveFactCount(t, svc))
		})
	}
}

// (d) An observation naming a hypothesis in distinct_from does not subsume
// (retract) it: the caller said it is a different subject.
func TestLearnHandler_DistinctFromStopsSubsumption(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, ts.th.Dedup+(1-ts.th.Dedup)*0.5)
			seed := dfSeed(t, ctx, emb, "hypothesis")
			r := dfLearn(t, ctx, emb, dfFact("observation",
				"Anthropic IPO launch happened in October 2026",
				"A "+probeMarker+" observation about a different launch.", 0.8, []any{seed}))
			require.False(t, r.IsError, resultText(t, r))
			exists, err := svc.Facts().FactExists(ctx, "agent/test", seed)
			require.NoError(t, err)
			require.True(t, exists, "the named hypothesis must not be subsumed")
			require.NotContains(t, dfRead(t, svc, mergedFactPath(t, r)), seed, "no subsumption lineage ref")
		})
	}
}

// (k) A distinct_from naming a path that does not exist is still refused, and
// nothing is written, even on a fact the merge stage would otherwise fold:
// the validation must not have become reachable only on the unmerged path.
func TestLearnHandler_DistinctFromMissingPathStillRefusedOnAMerge(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, ts.th.Dedup+(1-ts.th.Dedup)*0.5)
			seed := dfSeed(t, ctx, emb, "observation")
			seedBefore := dfRead(t, svc, seed)
			before := liveFactCount(t, svc)
			r := dfLearn(t, ctx, emb, dfFact("observation",
				"Anthropic IPO launch happens in October 2026",
				"A "+probeMarker+" restatement.", 0.9,
				[]any{"kb/decisions/" + dfCategory + "/nope.md"}))
			require.True(t, r.IsError)
			require.True(t, strings.Contains(resultText(t, r), "does not exist on this branch"), resultText(t, r))
			require.Equal(t, before, liveFactCount(t, svc))
			require.Equal(t, seedBefore, dfRead(t, svc, seed))
		})
	}
}
