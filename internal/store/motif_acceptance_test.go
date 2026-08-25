package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// Phase-2 acceptance: the pack-2 fixture replayed through the SHIPPED alias
// builder and match tiers.
//
// Modelled on the Phase-0 acceptance shape (env-gated, real fixtures, weak
// assertions and rich logging) because that shape caught a bug no unit test
// saw. The logging is the point: this test's job is to produce the number the
// GATE decision is made on, not to pass.
//
// What it measures: CROSS-AUTHOR RECALL. Pack 2 had four models independently
// name the same sixteen mechanisms. Two models naming one mechanism
// differently is exactly the aliasing problem in the wild — not a synthetic
// synonym pair, but two competent authors who chose different words.
//
// What it does NOT measure: the LLM judge. This is the mechanical layer plus
// the match tiers — "the shipped alias builder + prefilter". The judge's
// contribution is additive and cannot run offline, so the number here is a
// FLOOR on what the full pipeline achieves, and should be read as one.
//
// Env-gated: KNOMIT_MOTIF_ACCEPTANCE=1.

type packGroup struct {
	group  string
	byAuth map[string][]string // author -> motif names
}

func loadPack2(t *testing.T) []packGroup {
	t.Helper()
	path := filepath.Join("..", "..", ".claude", "plans", "motif", "memora-harness",
		"motif_eval2_outputs.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "pack-2 fixture must be readable")

	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &doc))

	byGroup := map[string]map[string][]string{}
	for author, blob := range doc {
		if author == "_meta" {
			continue
		}
		var groups map[string][]string
		require.NoError(t, json.Unmarshal(blob, &groups))
		for g, motifs := range groups {
			if byGroup[g] == nil {
				byGroup[g] = map[string][]string{}
			}
			byGroup[g][author] = motifs
		}
	}

	var out []packGroup
	for g, byAuth := range byGroup {
		out = append(out, packGroup{group: g, byAuth: byAuth})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].group < out[j].group })
	return out
}

func TestAcceptance_Pack2CrossAuthorRecall(t *testing.T) {
	if os.Getenv("KNOMIT_MOTIF_ACCEPTANCE") == "" {
		t.Skip("set KNOMIT_MOTIF_ACCEPTANCE=1 to run the pack-2 replay")
	}
	ctx := context.Background()
	groups := loadPack2(t)
	require.NotEmpty(t, groups)

	svc, branch := motifEnv(t)

	// Plant every author's every motif as a carrier fact. Paths and titles are
	// deliberately neutral: a path containing the motif's words would trip the
	// subject strip and the motif would never be stored (a trap that cost two
	// fixtures earlier in this phase).
	// Some pack-2 motifs do not satisfy the SHIPPED field contract — the pack
	// was generated before the 2–4 word shape was fixed. That is itself an
	// acceptance datum rather than a fixture problem: it measures how much of
	// what real models produce the write path would refuse, so those motifs are
	// counted and dropped rather than silently reshaped.
	//
	// Reshaping them would be the worse choice twice over: it would inflate the
	// recall number with names no author actually wrote, and it would hide the
	// refusal rate, which is the more actionable of the two figures.
	n, refused := 0, 0
	kept := map[string]map[string][]string{}
	for _, g := range groups {
		kept[g.group] = map[string][]string{}
		for author, motifs := range g.byAuth {
			for _, m := range motifs {
				if err := ValidateMotifForAcceptance(m); err != nil {
					refused++
					t.Logf("refused by the shipped validator: %-40s (%v)", m, err)
					continue
				}
				n++
				writeMotifFact(t, svc, branch, fmt.Sprintf("kb/p%04d.md", n), []string{m})
				kept[g.group][author] = append(kept[g.group][author], m)
			}
		}
	}
	for i := range groups {
		groups[i].byAuth = kept[groups[i].group]
	}
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	t.Logf("pack-2: %d motifs planted, %d REFUSED by the shipped validator (%.0f%% of %d)",
		n, refused, 100*float64(refused)/float64(n+refused), n+refused)

	clusters, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	t.Logf("planted %d motif instances; mechanical layer resolved them to %d clusters", n, len(clusters))

	// Recall per tier: across the tiers a reader can actually reach.
	// Every tier, reported. exact and stem coming out IDENTICAL is a design
	// property rather than a coincidence: the alias layer's grouping key IS the
	// stemmed token multiset, so the two tiers can differ only where a judge
	// merged clusters the mechanical layer kept apart — and no judge runs here.
	for _, tier := range []MotifMatchTier{
		MotifMatchExact, MotifMatchStem, MotifMatchToken2, MotifMatchToken1,
	} {
		rate, hits, pairs := crossAuthorRecall(t, svc, branch, groups, tier)
		t.Logf("tier %-8s cross-author recall %.0f%% (%d/%d author pairs)",
			tier, 100*rate, hits, pairs)
	}

	// THE ACCEPTANCE CRITERION (roadmap Phase 2): >= 60% cross-author recall at
	// the permissive operating point.
	//
	// Measured at 72% on the mechanical layer ALONE — no LLM judge, which
	// cannot run offline. The judge only ever merges clusters the mechanical
	// layer kept apart, so its contribution is additive and this number is a
	// FLOOR on the shipped pipeline, not an estimate of it.
	//
	// Asserted at the blueprint's threshold rather than at the measured value:
	// pinning 72% would turn an acceptance criterion into a change-detector
	// that fails on any harmless drift, and the question the GATE asks is
	// whether the target is met, not whether the number moved.
	rate, hits, pairs := crossAuthorRecall(t, svc, branch, groups, MotifMatchToken1)
	t.Logf("ACCEPTANCE: cross-author recall at the permissive tier = %.0f%% (%d/%d), target >= 60%%",
		100*rate, hits, pairs)
	require.GreaterOrEqual(t, rate, 0.60,
		"pack-2 cross-author recall at the permissive operating point is below the "+
			"blueprint's 60%% acceptance target")
}

