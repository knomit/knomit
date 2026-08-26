// Package synthesize — shared decision application logic used by both the
// headless pipeline and the review orchestrator.
package synthesize

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/refs"
	"knomit/internal/store"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// normalizeFactPath replaces the filename component of a path with an 8-char
// UUID, matching the convention used by learn. This prevents LLM-generated
// filenames (e.g. "chrome-extension-threat-surface-2026.md") from clashing
// with each other or with learn-generated facts. Directory case is normalised
// by fact.NewFact, which lowercases the full path unconditionally.
func normalizeFactPath(path string) string {
	dir := filepath.Dir(path)
	id := uuid.New().String()[:8]
	return dir + "/" + id + ".md"
}

// validateOutputPath rejects LLM-emitted fact paths that don't live under
// the configured ontology root. Returns nil on success or a descriptive
// error suitable for the onProgress warn channel. Paths are compared
// case-insensitively because fact.NewFact lowercases all paths
// unconditionally before persisting.
func validateOutputPath(path, ontologyRoot string) error {
	root := strings.TrimRight(ontologyRoot, "/")
	if root == "" {
		return fmt.Errorf("ontology root not configured")
	}
	prefix := strings.ToLower(root) + "/"
	if !strings.HasPrefix(strings.ToLower(path), prefix) {
		return fmt.Errorf("path %q is outside ontology root %q", path, ontologyRoot)
	}
	// A dot-prefixed segment is private: it would be written and then skipped
	// by the indexer, Verify and the exporter alike, so it is refused here
	// rather than silently discarded. All four of this function's callers
	// (merge, distill, propose, and discovery's emergent-fact write) land on
	// this one rule.
	if fact.IsPrivatePath(path) {
		return fmt.Errorf("%s is private: a path segment beginning with '.' cannot hold a fact", path)
	}
	return nil
}

// validateOutputType rejects LLM-emitted facts with empty or invalid
// epistemic type. Returns nil on success. Empty type is the most common
// failure (LLM omits the field) and historically slipped through to disk
// as `type:` blanks; the API silently masked these by defaulting to
// "observation" on read, hiding the bug.
func validateOutputType(t string) error {
	if t == "" {
		return fmt.Errorf("epistemic type is empty")
	}
	if !fact.Epistemic.AllowsType(fact.Type(t)) {
		return fmt.Errorf("invalid epistemic type %q", t)
	}
	return nil
}

// ReviewStats tracks what actions were taken during a review.
type ReviewStats struct {
	Pruned      int
	Merged      int
	Updated     int
	Synthesized int
	// Retired is every path this apply actually removed from the corpus —
	// retracted facts and merge sources alike, and only those whose delete
	// SUCCEEDED. It is the input to the mid-session refresh of already-queued
	// work items (see inflight.go), which is why it reports what happened
	// rather than what was asked for: an item stripped of a fact that is still
	// live would be a second bug wearing the first one's fix.
	//
	// Not serialised. ReviewStats is embedded in ReviewResult as `summary`,
	// and a path list there would be a wire-shape change for an internal
	// hand-off.
	Retired []string `json:"-"`
}

