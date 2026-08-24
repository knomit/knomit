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
	// Strict reports that a conservative fallback is in force: any shared
	// non-universal label blocks.
	Strict bool
	// Fallback names WHICH one, because they are different diagnoses of the
	// same corpus and a reader debugging one that bridges nothing needs to
	// tell them apart: "too few labels to estimate from" is not "the estimate
	// came back meaningless".
	Fallback disjointnessFallback
}

// disjointnessFallback names why the gate is running conservatively.
type disjointnessFallback string

const (
	noFallback disjointnessFallback = ""
	// labelFloor: fewer labels than the percentile needs to be an estimate.
	labelFloor disjointnessFallback = "label-floor"
	// degenerateCut: enough labels, but the percentile came back below 2.
	degenerateCut disjointnessFallback = "degenerate-cut"
)

// minUsableCut is the smallest cut a percentile can yield and still gate
// anything.
//
// CONSTANT CLASSIFICATION (MN13, third class): a STATISTICAL-VALIDITY FLOOR,
// on the value the estimator YIELDS rather than on the population it is
// estimated from — the same logic as minLabelsForPercentile, one step later.
//
// The gate blocks when a shared label's df <= Cut, and a SHARED label has
// df >= 2 by construction. A cut of 1 is therefore unsatisfiable: not a strict
// gate, not a loose gate, NO gate. It is what p90 returns whenever >=90% of a
// corpus's labels are hapax, which a young repo dominated by path and uuid
// tokens is — measured at 57 labels / 46 live facts, past the label floor and
// so getting no protection from it either.
//
// Below the label floor the strict fallback covers the corpus; with a mature
// distribution p90 lands at 3-5 and the percentile covers it; between them
// there was a band with neither, and the health line said `df <= 1` as though
// the gate were working. Designer ruling, phase4-rulings-5: fall back to
// strict, because at the scale real repos actually reach — hundreds of facts,
// not thousands — that band is near the STEADY STATE, and strict costs the
// axis nothing under the gems framing (a genuine cross-domain pair shares no
// labels and passes strict untouched).
const minUsableCut = 2

// resolveDisjointnessPoint derives the operating point from a corpus's own
// label distribution.
func resolveDisjointnessPoint(d store.SubjectLabelDF) disjointnessPoint {
	p := disjointnessPoint{
		Labels:   len(d.DF),
		Umbrella: d.LiveFacts * umbrellaPerCent / 100,
	}
	if len(d.DF) < minLabelsForPercentile {
		p.Strict = true
		p.Fallback = labelFloor
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
	if p.Cut < minUsableCut {
		p.Strict = true
		p.Fallback = degenerateCut
	}
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
		if d.DF[tok] >= d.LiveFacts && d.LiveFacts > 0 {
			// UNIVERSAL LABELS are excluded in EVERY mode, strict included
			// (designer ruling 2026-08-23, phase3-rulings-5, review L4).
			//
			// A label carried by every live fact distinguishes nothing — the
			// ontology root is one by construction, since every path starts
			// with it. This is a tautology rather than the ratio judgement the
			// strict fallback exists to suspend, which is why it survives below
			// the label floor: without it, "strict" made every pair in the
			// corpus share `kb` and turned the axis OFF rather than
			// conservative (measured: four tests red, FromNothing among them).
			continue
		}
		if p.Strict {
			// STRICT: any shared label blocks, umbrella included.
			//
			// The umbrella exclusion is IGNORED here, and that is the whole
			// point of the fallback. Umbrella is `df > 20% of N`, so on a
			// corpus of nine facts or fewer the cut is 1 — and a SHARED label
			// has df >= 2 by definition, which made every shared label an
			// umbrella and turned the conservative fallback into a no-op in
			// exactly the regime it exists for (Phase-3 review, L4). A ratio is
			// no more valid below the population floor than the percentile it
			// stands beside; MN13's third class applies to both.
			return false
		}
		df := d.DF[tok]
		if df > p.Umbrella {
			continue // umbrella: carried by too much of the corpus to mean anything
		}
		if df <= p.Cut {
			return false // specific enough to be the same subject
		}
	}
	return true
}
