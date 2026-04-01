// Package synthesize — shared decision application logic used by both the
// headless pipeline and the review orchestrator.
package synthesize

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"knomit/internal/fact"
	"knomit/internal/mcp"
	"knomit/internal/store"
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

// ReviewStats tracks what actions were taken during a review.
type ReviewStats struct {
	Pruned      int
	Merged      int
	Updated     int
	Synthesized int
}

// ApplyPruneDecisions applies prune decisions (retract/update) and merges to the git store.
func ApplyPruneDecisions(ctx context.Context,
	gs GitStore,
	idx SearchIndex,
	decisions []PruneDecision,
	merges []MergeEntry,
	recipeName string,
	onProgress func(ProgressEvent),
	agentBranch string,
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
			if err := idx.Delete(ctx, agentBranch, d.Path); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("index delete %s: %v", d.Path, err)})
			}
		
			onProgress(ProgressEvent{Phase: "detail-retract", Message: "retract " + d.Path})
			stats.Pruned++

		case "update":
			readResult, err := gs.ReadFact(ctx, agentBranch, d.Path, nil)
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update read %s: %v", d.Path, err)})
				continue
			}
			f, err := mcp.ParseFact(d.Path, readResult.Content)
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update parse %s: %v", d.Path, err)})
				continue
			}
			f.Confidence = d.Confidence
			updated := mcp.SerializeFact(f)
			msg := fmt.Sprintf("synthesize-%s: update confidence %s → %.2f", recipeName, d.Path, d.Confidence)
			writeRes, err := gs.WriteFact(ctx, agentBranch, d.Path, updated, msg, "update")
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update write %s: %v", d.Path, err)})
				continue
			}
			if err := idx.Upsert(ctx, agentBranch, writeRes.CommitHash, store.NewFactRecord(f, writeRes.BlobHash)); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("index upsert %s: %v", d.Path, err)})
			}
		
			onProgress(ProgressEvent{Phase: "detail-update", Message: fmt.Sprintf("update %.2f %s", d.Confidence, d.Path)})
			stats.Updated++
		}
	}

	// Apply merges.
	for _, m := range merges {
		mf := m.Merged
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
		content := mcp.SerializeFact(merged)
		msg := fmt.Sprintf("synthesize-%s: merge %s", recipeName, strings.Join(m.Paths, ", "))
		writeRes, err := gs.WriteFact(ctx, agentBranch, merged.Path(), content, msg, "subsume")
		if err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge write %s: %v", merged.Path(), err)})
			continue
		}
		_ = idx.Upsert(ctx, agentBranch, writeRes.CommitHash, store.NewFactRecord(merged, writeRes.BlobHash))

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
			_ = idx.Delete(ctx, agentBranch, src)
			deletedPaths[src] = true
		
		}
		onProgress(ProgressEvent{Phase: "detail-merge", Message: "merge " + merged.Path()})
		stats.Merged++
	}

	return stats, nil
}

// ApplyDistillDecisions applies distill results: writes synthesized facts and retracts subsumed ones.
// Returns stats, the written facts (with normalized paths), and any error.
func ApplyDistillDecisions(ctx context.Context,
	gs GitStore,
	idx SearchIndex,
	synthesized []distillFact,
	retract []string,
	recipeName string,
	onProgress func(ProgressEvent),
	agentBranch string,
) (*ReviewStats, []distillFact, error) {
	stats := &ReviewStats{}
	var written []distillFact

	log.Info().Int("synthesized", len(synthesized)).Int("forgotten", len(retract)).Msg("distill: committing results")

	// Commit synthesized facts.
	for _, df := range synthesized {
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
		content := mcp.SerializeFact(f)
		msg := fmt.Sprintf("synthesize-%s: distill %s", recipeName, f.Path())
		writeRes, err := gs.WriteFact(ctx, agentBranch, f.Path(), content, msg, "subsume")
		if err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill write %s: %v", f.Path(), err)})
			continue
		}
		_ = idx.Upsert(ctx, agentBranch, writeRes.CommitHash, store.NewFactRecord(f, writeRes.BlobHash))

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
		_ = idx.Delete(ctx, agentBranch, path)
	
		onProgress(ProgressEvent{Phase: "detail-distill-retract", Message: "retract " + path})
		stats.Pruned++
	}

	return stats, written, nil
}