// ApplyPruneDecisions applies prune decisions (retract/update) and merges to the git store.
// ontologyRoot is the configured fact root (e.g. "kb"); merge outputs whose
// path falls outside this root or whose epistemic type is empty/invalid are
// rejected with a warn rather than written.
func ApplyPruneDecisions(ctx context.Context,
	gs store.FactIndex,
	// idx supplies FactExistsAt — the same resolution predicate every READER
	// uses, which the ref gate must match exactly.
	idx SearchQuery,
	decisions []PruneDecision,
	merges []MergeEntry,
	recipeName string,
	onProgress func(ProgressEvent),
	agentBranch string,
	// localRepoID is this repo's 12-hex id. Refs are stored canonical
	// (kb://<own-id>/<path>), so every lineage filter below needs it to tell a
	// local edge from a foreign one; passing "" reads them all as foreign.
	localRepoID string,
	ontologyRoot string,
) (*ReviewStats, error) {
	stats := &ReviewStats{}
	// Track deleted paths to avoid double-deletion when a path appears in
	// both "retract" decisions and merge source lists.
	deletedPaths := make(map[string]bool)
	// mergeGate is the one gate the merge outputs below go through, built once
	// for the whole call.
	mergeGate := refs.New(localRepoID, refs.FromFactQuery(idx, agentBranch))
	log.Info().Int("decisions", len(decisions)).Int("merges", len(merges)).Msg("prune: applying results")

	// Apply decisions.
	for _, d := range decisions {
		switch d.Action {
		case "keep":
			// no-op
		case "retract":
			msg := fmt.Sprintf("synthesize-%s: retract %s", recipeName, d.Path)
			if _, err := gs.DeleteFact(ctx, agentBranch, d.Path, msg); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("retract %s: %v", d.Path, err)})
				continue
			}
			// Recorded AFTER the delete succeeded. The merge loop below already
			// does this; the retract branch used to mark the path deleted first,
			// which made a FAILED retract look identical to a completed one —
			// harmless while deletedPaths only suppressed double-deletion, and
			// not harmless now that the same set says which facts left the
			// corpus.
			deletedPaths[d.Path] = true
			onProgress(ProgressEvent{Phase: "detail-retract", Message: "retract " + d.Path})
			stats.Pruned++

		case "update":
			readResult, err := gs.ReadFact(ctx, agentBranch, d.Path, nil)
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update read %s: %v", d.Path, err)})
				continue
			}
			f, err := fact.ParseFact(d.Path, readResult.Content)
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update parse %s: %v", d.Path, err)})
				continue
			}
			f.Confidence = d.Confidence
			updated, err := fact.SerializeFact(f)
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update serialize %s: %v", d.Path, err)})
				continue
			}
			msg := fmt.Sprintf("synthesize-%s: update confidence %s → %.2f", recipeName, d.Path, d.Confidence)
			if _, err := gs.WriteFact(ctx, agentBranch, d.Path, updated, msg, "update"); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update write %s: %v", d.Path, err)})
				continue
			}
			onProgress(ProgressEvent{Phase: "detail-update", Message: fmt.Sprintf("update %.2f %s", d.Confidence, d.Path)})
			stats.Updated++
		}
	}

	// Apply merges.
	for _, m := range merges {
		mf := m.Merged
		if err := validateOutputPath(mf.Path, ontologyRoot); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge rejected: %v", err)})
			continue
		}
		if err := validateOutputType(mf.Type); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge %s rejected: %v", mf.Path, err)})
			continue
		}
		// Prune-merges cannot create hypothesis-type facts. Hypotheses
		// are only created by knomit_hypothesize via knomit_learn (the
		// agent-driven path), never by review. The prompt forbids this
		// in merged.type's enum but the schema accepts the LLM's word;
		// this guard mirrors the same check on the distill path so the
		// architectural invariant "review never creates hypotheses" is
		// enforced server-side, not just by prompt.
		if fact.Type(mf.Type) == fact.Hypothesis {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge: skipping hypothesis-type output %s — prune-merge cannot create hypotheses", mf.Path)})
			continue
		}
		// Replace the LLM-generated filename with a UUID, as distill does
		// below and as discovery does in discovery.go (knomit#107a). This was
		// the only one of validateOutputPath's four callers that did not, and
		// validateOutputPath checks the ontology root and private paths and
		// NOTHING ELSE — there is no collision check on this path. So an
		// LLM-supplied merged.path naming a fact that already existed
		// overwrote it whole: body, refs, motifs, origin, gone with no warning.
		//
		// The corpus's own convention note recorded this as a hazard rather
		// than a design — "Do not assume a prune-merge output path is
		// collision-proof by UUID" — which is what a reader had to know in
		// order not to be bitten by it. Now they do not.
		//
		// NORMALIZE rather than reject (designer ruling). Rejecting the merge
		// on collision would silently undo a consolidation the judge asked
		// for, which is the argument the DropInvalidMotifs comment makes a few
		// lines below; the LLM's DIRECTORY choice is still honoured, only the
		// filename is replaced.
		mf.Path = normalizeFactPath(mf.Path)
		// TRANSFER: pooled from the facts being subsumed, not from mf.Sources.
		// The merged fact REPLACES its sources and they are deleted immediately
		// below, so trusting the count the model happened to emit discards the
		// corroborations the merge just absorbed. §5.3 already mandates summing
		// for dedup-on-learn; this is the same merge.
		weight, pooled, readable := computeTransfer(ctx, gs, agentBranch, localRepoID, m.Paths)
		// A merge that can read none of the facts it is about to delete is
		// destroying evidence it never counted. The floor would otherwise
		// dress that up as a plausible sources: 1.
		if readable == 0 && len(m.Paths) > 0 {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf(
				"merge %s: none of the %d subsumed facts could be read — their corroborations are lost",
				mf.Path, len(m.Paths))})
		}
		merged := fact.NewFact(mf.Path)
		merged.Title = mf.Title
		merged.Body = mf.Body
		merged.Type = fact.Type(mf.Type)
		merged.Domain = mf.Domain
		merged.Confidence = mf.Confidence
		merged.Sources = pooled
		merged.Entities = mf.Entities
		// DROP bad motifs rather than losing the fact. These are LLM-proposed
		// and effectively untrusted input; the rest of this merged claim — its
		// body, its refs, the consolidation decision behind it — is good work.
		//
		// Without this, one malformed name ("silent fallback", a space instead
		// of a hyphen) fails SerializeFact and the caller's warn+continue
		// discards the entire merged fact, silently undoing a consolidation the
		// judge asked for. §2.10's read/write asymmetry exists for exactly this
		// shape of input: ignore what you cannot use, keep what you can.
		//
		// The count cap, the shape rule and the subject strip still live in
		// SerializeFact alone (MN4) — this drops entries THAT gate would
		// reject, using that gate's own definition, rather than re-implementing
		// one.
		merged.Motifs = fact.DropInvalidMotifs(mf.Motifs)
		merged.EvidenceWeight = weight

		// Same gate as every other write path. The merged fact is NEW and its
		// refs are wholly LLM-authored, so there is nothing carried forward to
		// exempt. Citing the facts it subsumes (deleted just below) resolves:
		// they are live at the pre-write head and stay reachable by walk-back.
		canonRefs, _, gerr := mergeGate.Apply(ctx, merged.Path(), mf.Refs, nil)
		if gerr != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge %s rejected: %v", merged.Path(), gerr)})
			continue
		}
		merged.Refs = canonRefs

		content, err := fact.SerializeFact(merged)
		if err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge serialize %s: %v", merged.Path(), err)})
			continue
		}
		msg := fmt.Sprintf("synthesize-%s: merge %s", recipeName, strings.Join(m.Paths, ", "))
		if _, err := gs.WriteFact(ctx, agentBranch, merged.Path(), content, msg, "subsume"); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge write %s: %v", merged.Path(), err)})
			continue
		}

		// Delete source facts (losers get retract tag).
		for _, src := range m.Paths {
			if deletedPaths[src] {
				continue
			}
			srcMsg := fmt.Sprintf("synthesize-%s: subsumed by %s", recipeName, merged.Path())
			if _, err := gs.DeleteFact(ctx, agentBranch, src, srcMsg); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge delete source %s: %v", src, err)})
				continue
			}
			deletedPaths[src] = true
		}
		onProgress(ProgressEvent{Phase: "detail-merge", Message: "merge " + merged.Path()})
		stats.Merged++
	}

	// Sorted so the retired set is a deterministic function of what happened,
	// not of Go's map iteration order.
	stats.Retired = make([]string, 0, len(deletedPaths))
	for p := range deletedPaths {
		stats.Retired = append(stats.Retired, p)
	}
	sort.Strings(stats.Retired)

	return stats, nil
}

