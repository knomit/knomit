package synthesize

// REINFORCE — discovery's third outcome (GATE rider 3, designer ruling
// 2026-08-23).
//
// The far-lane demo established that the most likely failure of a discovery
// task is re-deriving a fact the corpus already holds, and that the shipped
// machinery cannot catch it: the 0.92 embedding gate sees only verbatim
// duplicates, and a genuine semantic restatement shares almost no surface
// wording with the fact it restates (90d69628). Novelty therefore rests on the
// agent querying the corpus — and once it has, a hit is not a dead end. The
// seeds are an INDEPENDENT derivation of a claim already held, and recording
// that is corroboration: one more proof the wheel is round.
//
// THE WRITE CONTRACT (phase3-rulings-4 and -5, from the fresh-eyes review).
// This path edits facts the agent did not author, so it may change ONLY the
// refs, sources and evidence_weight lines of the STORED BYTES. Everything below
// exists to make that true mechanically rather than carefully: the write does
// not try to preserve each field one by one, it proves the rewrite is
// meaning-preserving and refuses otherwise.

import (
	"context"
	"fmt"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/refs"
	"knomit/internal/store"
)

// permittedRewriteLines are the frontmatter keys a reinforcement may change.
var permittedRewriteLines = map[string]struct{}{
	"refs": {}, "sources": {}, "evidence_weight": {},
}

