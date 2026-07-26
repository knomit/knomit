package main

import (
	"fmt"
	"math/rand"

	"knomit/internal/fact"
)

// factSlot is a single fact's structural assignment: everything corpusgen
// itself decides (topic, category, kind/type, confidence/sources, and any
// shared-ref/keyword-overlap instruction), before the LLM writes its prose.
// Keeping these seeded-deterministic (not LLM-chosen) is what makes a
// corpus's STRUCTURE reproducible across runs even though the generated
// text itself isn't byte-identical rerun-to-rerun.
type factSlot struct {
	Index         int
	Topic         string
	Category      string
	Kind          fact.Kind
	Type          fact.Type
	Confidence    float64
	Sources       int
	SharedRefURL  string // synthetic mode only: "" if this fact isn't part of a shared-ref group
	SharedKeyword string // "" if this fact isn't part of a keyword-overlap group
	ResearchHint  string // real mode only: "" if this fact isn't part of a shared-research-angle group
}

// epistemicTypeWeight is the sampling weight for each epistemic type in a
// generated corpus. Synthesis gets deliberate weight even though it's the
// rarer type in a real hand-authored corpus: knomit_hypothesize's backward
// pool filters to IncludeTypes=synthesis only (see
// .claude/plans/yake-forward-vs-backward-pool-report.md), so a corpus with
// too few synthesis-typed facts would starve that pool regardless of size.
var epistemicTypeWeight = []struct {
	t fact.Type
	w int
}{
	{fact.Observation, 30},
	{fact.Concept, 15},
	{fact.Pattern, 10},
	{fact.Principle, 10},
	{fact.Reference, 5},
	{fact.Process, 5},
	{fact.Insight, 10},
	{fact.Synthesis, 15},
}

func sampleType(rng *rand.Rand) fact.Type {
	total := 0
	for _, e := range epistemicTypeWeight {
		total += e.w
	}
	r := rng.Intn(total)
	for _, e := range epistemicTypeWeight {
		if r < e.w {
			return e.t
		}
		r -= e.w
	}
	return fact.Observation
}

// sampleConfidence draws from a triangular-ish distribution centered around
// ~0.6-0.65 (the average of two uniform draws), matching the kind of spread
// a real hand-authored corpus shows rather than everything clustering at a
// single value.
func sampleConfidence(rng *rand.Rand) float64 {
	base := (rng.Float64() + rng.Float64()) / 2
	c := 0.3 + base*0.65
	return float64(int(c*100)) / 100 // round to 2 decimals
}

func sampleSources(rng *rand.Rand) int {
	r := rng.Float64()
	switch {
	case r < 0.70:
		return 1
	case r < 0.90:
		return 2
	default:
		return 3
	}
}

// buildNarrowSlots assigns every fact in a narrow-diversity corpus to a
// single fixed topic, cycling through that topic's real leaf categories
// (from the parsed ontology), plus shared-ref/research-hint and
// keyword-overlap group assignments per sharedRefsRate/keywordOverlapRate.
// contentSource selects which shared-citation mechanic applies: "synthetic"
// scripts a fake shared URL (assignSharedRefGroups); "real" instead assigns a
// shared research angle for a group to independently investigate
// (assignResearchHintGroups), windowed to batchSize so a group has a chance
// of landing in the same LLM call — see refs_pool.go for why that matters.
func buildNarrowSlots(o *fact.Ontology, topic string, size int, contentSource string, batchSize int, sharedRefsRate, keywordOverlapRate float64, rng *rand.Rand) ([]factSlot, error) {
	if topic == "" {
		return nil, fmt.Errorf("--topic is required for --diversity narrow")
	}
	categories, err := leafCategories(o, topic)
	if err != nil {
		return nil, err
	}

	slots := make([]factSlot, size)
	for i := range slots {
		slots[i] = factSlot{
			Index:      i,
			Topic:      topic,
			Category:   categories[rng.Intn(len(categories))],
			Kind:       fact.Epistemic,
			Type:       sampleType(rng),
			Confidence: sampleConfidence(rng),
			Sources:    sampleSources(rng),
		}
	}

	switch contentSource {
	case "real":
		assignResearchHintGroups(slots, batchSize, sharedRefsRate, rng)
	default:
		assignSharedRefGroups(slots, topic, sharedRefsRate, rng)
	}
	assignKeywordGroups(slots, keywordOverlapRate, rng)
	return slots, nil
}

// buildSlots dispatches on the diversity profile. Only "narrow" is
// implemented for the current pilot pass (see
// .claude/plans/floofy-shimmying-allen.md) — broad/realistic-mixed are
// deferred until the narrow pilots validate cleanly.
func buildSlots(o *fact.Ontology, diversity, topic string, size int, contentSource string, batchSize int, sharedRefsRate, keywordOverlapRate float64, rng *rand.Rand) ([]factSlot, error) {
	switch diversity {
	case "narrow":
		return buildNarrowSlots(o, topic, size, contentSource, batchSize, sharedRefsRate, keywordOverlapRate, rng)
	default:
		return nil, fmt.Errorf("--diversity %q not yet implemented (only \"narrow\" is supported in this pass)", diversity)
	}
}