// ApplyDistillDecisions applies distill results: writes synthesized facts and retracts subsumed ones.
// Returns stats, the written facts (with normalized paths), and any error.
// ontologyRoot is the configured fact root (e.g. "kb"); synthesized facts
// whose path falls outside this root or whose epistemic type is
// empty/invalid are rejected with a warn rather than written.
func ApplyDistillDecisions(ctx context.Context,
	gs store.FactIndex,
	// idx supplies FactExistsAt — the reader's resolution predicate.
	idx SearchQuery,
	synthesized []distillFact,
	retract []string,
	recipeName string,
	onProgress func(ProgressEvent),
	agentBranch string,
	// localRepoID is this repo's 12-hex id. Refs are stored canonical
	// (kb://<own-id>/<path>), so every lineage filter below needs it to tell a
	// local edge from a foreign one; passing "" reads them all as foreign.
	localRepoID string,
	ontologyRoot string,
) (*ReviewStats, []distillFact, error) {
	stats := &ReviewStats{}
	var written []distillFact
	gate := refs.New(localRepoID, refs.FromFactQuery(idx, agentBranch))

	log.Info().Int("synthesized", len(synthesized)).Int("forgotten", len(retract)).Msg("distill: committing results")

	// Commit synthesized facts.
	for _, df := range synthesized {
		if err := validateOutputPath(df.Path, ontologyRoot); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill rejected: %v", err)})
			continue
		}
		if err := validateOutputType(df.Type); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill %s rejected: %v", df.Path, err)})
			continue
		}
		// Distill cannot create hypothesis-type facts.
		if fact.Type(df.Type) == fact.Hypothesis {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill: skipping hypothesis-type output %s — distill cannot create hypotheses", df.Path)})
			continue
		}
		// Replace LLM-generated filename with a UUID to match learn convention.
		df.Path = normalizeFactPath(df.Path)
		localRefs := localFactRefPaths(df.Refs, localRepoID)
		weight := computeWeight(ctx, gs, agentBranch, localRepoID, localRefs)
		f := fact.NewFact(df.Path)
		f.Title = df.Title
		f.Body = df.Body
		f.Type = fact.Type(df.Type)
		f.Domain = df.Domain
		f.Confidence = df.Confidence
		// SHARE, so 1: a distilled fact is a NEW claim produced by one act of
		// synthesis, and the facts it cites stay alive holding their own
		// counts. Pooling them here would record one observation in two live
		// facts at once, and RAPTOR distills over its own output — so the
		// inflation compounds per level. The underlying evidence is already
		// carried by EvidenceWeight above.
		f.Sources = 1
		f.Entities = df.Entities
		f.Motifs = fact.DropInvalidMotifs(df.Motifs) // see the merge path above
		f.EvidenceWeight = weight
		df.Path = f.Path() // sync df so written slice reflects the canonical (lowercase) path

		// Same gate as knomit_learn and the REST writers. The pipeline is a
		// write path like any other, and its refs come from an LLM — the most
		// likely producer of a path that does not exist. `retract` is passed
		// because a distilled fact SUBSUMES the facts it cites: they are still
		// present now and are deleted below, and citing what you retract is
		// lineage, not a dangling ref.
		//
		// A rejection warns and skips this one fact, matching how every other
		// validation failure here behaves — one bad proposal must not abort a
		// review that produced good ones.
		canonRefs, _, gerr := gate.Apply(ctx, f.Path(), df.Refs, nil)
		if gerr != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill %s rejected: %v", f.Path(), gerr)})
			continue
		}
		f.Refs = canonRefs

		content, err := fact.SerializeFact(f)
		if err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill serialize %s: %v", f.Path(), err)})
			continue
		}
		msg := fmt.Sprintf("synthesize-%s: distill %s", recipeName, f.Path())
		if _, err := gs.WriteFact(ctx, agentBranch, f.Path(), content, msg, "subsume"); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill write %s: %v", f.Path(), err)})
			continue
		}
		onProgress(ProgressEvent{Phase: "detail-learn", Message: "learn " + f.Path()})
		stats.Synthesized++
		written = append(written, df)
	}

	// Delete subsumed facts.
	for _, path := range retract {
		msg := fmt.Sprintf("synthesize-%s: subsumed by distilled fact", recipeName)
		if _, err := gs.DeleteFact(ctx, agentBranch, path, msg); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill retract %s: %v", path, err)})
			continue
		}
		onProgress(ProgressEvent{Phase: "detail-distill-retract", Message: "retract " + path})
		stats.Pruned++
	}

	return stats, written, nil
}