// applyReinforcements runs every reinforcement through its gate chain and
// strengthens the survivors.
//
// The equivalence claim gets alias-judge discipline: a stated one-sentence
// reason, recorded in the commit message — this repo's provenance carrier for
// fact writes — and default-NO whenever it is missing. The entailment gate
// (626f3970) is the mechanical backstop once it exists; until then these gates
// and the agent's own default-NO are what stand.
//
// No error return: every rejection is per-reinforcement, warned and skipped,
// like the proposal loop it runs beside.
func applyReinforcements(
	ctx context.Context,
	gs store.FactIndex,
	idx SearchQuery,
	payload DiscoverWorkPayload,
	rs []FactReinforcement,
	branch string,
	localRepoID string,
	onProgress func(ProgressEvent),
) []string {
	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}
	seedPaths := make(map[string]struct{}, len(payload.Bridge.Members))
	for _, m := range payload.Bridge.Members {
		seedPaths[m.File] = struct{}{}
	}
	gate := refs.New(localRepoID, refs.FromFactQuery(idx, branch))

	var written []string
	for _, r := range rs {
		reject := func(format string, args ...any) {
			onProgress(ProgressEvent{Phase: "warn",
				Message: "reinforce rejected: " + fmt.Sprintf(format, args...)})
		}

		if strings.TrimSpace(r.Reason) == "" {
			// An equivalence nobody could justify in a sentence is the
			// hallucinated equivalence this gate exists to stop, and an
			// over-merge is invisible everywhere downstream.
			reject("%s carries no stated equivalence reason", r.Path)
			continue
		}
		if _, isSeed := seedPaths[r.Path]; isSeed {
			reject("%s is one of the seeds — a fact is not an independent derivation of itself", r.Path)
			continue
		}
		if !refsCoverSeeds(r.Refs, seedPaths) {
			reject("%s refs does not cite every seed", r.Path)
			continue
		}

		read, err := gs.ReadFact(ctx, branch, r.Path, nil)
		if err != nil {
			reject("%s is not readable on this branch: %v", r.Path, err)
			continue
		}
		f, err := fact.ParseFact(r.Path, read.Content)
		if err != nil {
			reject("%s does not parse: %v", r.Path, err)
			continue
		}

		// TRIPWIRE (review H1). ParseFact is deliberately lenient: it DROPS
		// motifs that today's rules reject and records them only in
		// MotifWarnings, which is not persisted. Writing such a fact back
		// deletes them permanently — measured, on a planted `legacy` motif —
		// and MN3 says nothing ever rewrites a fact's motif strings. A fact
		// carrying legacy data is not this path's to clean.
		//
		// The meaning-preservation check below would also catch this, since the
		// motifs line changes. The tripwire stays anyway, first, because it
		// names WHY the fact was skipped: "would be rewritten outside the
		// permitted lines" sends a reader looking for a formatting problem, and
		// a skip nobody can explain is a skip somebody later "fixes".
		//
		// STANDING CAVEAT (reviewer, rulings-5): the fidelity measurement's
		// zero motif-loss column is NOT evidence about this path. No production
		// fact carries motifs yet — the shipped DBs predate the field — so the
		// planted-motif test is the only evidence there is. Never read a
		// measured zero here as an all-clear.
		if len(f.MotifWarnings) > 0 {
			reject("%s carries motifs this version cannot round-trip (%s) — "+
				"reinforcing it would delete them", r.Path, strings.Join(f.MotifWarnings, "; "))
			continue
		}

		// The fact re-rendered UNCHANGED. If that already differs from what is
		// stored in a way that changes meaning, this is not the path that
		// rewrites it.
		baseline, err := fact.SerializeFact(f)
		if err != nil {
			reject("%s does not re-serialize: %v", r.Path, err)
			continue
		}
		if why, ok := rewriteIsMeaningPreserving(read.Content, baseline, f.Origin, f.Origin); !ok {
			reject("%s would be rewritten outside the permitted lines (%s)", r.Path, why)
			continue
		}

		// PRIOR IS A SNAPSHOT (0ee925f4). Passing the same slice as both refs
		// and prior silently disables the gate, so prior is copied BEFORE the
		// write list is built from it and the two can diverge.
		prior := append([]string(nil), f.Refs...)

		// SEEDS ONLY (review H2). refsCoverSeeds is a superset check, so r.Refs
		// may name anything the model happened to read while deciding — and
		// rider 2 now instructs it to read. Every extra would become a permanent
		// DERIVED_FROM edge on a fact someone else authored, and would move its
		// evidence weight. Surplus citation does not kill an otherwise valid
		// reinforcement; the extras are dropped with a warning.
		var extras, seeds []string
		for _, ref := range r.Refs {
			if _, isSeed := seedPaths[ref]; isSeed {
				seeds = append(seeds, ref)
			} else {
				extras = append(extras, ref)
			}
		}
		if len(extras) > 0 {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf(
				"reinforce %s: discarded %d ref(s) naming facts outside the bridge: %s",
				r.Path, len(extras), strings.Join(extras, ", "))})
		}

		// EXISTING REFS VERBATIM (review L8). Only the incoming seeds are
		// canonicalised; the strings the fact already carried are written back
		// exactly as they were, because canonicalising them is a mutation of
		// authored data outside "append the seeds". Membership is tested on the
		// canonical form so a bare-path existing ref and its canonical seed do
		// not both land.
		canonSeeds, _ := gate.Canonicalize(seeds)
		canonPrior, _ := gate.Canonicalize(prior)
		have := make(map[string]struct{}, len(canonPrior))
		for _, ref := range canonPrior {
			have[ref] = struct{}{}
		}
		newRefs := append([]string(nil), f.Refs...)
		added := 0
		for _, ref := range canonSeeds {
			if _, dup := have[ref]; dup {
				continue // already a derivation path of this fact, in some form
			}
			have[ref] = struct{}{}
			newRefs = append(newRefs, ref)
			added++
		}
		if added == 0 {
			// Every seed is already one of this fact's derivation paths.
			// Recording it again would turn `sources` into a count of how often
			// the fact was LOOKED at rather than how often it was independently
			// derived.
			onProgress(ProgressEvent{Phase: "detail-discover",
				Message: "reinforce skipped (already derived from these seeds): " + r.Path})
			continue
		}

		// CheckBatch rather than Apply: Apply canonicalises the whole write
		// list, which is exactly the existing-ref rewrite L8 reported. The check
		// itself is the one every write path runs, prior snapshot and all.
		if gerr := gate.CheckBatch(ctx,
			map[string][]string{f.Path(): newRefs},
			map[string][]string{f.Path(): prior}); gerr != nil {
			reject("%s: %v", r.Path, gerr)
			continue
		}

		f.Refs = newRefs
		f.Sources++
		f.EvidenceWeight = computeWeight(ctx, gs, branch, localRepoID,
			localFactRefPaths(newRefs, localRepoID))

		content, err := fact.SerializeFact(f)
		if err != nil {
			reject("%s serialize: %v", r.Path, err)
			continue
		}
		msg := fmt.Sprintf("reinforce: %s independently derived via bridge %q — %s",
			r.Path, payload.Bridge.Token, firstLine(r.Reason))
		if _, err := gs.WriteFact(ctx, branch, f.Path(), content, msg, "discover"); err != nil {
			reject("%s write: %v", r.Path, err)
			continue
		}
		onProgress(ProgressEvent{Phase: "detail-discover", Message: "reinforce " + f.Path()})
		written = append(written, f.Path())
	}
	return written
}

