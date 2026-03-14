// Package synthesize — shared decision application logic used by both the
// headless pipeline and the review orchestrator.
package synthesize

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"knomit/internal/fact"
	"knomit/internal/mcp"
	"knomit/internal/store"
)

// ReviewStats tracks what actions were taken during a review.
type ReviewStats struct {
	Pruned      int
	Merged      int
	Updated     int
	Synthesized int
}

// ApplyPruneDecisions applies prune decisions (retract/update) and merges to the git store.
func ApplyPruneDecisions(
	gs GitStore,
	idx SearchIndex,
	decisions []PruneDecision,
	merges []MergeEntry,
	recipeName string,
	onProgress func(ProgressEvent),
) (*ReviewStats, error) {
	stats := &ReviewStats{}
	// Track deleted paths to avoid double-deletion when a path appears in
	// both "retract" decisions and merge source lists.
	deletedPaths := make(map[string]bool)
	tagCounter := 0

	log.Info().Int("decisions", len(decisions)).Int("merges", len(merges)).Msg("prune: applying results")

	// Apply decisions.
	for _, d := range decisions {
		switch d.Action {
		case "keep":
			// no-op
		case "retract":
			msg := fmt.Sprintf("synthesize-%s: retract %s", recipeName, d.Path)
			deletedPaths[d.Path] = true
			if _, err := gs.DeleteFile(d.Path, msg); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("retract %s: %v", d.Path, err)})
				continue
			}
			if err := idx.Delete(d.Path); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("index delete %s: %v", d.Path, err)})
			}
			tagOp(gs, "retract", recipeName, &tagCounter)
			onProgress(ProgressEvent{Phase: "detail-retract", Message: "retract " + d.Path})
			stats.Pruned++

		case "update":
			content, err := gs.ReadFile(d.Path)
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update read %s: %v", d.Path, err)})
				continue
			}
			f, err := mcp.ParseFact(d.Path, content)
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update parse %s: %v", d.Path, err)})
				continue
			}
			f.Confidence = d.Confidence
			updated := mcp.SerializeFact(f)
			msg := fmt.Sprintf("synthesize-%s: update confidence %s → %.2f", recipeName, d.Path, d.Confidence)
			commitHash, blobHash, err := gs.WriteFile(d.Path, updated, msg)
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update write %s: %v", d.Path, err)})
				continue
			}
			if err := idx.Upsert(store.FactRecord{
				Path:       f.Path,
				Title:      f.Title,
				BlobHash:   blobHash,
				Type:       string(f.Type),
				Domain:     f.Domain,
				Entities:   f.Entities,
				Confidence: f.Confidence,
				Sources:    f.Sources,
				Refs:       f.Refs,
				CommitHash: commitHash,
			}); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("index upsert %s: %v", d.Path, err)})
			}
			tagOp(gs, "update", recipeName, &tagCounter)
			onProgress(ProgressEvent{Phase: "detail-update", Message: fmt.Sprintf("update %.2f %s", d.Confidence, d.Path)})
			stats.Updated++
		}
	}

	// Apply merges: winner gets update tag, losers get retract tag.
	for _, m := range merges {
		mf := m.Merged
		merged := mcp.Fact{
			Path:       mf.Path,
			Title:      mf.Title,
			Body:       mf.Body,
			Type:       fact.EpistemicType(mf.Type),
			Domain:     mf.Domain,
			Confidence: mf.Confidence,
			Sources:    mf.Sources,
			Entities:   mf.Entities,
			Refs:       mf.Refs,
		}
		content := mcp.SerializeFact(merged)
		msg := fmt.Sprintf("synthesize-%s: merge %s", recipeName, strings.Join(m.Paths, ", "))
		commitHash, blobHash, err := gs.WriteFile(mf.Path, content, msg)
		if err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge write %s: %v", mf.Path, err)})
			continue
		}
		_ = idx.Upsert(store.FactRecord{
			Path:       mf.Path,
			Title:      mf.Title,
			BlobHash:   blobHash,
			Type:       mf.Type,
			Domain:     mf.Domain,
			Entities:   mf.Entities,
			Confidence: mf.Confidence,
			Sources:    mf.Sources,
			Refs:       mf.Refs,
			CommitHash: commitHash,
		})
		if err := idx.GraphAddDerivedFrom(mf.Path, m.Paths); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("derived_from %s: %v", mf.Path, err)})
		}
		tagOp(gs, "update", recipeName, &tagCounter)

		// Delete source facts (losers get retract tag).
		for _, src := range m.Paths {
			if deletedPaths[src] {
				continue
			}
			srcMsg := fmt.Sprintf("synthesize-%s: subsumed by %s", recipeName, mf.Path)
			if _, err := gs.DeleteFile(src, srcMsg); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge delete source %s: %v", src, err)})
				continue
			}
			_ = idx.Delete(src)
			deletedPaths[src] = true
			tagOp(gs, "retract", recipeName, &tagCounter)
		}
		onProgress(ProgressEvent{Phase: "detail-merge", Message: "merge " + mf.Path})
		stats.Merged++
	}

	return stats, nil
}

// ApplyDistillDecisions applies distill results: writes synthesized facts and retracts subsumed ones.
func ApplyDistillDecisions(
	gs GitStore,
	idx SearchIndex,
	synthesized []distillFact,
	retract []string,
	recipeName string,
	onProgress func(ProgressEvent),
) (*ReviewStats, error) {
	stats := &ReviewStats{}
	tagCounter := 0

	log.Info().Int("synthesized", len(synthesized)).Int("forgotten", len(retract)).Msg("distill: committing results")

	// Commit synthesized facts.
	for _, df := range synthesized {
		f := mcp.Fact{
			Path:       df.Path,
			Title:      df.Title,
			Body:       df.Body,
			Type:       fact.EpistemicType(df.Type),
			Domain:     df.Domain,
			Confidence: df.Confidence,
			Sources:    1,
			Entities:   df.Entities,
			Refs:       df.Refs,
		}
		content := mcp.SerializeFact(f)
		msg := fmt.Sprintf("synthesize-%s: distill %s", recipeName, df.Path)
		commitHash, blobHash, err := gs.WriteFile(df.Path, content, msg)
		if err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill write %s: %v", df.Path, err)})
			continue
		}
		_ = idx.Upsert(store.FactRecord{
			Path:       df.Path,
			Title:      df.Title,
			BlobHash:   blobHash,
			Type:       df.Type,
			Domain:     df.Domain,
			Entities:   df.Entities,
			Confidence: df.Confidence,
			Sources:    1,
			Refs:       df.Refs,
			CommitHash: commitHash,
		})
		if len(df.Refs) > 0 {
			if err := idx.GraphAddDerivedFrom(df.Path, df.Refs); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("derived_from %s: %v", df.Path, err)})
			}
		}
		tagOp(gs, "subsume", recipeName, &tagCounter)
		onProgress(ProgressEvent{Phase: "detail-learn", Message: "learn " + df.Path})
		stats.Synthesized++
	}

	// Delete subsumed facts.
	for _, path := range retract {
		msg := fmt.Sprintf("synthesize-%s: subsumed by distilled fact", recipeName)
		if _, err := gs.DeleteFile(path, msg); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill retract %s: %v", path, err)})
			continue
		}
		_ = idx.Delete(path)
		tagOp(gs, "retract", recipeName, &tagCounter)
		onProgress(ProgressEvent{Phase: "detail-distill-retract", Message: "retract " + path})
		stats.Pruned++
	}

	return stats, nil
}