// ApplyReflectDecisions is the side-effect channel for the reflect step.
// It is symmetric with ApplyPruneDecisions / ApplyDistillDecisions: the
// agent's response IS the write contract.
//
// Two arms:
//   - reinforce: each entry binds an existing methodology fact to the
//     transitions it explained. The methodology path is appended to each
//     transition fact's refs, so the transition (the promoted/retracted
//     hypothesis) cites the methodology that explains it. Reinforcement
//     count for a methodology emerges as the number of facts whose refs
//     contain its path — no separate counter, no side-channel table.
//   - propose: each entry is a brand-new methodology fact. Server-stamped
//     type=methodology; rejected if too similar to an existing methodology
//     (cosine ≥ noveltyThreshold).
//
// All structural and DB-resolved validation runs before any writes. Both
// proposed methodologies and updated transition facts are committed in a
// single BatchWriteFacts call so the reflect step lands atomically per
// session. Caller is expected to have already run validateReflectResponse
// for structural checks.
//
// onProgress is tolerated as nil.
func ApplyReflectDecisions(
	ctx context.Context,
	gs store.FactIndex,
	idx SearchQuery,
	result ReflectResult,
	sess *store.PipelineSession,
	// localRepoID is this repo's 12-hex id, for the ref gate below.
	localRepoID string,
	ontologyRoot string,
	noveltyThreshold float64,
	onProgress func(ProgressEvent),
) error {
	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}

	branch := sess.Branch

	// files accumulates everything to commit in this reflect application —
	// proposed methodology facts plus reinforced transition facts (with
	// their refs extended). One commit, atomic.
	files := make(map[string]string)
	// The same facts, kept parsed, so the ref gate below can check and
	// canonicalize them without re-parsing what this function just built.
	factsByPath := make(map[string]fact.Fact)
	refsByPath := make(map[string][]string)
	// A reinforced transition fact is an EXISTING fact gaining one methodology
	// ref. Everything it already cited resolved at its own commit and is not
	// re-judged here.
	priorRefsByPath := make(map[string][]string)
	reflectGate := refs.New(localRepoID, refs.FromFactQuery(idx, branch))

	// Phase 1 — validate reinforce targets resolve to methodology facts and
	// stage transition-fact updates appending the methodology path to refs.
	for i, e := range result.Reinforce {
		mr, err := gs.ReadFact(ctx, branch, e.MethodologyPath, nil)
		if err != nil {
			return fmt.Errorf("reinforce[%d]: methodology %q not found: %w", i, e.MethodologyPath, err)
		}
		mf, err := fact.ParseFact(e.MethodologyPath, mr.Content)
		if err != nil {
			return fmt.Errorf("reinforce[%d]: cannot parse %q: %w", i, e.MethodologyPath, err)
		}
		if mf.Type != fact.Methodology {
			return fmt.Errorf("reinforce[%d]: %q is type %q, not methodology", i, e.MethodologyPath, mf.Type)
		}

		for _, tp := range e.TransitionPaths {
			tr, err := gs.ReadFact(ctx, branch, tp, nil)
			if err != nil {
				return fmt.Errorf("reinforce[%d]: transition fact %q not found: %w", i, tp, err)
			}
			tf, err := fact.ParseFact(tp, tr.Content)
			if err != nil {
				return fmt.Errorf("reinforce[%d]: cannot parse transition %q: %w", i, tp, err)
			}
			before := append([]string(nil), tf.Refs...)
			if appendRefIfMissing(&tf.Refs, e.MethodologyPath, localRepoID) {
				serialized, err := fact.SerializeFact(tf)
				if err != nil {
					return fmt.Errorf("reinforce[%d]: serialize transition %q: %w", i, tp, err)
				}
				files[tf.Path()] = serialized
				factsByPath[tf.Path()] = tf
				refsByPath[tf.Path()] = tf.Refs
				priorRefsByPath[tf.Path()] = before
			}
		}
		onProgress(ProgressEvent{Phase: "detail-reflect-reinforce",
			Message: fmt.Sprintf("reinforced %s with %d transitions", e.MethodologyPath, len(e.TransitionPaths))})
	}

	// Phase 2 — validate and stage propose entries. Novelty gate runs
	// before serialization; staged propose facts share the commit batch
	// with the transition updates from Phase 1.
	for i, p := range result.Propose {
		topic, category, err := splitTopicPath(p.TopicPath, ontologyRoot)
		if err != nil {
			return fmt.Errorf("propose[%d]: %w", i, err)
		}
		path := fact.BuildFactPath(ontologyRoot, topic, category)
		if err := validateOutputPath(path, ontologyRoot); err != nil {
			return fmt.Errorf("propose[%d]: %w", i, err)
		}

		// Novelty gate: search the existing methodology corpus on this
		// branch for anything within similarity threshold of (title+body).
		// Search will internally embed the Text via the configured
		// embedder; if no embedder is wired up, this falls back to
		// keyword/tag scoring — still a useful guard, just looser.
		hits, err := idx.Search(ctx, branch, store.SearchOptions{
			Text:          p.Title + "\n\n" + p.Body,
			IncludeTypes:  []string{string(fact.Methodology)},
			MinSimilarity: noveltyThreshold,
			Limit:         5,
		})
		if err != nil {
			return fmt.Errorf("propose[%d]: novelty search: %w", i, err)
		}
		if len(hits) > 0 {
			top := hits[0]
			score := top.Score / 100.0
			return fmt.Errorf(
				"propose[%d]: too similar to existing methodology %q (cosine %.2f ≥ threshold %.2f); reinforce it instead",
				i, top.Path, score, noveltyThreshold,
			)
		}

		f := fact.NewFact(path)
		f.Title = p.Title
		f.Body = p.Body
		f.Type = fact.Methodology // server-stamped; agent input ignored
		f.Domain = []string(p.Domain)
		f.Entities = []string(p.Entities)
		f.Confidence = p.Confidence
		// SHARE, so 1: a methodology's refs are explanatory citations — the
		// transitions that motivated it — not corroborations OF it, and they
		// stay alive holding their own counts.
		f.Sources = 1
		f.Refs = []string(p.Refs)
		serialized, err := fact.SerializeFact(f)
		if err != nil {
			return fmt.Errorf("propose[%d]: serialize: %w", i, err)
		}
		files[f.Path()] = serialized
		factsByPath[f.Path()] = f
		refsByPath[f.Path()] = f.Refs
		onProgress(ProgressEvent{Phase: "detail-reflect-propose", Message: "wrote methodology " + f.Path()})
	}

	// Phase 3 — single atomic commit covering both arms. If there's
	// nothing to write (empty reinforce + empty propose, or every
	// transition already cited the methodology), this is a no-op.
	if len(files) > 0 {
		// Same gate as every other write path, applied to the whole batch so a
		// proposed methodology and a transition citing it satisfy each other —
		// they land in ONE commit. Reflect is all-or-nothing (unlike distill's
		// warn-and-skip) because both arms already return errors rather than
		// degrading, and a half-applied reflect is what the atomic commit here
		// exists to prevent.
		if err := reflectGate.CheckBatch(ctx, refsByPath, priorRefsByPath); err != nil {
			return fmt.Errorf("apply reflect: %w", err)
		}
		for path, f := range factsByPath {
			canon, changed := reflectGate.Canonicalize(f.Refs)
			if !changed {
				continue
			}
			f.Refs = canon
			serialized, serr := fact.SerializeFact(f)
			if serr != nil {
				return fmt.Errorf("apply reflect: re-serialize %q: %w", path, serr)
			}
			files[path] = serialized
		}

		commitMsg := fmt.Sprintf("review: reflect (reinforce=%d propose=%d)",
			len(result.Reinforce), len(result.Propose))
		if _, _, err := gs.BatchWriteFacts(ctx, branch, files, nil, commitMsg, "review"); err != nil {
			return fmt.Errorf("apply reflect: write: %w", err)
		}
	}

	log.Info().
		Str("session", sess.ID).
		Int("reinforced", len(result.Reinforce)).
		Int("proposed", len(result.Propose)).
		Msg("review: reflect applied")

	return nil
}