// crossAuthorRecall is the fraction of cross-author pairs, within a group, whose
// independently-chosen motif names the pipeline connects at the given tier.
//
// Pairs where either author named nothing are skipped rather than counted as
// misses: a model that declined to name a mechanism has not failed to MATCH
// one, and counting it would measure write-side willingness under a
// read-side heading.
func crossAuthorRecall(t *testing.T, svc *Service, branch string, groups []packGroup, tier MotifMatchTier) (rate float64, hits, pairs int) {
	t.Helper()
	for _, g := range groups {
		authors := make([]string, 0, len(g.byAuth))
		for a := range g.byAuth {
			authors = append(authors, a)
		}
		sort.Strings(authors)
		for i := range authors {
			for j := i + 1; j < len(authors); j++ {
				a, b := g.byAuth[authors[i]], g.byAuth[authors[j]]
				if len(a) == 0 || len(b) == 0 {
					continue
				}
				pairs++
				if anyMatch(t, svc, branch, a, b, tier) {
					hits++
				}
			}
		}
	}
	if pairs > 0 {
		rate = float64(hits) / float64(pairs)
	}
	return rate, hits, pairs
}

// anyMatch reports whether any of a's motifs matches any of b's at the tier.
func anyMatch(t *testing.T, svc *Service, branch string, a, b []string, tier MotifMatchTier) bool {
	t.Helper()
	want := map[string]struct{}{}
	for _, m := range b {
		want[m] = struct{}{}
	}
	for _, m := range a {
		res, err := svc.FactQuery().Search(context.Background(), branch, SearchOptions{
			Motifs:     []string{m},
			MotifMatch: tier,
			Limit:      500,
		})
		require.NoError(t, err)
		for _, r := range res {
			for _, got := range r.Motifs {
				if _, ok := want[got]; ok {
					return true
				}
			}
		}
	}
	return false
}

// ValidateMotifForAcceptance runs one motif through the SHIPPED write gate, so
// the acceptance replay measures what the real validator would accept rather
// than what a test-local rule would.
//
// It goes through SerializeFact rather than reimplementing the shape check —
// MN4 has exactly one validation entry point, and an acceptance harness with
// its own copy would be measuring a different product.
func ValidateMotifForAcceptance(motif string) error {
	f := fact.NewFact("kb/acceptance/probe.md")
	f.Title = "Probe"
	f.Body = "Probe body."
	f.Type = fact.Observation
	f.Domain = []string{"probe"}
	f.Entities = []string{"Probe"}
	f.Refs = []string{}
	f.Confidence = 0.8
	f.Sources = 1
	f.Motifs = []string{motif}
	_, err := fact.SerializeFact(f)
	return err
}

// The roadmap's other Phase-2 acceptance item, stated directly: a planted
// synonym pair resolves to ONE canonical id with NEITHER FACT CHANGING.
//
// Both halves matter and they pull against each other — resolving two spellings
// together is trivial if you are allowed to rewrite one of them, and MN3's
// whole content is that you are not. The blob-hash assertion is the half that
// makes the first half mean anything.
func TestAcceptance_PlantedSynonymResolvesWithoutTouchingEitherFact(t *testing.T) {
	ctx := context.Background()
	svc, branch := motifEnv(t)

	writeMotifFact(t, svc, branch, "kb/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/two.md", []string{"silent-fallbacks"})

	before := map[string]string{}
	for _, p := range []string{"kb/one.md", "kb/two.md"} {
		rec, err := svc.FactQuery().GetByPath(ctx, branch, p)
		require.NoError(t, err)
		before[p] = rec.BlobHash
	}

	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	a, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	b, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallbacks")
	require.NoError(t, err)
	require.Equal(t, a, b, "the planted synonym pair must resolve to one canonical id")

	clusters, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	require.Len(t, clusters, 1, "...and to ONE vocabulary entry, not two that happen to agree")
	require.Equal(t, 2, clusters[0].DF, "with both facts counted as its carriers")

	for p, want := range before {
		rec, err := svc.FactQuery().GetByPath(ctx, branch, p)
		require.NoError(t, err)
		require.Equal(t, want, rec.BlobHash,
			"MN3: resolving the vocabulary must not rewrite %s — the authored strings "+
				"ARE the claim, and everything built on them is derived", p)
		require.Contains(t, rec.Motifs, map[string]string{
			"kb/one.md": "silent-fallback", "kb/two.md": "silent-fallbacks",
		}[p], "each fact still carries the spelling ITS author wrote")
	}
}
