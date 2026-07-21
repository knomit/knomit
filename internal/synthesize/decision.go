// Package synthesize — shared decision application logic used by both the
// headless pipeline and the review orchestrator.
package synthesize

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"knomit/internal/fact"
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
}

// ApplyPruneDecisions applies prune decisions (retract/update) and merges to the git store.
// ontologyRoot is the configured fact root (e.g. "kb"); merge outputs whose
// path falls outside this root or whose epistemic type is empty/invalid are
// rejected with a warn rather than written.
func ApplyPruneDecisions(ctx context.Context,
	gs store.FactIndex,
	decisions []PruneDecision,
	merges []MergeEntry,
	recipeName string,
	onProgress func(ProgressEvent),
	agentBranch string,
	ontologyRoot string,
) (*ReviewStats, error) {
	stats := &ReviewStats{}
	// Track deleted paths to avoid double-deletion when a path appears in
	// both "retract" decisions and merge source lists.
	deletedPaths := make(map[string]bool)
	log.Info().Int("decisions", len(decisions)).Int("merges", len(merges)).Msg("prune: applying results")

	// Apply decisions.
	for _, d := range decisions {
		switch d.Action {
		case "keep":
			// no-op
		case "retract":
			msg := fmt.Sprintf("synthesize-%s: retract %s", recipeName, d.Path)
			deletedPaths[d.Path] = true
			if _, err := gs.DeleteFact(ctx, agentBranch, d.Path, msg); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("retract %s: %v", d.Path, err)})
				continue
			}
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
		weight := computeWeight(ctx, gs, agentBranch, m.Paths)
		merged := fact.NewFact(mf.Path)
		merged.Title = mf.Title
		merged.Body = mf.Body
		merged.Type = fact.Type(mf.Type)
		merged.Domain = mf.Domain
		merged.Confidence = mf.Confidence
		merged.Sources = mf.Sources
		merged.Entities = mf.Entities
		merged.Refs = mf.Refs
		merged.EvidenceWeight = weight
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

	return stats, nil
}

// ApplyDistillDecisions applies distill results: writes synthesized facts and retracts subsumed ones.
// Returns stats, the written facts (with normalized paths), and any error.
// ontologyRoot is the configured fact root (e.g. "kb"); synthesized facts
// whose path falls outside this root or whose epistemic type is
// empty/invalid are rejected with a warn rather than written.
func ApplyDistillDecisions(ctx context.Context,
	gs store.FactIndex,
	synthesized []distillFact,
	retract []string,
	recipeName string,
	onProgress func(ProgressEvent),
	agentBranch string,
	ontologyRoot string,
) (*ReviewStats, []distillFact, error) {
	stats := &ReviewStats{}
	var written []distillFact

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
		var localRefs []string
		for _, r := range df.Refs {
			if strings.HasSuffix(r, ".md") {
				localRefs = append(localRefs, r)
			}
		}
		weight := computeWeight(ctx, gs, agentBranch, localRefs)
		f := fact.NewFact(df.Path)
		f.Title = df.Title
		f.Body = df.Body
		f.Type = fact.Type(df.Type)
		f.Domain = df.Domain
		f.Confidence = df.Confidence
		f.Sources = 1
		f.Entities = df.Entities
		f.Refs = df.Refs
		f.EvidenceWeight = weight
		df.Path = f.Path() // sync df so written slice reflects the canonical (lowercase) path
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
			if appendRefIfMissing(&tf.Refs, e.MethodologyPath) {
				serialized, err := fact.SerializeFact(tf)
				if err != nil {
					return fmt.Errorf("reinforce[%d]: serialize transition %q: %w", i, tp, err)
				}
				files[tf.Path()] = serialized
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
		f.Sources = 1
		f.Refs = []string(p.Refs)
		serialized, err := fact.SerializeFact(f)
		if err != nil {
			return fmt.Errorf("propose[%d]: serialize: %w", i, err)
		}
		files[f.Path()] = serialized
		onProgress(ProgressEvent{Phase: "detail-reflect-propose", Message: "wrote methodology " + f.Path()})
	}

	// Phase 3 — single atomic commit covering both arms. If there's
	// nothing to write (empty reinforce + empty propose, or every
	// transition already cited the methodology), this is a no-op.
	if len(files) > 0 {
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

// appendRefIfMissing appends ref to *refs if not already present, treating
// path comparison case-insensitively to match fact.NewFact's lowercasing.
// Returns true iff the slice was modified.
func appendRefIfMissing(refs *[]string, ref string) bool {
	for _, existing := range *refs {
		if strings.EqualFold(existing, ref) {
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
