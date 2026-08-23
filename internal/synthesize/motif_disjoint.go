package synthesize

// Subject-disjointness — blueprint §4 gate 3, as amended by GATE RIDER 1
// (designer ruling 2026-08-23,
// .claude/plans/motif/2026-08-23-phase3-rulings-1.md Q3).
//
// Two facts bridge on a shared mechanism only if they are about DIFFERENT
// subjects; a "bridge" between two facts about the same thing is a duplicate,
// not a discovery (8ebd5d90).
//
// The blueprint's original test — share no non-umbrella entity, domain tag or
// path token — was measured TOO STRICT on the merged corpus: it rejected four
// of the five genuine cross-domain windows on ONE shared coarse tag,
// `evaluation` (gate annex §7.4). Those four share no entities at all and are
// plainly different situations: a UI agent faking clicks, a coding agent gaming
// a terminal benchmark, an RL-tuned agent exploiting a reward.
//
// The finer criterion: a shared label blocks only when it is SPECIFIC enough
// that carrying it is evidence of a shared SUBJECT rather than of a shared
// AREA. Specificity is read off this corpus's own label-df distribution, never
// off a fixed number.

import (
	"sort"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// disjointnessPercentile names WHERE in the corpus's own label-df distribution
// the specificity line falls.
//
// CONSTANT CLASSIFICATION (MN13): a SELECTION POINT, not a df value and not a
// claim about any corpus. The same code on two corpora derives two different
// df cuts from it, which is precisely what MN13 requires of anything that
// would otherwise be a corpus-property constant; the derived cut is printed in
// the session's health lines as a fingerprint. At p90 only the commonest tenth
// of a corpus's labels stop blocking — the `evaluation`-class tags the annex
// indicted, and no more.
const disjointnessPercentile = 90

// minLabelsForPercentile is the population below which the percentile above is
// not an estimate of anything.
//
// CONSTANT CLASSIFICATION (MN13, third class): a STATISTICAL-VALIDITY FLOOR.
// It encodes estimator validity, not a property of any corpus. Below it the
// gate does not invent a cut — it falls back to STRICT (any shared non-umbrella
// label blocks), which is the conservative direction under default-NO, and the
// fallback is reported in health rather than being silent.
const minLabelsForPercentile = 50

// umbrellaPerCent is the umbrella rule (43ae4cac): a label carried by more than
// this share of the corpus proves nothing about a shared subject, and without
// excluding it the gate silently rejects entire corpora.
//
// CONSTANT CLASSIFICATION (MN13): a RATIO of the corpus's own size, never an
// absolute count — the df it resolves to is a different number on every corpus.
const umbrellaPerCent = 20

// disjointnessPoint is the gate's operating point on one corpus, at one moment.
// It is recomputed per session from live frontmatter: nothing about it is
// stored, so a corpus whose character drifts changes its own gate.
type disjointnessPoint struct {
	// Cut is the df at or below which a shared label is specific enough to
	// prove a shared subject.
	Cut int
	// Umbrella is the df above which a label is excluded from the gate.
	Umbrella int
	// Labels is how many distinct labels the point was derived from.
	Labels int
	// Strict reports the validity-floor fallback.
	Strict bool
}

// resolveDisjointnessPoint derives the operating point from a corpus's own
// label distribution.
func resolveDisjointnessPoint(d store.SubjectLabelDF) disjointnessPoint {
	p := disjointnessPoint{
		Labels:   len(d.DF),
		Umbrella: d.LiveFacts * umbrellaPerCent / 100,
	}
	if len(d.DF) < minLabelsForPercentile {
		p.Strict = true
		return p
	}
	vals := make([]int, 0, len(d.DF))
	for _, v := range d.DF {
		vals = append(vals, v)
	}
	sort.Ints(vals)
	// Nearest-rank: the smallest value at or above the requested percentile.
	idx := (disjointnessPercentile*len(vals) + 99) / 100
	if idx > 0 {
		idx--
	}
	p.Cut = vals[idx]
	return p
}

// subjectDisjoint reports whether a and b are about different subjects.
//
// It reads fact.SubjectTokens — the same token set the write-time subject strip
// uses — so the question "does this pair share a subject?" and the question
// "does this motif restate its own fact's subject?" cannot be answered from two
// different vocabularies.
func subjectDisjoint(a, b factForLLM, d store.SubjectLabelDF, p disjointnessPoint) bool {
	at := fact.SubjectTokens(a.Entities, a.Domain, a.File)
	bt := fact.SubjectTokens(b.Entities, b.Domain, b.File)
	for tok := range at {
		if _, shared := bt[tok]; !shared {
			continue
		}
		df := d.DF[tok]
		if df > p.Umbrella {
			continue // umbrella: carried by too much of the corpus to mean anything
		}
		if p.Strict || df <= p.Cut {
			return false // specific enough to be the same subject
		}
	}
	return true
}
