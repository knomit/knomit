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

import (
	"context"
	"fmt"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/refs"
	"knomit/internal/store"
)

// applyReinforcements runs every reinforcement through its gate chain and
// strengthens the survivors.
//
// What a reinforcement may change: refs (append only), sources (+1),
// evidence_weight (recomputed, never authored). NOTHING else — the title, body,
// type, kind, domain, entities, motifs, confidence and origin of a fact the
// agent did not author are not this path's to touch. That list is guarded by
// test rather than by comment.
//
// The equivalence claim itself gets alias-judge discipline: a stated
// one-sentence reason, recorded in the commit message — this repo's provenance
// carrier for fact writes — and default-NO whenever it is missing. The
// entailment gate (626f3970) is the mechanical backstop once it exists; until
// then these gates and the agent's own default-NO are what stand.
func applyReinforcements(
	ctx context.Context,
	gs store.FactIndex,
	idx SearchQuery,
	payload DiscoverWorkPayload,
	rs []FactReinforcement,
	branch string,
	localRepoID string,
	onProgress func(ProgressEvent),
) ([]string, error) {
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

		// PRIOR IS A SNAPSHOT (0ee925f4). Passing the same slice as both refs
		// and prior silently disables the gate, so prior is copied BEFORE the
		// write list is built from it and the two can diverge.
		prior := append([]string(nil), f.Refs...)
		writeList := dedupeRefs(append(append([]string(nil), f.Refs...), r.Refs...))

		canonRefs, _, gerr := gate.Apply(ctx, f.Path(), writeList, prior)
		if gerr != nil {
			reject("%s: %v", r.Path, gerr)
			continue
		}
		canonPrior, _ := gate.Canonicalize(prior)
		if sameRefSet(canonRefs, canonPrior) {
			// Every seed is already one of this fact's derivation paths.
			// Recording it again would turn `sources` into a count of how often
			// the fact was LOOKED at rather than how often it was independently
			// derived.
			onProgress(ProgressEvent{Phase: "detail-discover",
				Message: "reinforce skipped (already derived from these seeds): " + r.Path})
			continue
		}

		f.Refs = canonRefs
		f.Sources++
		f.EvidenceWeight = computeWeight(ctx, gs, branch, localRepoID,
			localFactRefPaths(canonRefs, localRepoID))

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
	return written, nil
}

// dedupeRefs preserves order and drops repeats, so a seed the fact already
// cites does not appear twice in the write list.
func dedupeRefs(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, r := range in {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}

// sameRefSet reports whether two ref lists carry the same members.
func sameRefSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, r := range a {
		seen[r] = struct{}{}
	}
	for _, r := range b {
		if _, ok := seen[r]; !ok {
			return false
		}
	}
	return true
}