// appendRefIfMissing appends ref to *refs if not already present. Returns true
// iff the slice was modified.
//
// Presence is decided on the CLASSIFIED path, not the raw string: a ref already
// stored in canonical kb://<own-id>/<path> form and the bare path handed in
// here are the same edge, and a raw comparison would call the canonical one
// absent and append a second spelling of it every time. (Gate.Canonicalize
// would collapse the pair on write, so the corpus stayed correct — but every
// reinforce would report a change and re-commit an identical fact.)
//
// Case-insensitive, matching fact.NewFact's lowercasing, which ClassifyRef
// already applies to fact paths.
func appendRefIfMissing(refs *[]string, ref, localRepoID string) bool {
	want := fact.ClassifyRef(ref, localRepoID)
	for _, existing := range *refs {
		if c := fact.ClassifyRef(existing, localRepoID); c.Kind == want.Kind && c.Path == want.Path {
			return false
		}
	}
	*refs = append(*refs, ref)
	return true
}

// splitTopicPath normalises an agent-supplied topic_path into (topic,
// category) suitable for fact.BuildFactPath. The topic_path may optionally
// be prefixed with the ontology root (e.g. "kb/meta/reasoning"); leading
// and trailing slashes are tolerated. The remainder must split into at
// least two segments — BuildFactPath produces a malformed path if either
// segment is empty, so we reject up front with a clear message.
func splitTopicPath(topicPath, ontologyRoot string) (topic, category string, err error) {
	clean := strings.TrimSpace(topicPath)
	clean = strings.Trim(clean, "/")
	rootPrefix := strings.ToLower(strings.TrimRight(ontologyRoot, "/")) + "/"
	if strings.HasPrefix(strings.ToLower(clean)+"/", rootPrefix) {
		clean = clean[len(rootPrefix)-1:]
		clean = strings.TrimLeft(clean, "/")
	}
	if clean == "" {
		return "", "", fmt.Errorf("topic_path is empty after normalisation")
	}
	parts := strings.SplitN(clean, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("topic_path %q must contain both topic and category (e.g. \"meta/reasoning\")", topicPath)
	}
	return parts[0], parts[1], nil
}
