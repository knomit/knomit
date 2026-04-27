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
	if ontologyRoot == "" {
		return fmt.Errorf("ontology root not configured")
	}
	prefix := strings.ToLower(ontologyRoot) + "/"
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
	return fact.EpistemicType(t).Validate()
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
			updated := fact.SerializeFact(f)
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
		weight := computeWeight(ctx, gs, agentBranch, m.Paths)
		merged := fact.NewFact(mf.Path)
		merged.Title = mf.Title
		merged.Body = mf.Body
		merged.Type = fact.EpistemicType(mf.Type)
		merged.Domain = mf.Domain
		merged.Confidence = mf.Confidence
		merged.Sources = mf.Sources
		merged.Entities = mf.Entities
		merged.Refs = mf.Refs
		merged.EvidenceWeight = weight
		content := fact.SerializeFact(merged)
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
		if fact.EpistemicType(df.Type) == fact.Hypothesis {
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
		f.Type = fact.EpistemicType(df.Type)
		f.Domain = df.Domain
		f.Confidence = df.Confidence
		f.Sources = 1
		f.Entities = df.Entities
		f.Refs = df.Refs
		f.EvidenceWeight = weight
		df.Path = f.Path() // sync df so written slice reflects the canonical (lowercase) path
		content := fact.SerializeFact(f)
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