// rewriteIsMeaningPreserving reports whether rewriting `stored` as `rewritten`
// changes only what a reinforcement is allowed to change, and if not, why.
//
// MEANING-CHANGE, not byte equality (designer ruling, phase3-rulings-5). A
// strict byte comparison would refuse the 144 measured facts whose only
// difference is a materialised origin line, and the 159 whose refs line differs
// in YAML quote style — neither changes what the fact says.
//
// Permitted:
//   - any difference confined to the refs / sources / evidence_weight lines,
//     which is where a reinforcement writes and where rendering differences land;
//   - an `origin:` line present in the rewrite and ABSENT from the stored bytes,
//     whose value is what ParseFact already defaults to for that fact.
//
// Everything else skips — including an origin line whose value CHANGES, which
// is the 66 measured facts whose stored type/origin pairing is illegal and whose
// provenance a rewrite would silently reattribute to `authored`.
//
// storedOrigin/rewrittenOrigin are the PARSED origins of the two byte strings.
// On the reinforcement path they are necessarily equal — nothing here mutates
// Origin between parse and serialize — so that clause cannot fail in vivo. It is
// kept deliberately: it is what stops a future origin-mutating path (a repair
// pass, a retype, the belief-level lineage work) from riding through under the
// no-op exemption, and its test drives this function directly for exactly that
// reason. Do not delete it as dead logic.
func rewriteIsMeaningPreserving(stored, rewritten string, storedOrigin, rewrittenOrigin fact.Origin) (string, bool) {
	storedLines := frontmatterLines(stored)
	rewrittenLines := frontmatterLines(rewritten)

	for key, after := range rewrittenLines {
		before, existed := storedLines[key]
		if existed && before == after {
			continue
		}
		if _, permitted := permittedRewriteLines[key]; permitted {
			continue
		}
		if key == "origin" && !existed {
			if storedOrigin != rewrittenOrigin {
				return fmt.Sprintf("origin would materialise as %q, not the parsed default %q",
					rewrittenOrigin, storedOrigin), false
			}
			continue // the measured semantic no-op
		}
		if !existed {
			return "line " + key + " would be added", false
		}
		return "line " + key + " would change", false
	}
	for key := range storedLines {
		if _, still := rewrittenLines[key]; still {
			continue
		}
		if _, permitted := permittedRewriteLines[key]; permitted {
			continue
		}
		return "line " + key + " would be removed", false
	}
	// The body is everything after the frontmatter, and it is never this path's
	// to touch.
	if factBodyOf(stored) != factBodyOf(rewritten) {
		return "the body would change", false
	}
	return "", true
}

// frontmatterLines splits a fact's frontmatter into key → raw line. Keys are
// top-level only; fact frontmatter is flat, and SerializeFact renders lists in
// flow style, so a value never spans lines.
func frontmatterLines(content string) map[string]string {
	out := map[string]string{}
	fm, _, found := strings.Cut(strings.TrimPrefix(content, "---\n"), "\n---")
	if !found {
		return out
	}
	for _, line := range strings.Split(fm, "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "-") {
			continue
		}
		key, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = line
	}
	return out
}

// factBodyOf returns everything after the frontmatter.
func factBodyOf(content string) string {
	_, body, found := strings.Cut(strings.TrimPrefix(content, "---\n"), "\n---")
	if !found {
		return content
	}
	return body
}
