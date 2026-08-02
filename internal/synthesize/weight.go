package synthesize

import (
	"context"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// SourceWeight holds the confidence and sources count from a single source fact,
// used to compute a normalized evidence weight for synthesized facts.
type SourceWeight struct {
	Confidence float64
	Sources    int
}

// WeightStrategy computes a normalized evidence weight in [0, 1) from a set
// of source facts. Implement this interface to add alternative formulas.
type WeightStrategy interface {
	Compute(sources []SourceWeight) float64
}

// SumProductNorm implements WeightStrategy using sum(c*s) / (sum(c*s) + 1).
// This maps [0, ∞) → [0, 1) with diminishing returns as evidence accumulates.
type SumProductNorm struct{}

// Compute returns sum(confidence_i * sources_i) / (sum + 1).
func (SumProductNorm) Compute(srcs []SourceWeight) float64 {
	if len(srcs) == 0 {
		return 0
	}
	sum := 0.0
	for _, s := range srcs {
		sum += s.Confidence * float64(s.Sources)
	}
	return sum / (sum + 1.0)
}

// DefaultWeightStrategy is used by ApplyPruneDecisions and ApplyDistillDecisions.
// Swap to change the formula globally.
var DefaultWeightStrategy WeightStrategy = SumProductNorm{}

// derivedSourcesFloor is the sources count a derived fact carries when it
// pools nothing — no local refs, or every ref unreadable or hypothesis-typed.
// A synthesis is itself one act of corroboration, and letting the count reach
// 0 would erase the fact from every downstream weight: Compute multiplies by
// sources, so a 0-source ref contributes exactly nothing no matter how
// confident it is.
const derivedSourcesFloor = 1

// computeTransfer reads each source path from git once and returns the three
// numbers a TRANSFER-type write needs: the normalized evidence weight, the
// pooled corroboration count, and how many sources were actually readable.
//
// Transfer vs share is the distinction that decides whether pooling is correct
// at all (spec/mbekg.md §5.1). A merge DELETES the facts it consolidates, so
// their corroborations have nowhere else to live and must move to the output
// or be lost — summing is exactly §2.2's semantics. A derivation (distill,
// discover, reflect-propose) leaves its sources alive holding their own
// counts, so summing there would record one observation in two live facts at
// once and inflate multiplicatively on every review cycle. Only merges call
// this; derivations call computeWeight and set sources to 1.
//
// The pooled set is exactly the set weighed: sources that fail to read or
// parse contribute to neither, and hypothesis-typed sources are skipped
// because a conjecture corroborates nothing (§5.2). Letting the two numbers
// disagree would have them describe different evidence.
//
// Must be called before the source facts are deleted — computing afterwards
// reads nothing and reports a fact resting on no evidence at all. readable is
// returned so a destructive caller can tell "these facts had no corroborations"
// apart from "I could not read the facts I am about to delete".
func computeTransfer(ctx context.Context, gs store.FactIndex, agentBranch string, sourcePaths []string) (weight float64, pooled int, readable int) {
	// pooled and readable describe the DIRECT sources only — those are the
	// facts a merge is about to delete, and the lineage beneath them is not
	// being deleted with them. The weight, by contrast, composes through that
	// lineage (collectEvidence).
	for _, p := range sourcePaths {
		f, ok := readFactAt(ctx, gs, agentBranch, p)
		if !ok {
			continue
		}
		// readable counts every source the repository could actually produce,
		// INCLUDING hypotheses. It answers "could I see what I am about to
		// delete?", not "did it corroborate anything" — a merge whose inputs
		// are all conjecture read fine and pooled nothing, which is a true
		// zero, not lost evidence. Folding the hypothesis skip in here would
		// make the destructive caller's warn report a storage failure that
		// never happened.
		readable++
		if f.Type == fact.Hypothesis {
			// A conjecture corroborates nothing (§5.2).
			continue
		}
		pooled += f.Sources
	}
	if pooled < derivedSourcesFloor {
		pooled = derivedSourcesFloor
	}
	return DefaultWeightStrategy.Compute(collectEvidence(ctx, gs, agentBranch, sourcePaths)), pooled, readable
}

// evidenceMaxDepth bounds the lineage walk. Mirrors explainMaxDepth in the
// provenance graph: deep enough that no real derivation chain is truncated
// (RAPTOR caps synthesis recursion at 3), shallow enough that a pathological
// chain cannot make a single fact write walk the corpus.
const evidenceMaxDepth = 10

// collectEvidence walks the lineage reachable from sourcePaths and returns the
// DEDUPLICATED set of terminal facts whose evidence the output rests on.
//
// Setting sources to 1 on share-type derivations (§5.1) made evidence depth
// invisible one level up: every cited synthesis contributed confidence × 1,
// so a synthesis over ten well-corroborated facts scored like one over two
// flimsy ones. Recovering the mass arithmetically from a ref's stored weight
// (w/(1-w), exact since w = S/(S+1)) would restore depth but reintroduce the
// double-count §5.1 removed — two syntheses sharing a leaf each recover it in
// full, and RAPTOR clusters round-1 outputs by similarity, so overlapping refs
// are the expected case. Walking and deduplicating by path is what composes
// depth without counting a corroboration twice.
//
// The walk terminates at exactly the boundaries that make a transitive walk
// impossible for `sources`:
//
//   - AUTHORED facts are terminal. Their refs are citations ("see also"), not
//     lineage, so passing through would import evidence they never rested on.
//     This is also what makes a merge survivor terminal: it is authored-origin
//     and has already pooled its deleted inputs into its own sources, so
//     stopping there is exact and the walk never chases refs to files that no
//     longer exist.
//   - DERIVED facts (distilled, discovered) pass through to their sources,
//     which a share-type derivation left alive — unless none of those sources
//     resolves, in which case the fact falls back to its own mass rather than
//     contributing nothing. That covers a distill that retracted the very
//     facts it cites.
//   - HYPOTHESIS-typed facts contribute nothing and are not traversed, at any
//     depth (§5.2).
//
// Each path is visited at most once, which is what makes shared ancestry count
// once and also breaks ref cycles.
func collectEvidence(ctx context.Context, gs store.FactIndex, agentBranch string, sourcePaths []string) []SourceWeight {
	var out []SourceWeight
	// resolved memoizes each path's OUTCOME, not merely that it was visited.
	// Caching the visit alone conflates "already counted" with "resolved": a
	// dead ref shared by two parents would report unresolved to the first and
	// resolved to the second, so the second would skip its own fallback and
	// contribute nothing — the same fact weighed differently depending on
	// iteration order, and a silent under-count on exactly the retracted-source
	// case the fallback exists for.
	resolved := make(map[string]bool)
	onStack := make(map[string]bool)

	// visit returns whether path resolved to a usable node — including one
	// already FINISHED. "Already counted" must read as resolved, or a second
	// parent sharing the child would see nothing resolve and fall back to its
	// own mass, double-counting the very thing dedup exists to prevent.
	//
	// A path still on the stack is different: that is a back-edge, and
	// reporting it as resolved would let a cycle of derived facts satisfy each
	// other's grounding check and collect nothing at all — a silent zero on a
	// lineage that is merely circular, not absent. Reporting false lets the
	// fallback fire, so the cycle grounds on its own mass like any other dead
	// lineage.
	var visit func(path string, depth int) bool
	visit = func(path string, depth int) bool {
		key := strings.ToLower(path)
		if onStack[key] {
			return false
		}
		if r, ok := resolved[key]; ok {
			return r
		}
		onStack[key] = true
		defer func() { onStack[key] = false }()

		f, ok := readFactAt(ctx, gs, agentBranch, path)
		if !ok {
			resolved[key] = false
			return false
		}
		// Everything below this point contributes or terminates, so the path
		// resolved. Recording it before descending is what keeps a shared
		// ancestor counted once: a second parent reaching it sees resolved and
		// neither re-appends it nor falls back.
		resolved[key] = true
		if f.Type == fact.Hypothesis {
			// Resolved, but a conjecture corroborates nothing — and it must
			// not make its parent fall back either, so report resolved.
			return true
		}

		if f.Origin == fact.Distilled || f.Origin == fact.Discovered {
			if depth < evidenceMaxDepth {
				anyResolved := false
				for _, r := range localLineageRefs(f) {
					if visit(r, depth+1) {
						anyResolved = true
					}
				}
				if anyResolved {
					return true
				}
			}
			// Lineage exhausted, dead, or depth-capped — fall through and
			// count this fact's own mass.
		}

		out = append(out, SourceWeight{Confidence: f.Confidence, Sources: f.Sources})
		return true
	}

	for _, p := range sourcePaths {
		visit(p, 0)
	}
	return out
}

// localLineageRefs is the subset of a fact's refs that name another fact in
// this repository — the edges the lineage walk may follow. External URLs and
// cross-repo kb:// refs name nothing walkable here, and a self-ref (appended
// as merge lineage) is a back-edge the on-stack guard absorbs anyway.
func localLineageRefs(f fact.Fact) []string { return localFactRefPaths(f.Refs) }

// readFactAt reads and parses one fact, reporting whether it resolved. A
// source that cannot be read or parsed contributes nothing — the same
// tolerance the pre-walk implementation had.
func readFactAt(ctx context.Context, gs store.FactIndex, agentBranch, path string) (fact.Fact, bool) {
	readResult, err := gs.ReadFact(ctx, agentBranch, path, nil)
	if err != nil {
		return fact.Fact{}, false
	}
	f, err := fact.ParseFact(path, readResult.Content)
	if err != nil {
		return fact.Fact{}, false
	}
	return f, true
}

// computeWeight returns only the evidence weight, for SHARE-type writes: a
// derivation whose sources stay alive sets its own sources to 1 and has no
// count to pool (see computeTransfer for why).
func computeWeight(ctx context.Context, gs store.FactIndex, agentBranch string, sourcePaths []string) float64 {
	w, _, _ := computeTransfer(ctx, gs, agentBranch, sourcePaths)
	return w
}

// ComputeEvidenceWeight is the exported entry to computeWeight. The learn
// handler uses it so a manually-written derived fact (one an agent marks as a
// machine origin after previewing discovery/synthesis proposals) carries the
// same provenance weight the auto-apply pipeline would have computed.
// sourcePaths must be local fact paths.
func ComputeEvidenceWeight(ctx context.Context, gs store.FactIndex, agentBranch string, sourcePaths []string) float64 {
	return computeWeight(ctx, gs, agentBranch, sourcePaths)
}

// localFactRefPaths is the subset of refs naming a fact in THIS repository, as
// repo-relative paths — the edges a lineage walk or weight computation may
// follow. It consumes fact.ClassifyRef so synthesize cannot drift from the edge
// builder, replay, explain, or the fact API.
//
// The "ends in .md" test this replaced counted a source citation as a local
// edge whenever the cited FILE was markdown — src://knomit/.claude/plans/x.md@c
// ends in ".md" — and then walked a path that names no fact here.
//
// localRepoID is deliberately "": synthesize has no repo identity threaded
// through it, and with an empty id every kb:// ref reads as foreign and is
// excluded. That matches the previous behaviour of localLineageRefs exactly
// (it excluded all kb:// refs), so this is a strict improvement rather than a
// change: it stops counting src:// refs, and counts nothing new. Threading a
// real id would additionally admit kb://<own-id>/… lineage edges.
func localFactRefPaths(refs []string) []string {
	var out []string
	for _, r := range refs {
		if c := fact.ClassifyRef(r, ""); c.Kind == fact.RefLocalFact {
			out = append(out, c.Path)
		}
	}
	return out
}
