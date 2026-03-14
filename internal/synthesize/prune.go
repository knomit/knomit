// Package synthesize — Prune step of the synthesis pipeline: gathers facts,
// clusters by semantic similarity, sends clusters to an LLM for review, and
// applies keep/forget/update/merge decisions.
package synthesize

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/store"
)

// executePruneStep runs one prune step of the synthesis pipeline.
// When embeddings are available, facts are clustered first and only multi-fact
// clusters are sent to the LLM for review. Without embeddings, all facts are
// sent in byte-sized chunks (legacy behaviour).
func executePruneStep(ctx context.Context, gs GitStore, idx SearchIndex, embedder Embedder, adapter llm.LLMAdapter, step RecipeStep, recipe Recipe, onProgress func(ProgressEvent)) error {
	onProgress(ProgressEvent{Phase: "gather", Message: "loading facts for prune"})

	facts, err := gatherAllFacts(gs)
	if err != nil {
		return fmt.Errorf("prune: gather facts: %w", err)
	}
	log.Debug().Int("facts", len(facts)).Msg("prune: gathered facts")
	if len(facts) == 0 {
		onProgress(ProgressEvent{Phase: "prune", Message: "no facts found"})
		return nil
	}

	// Try to cluster facts using embeddings before sending to LLM.
	groups, err := clusterFactsForPrune(facts, idx, embedder, step, onProgress)
	if err != nil {
		return fmt.Errorf("prune: cluster: %w", err)
	}

	var allDecisions []PruneDecision
	var allMerges []MergeEntry

	// 100KB limit per LLM call to stay within context window limits.
	const maxChunkBytes = 100_000
	for gi, group := range groups {
		chunks := chunkFacts(group, maxChunkBytes)
		for ci, chunk := range chunks {
			label := fmt.Sprintf("cluster %d/%d chunk %d/%d (%d facts)", gi+1, len(groups), ci+1, len(chunks), len(chunk))
			log.Debug().Str("label", label).Msg("prune: sending to LLM")
			onProgress(ProgressEvent{Phase: "llm", Message: fmt.Sprintf("prune %s", label)})

			prompt := buildPrunePrompt(chunk, recipe.Prompt, step.Prompt)
			response, err := adapter.Complete(
				ctx,
				"You are a knowledge base maintenance assistant. Respond only with valid JSON.",
				[]llm.Message{{Role: "user", Content: prompt}},
				nil,
			)
			if err != nil {
				return fmt.Errorf("prune: LLM call %s: %w", label, err)
			}

			result, err := parsePruneResponse(response)
			if err != nil {
				return fmt.Errorf("prune: parse response %s: %w", label, err)
			}

			// Log each decision at debug level.
			for _, d := range result.Decisions {
				log.Debug().Str("path", d.Path).Str("action", d.Action).Float64("confidence", d.Confidence).Msg("prune: decision")
			}
			for _, m := range result.Merges {
				log.Debug().Strs("sources", m.Paths).Str("merged_path", m.Merged.Path).Msg("prune: merge")
			}

			log.Debug().Int("decisions", len(result.Decisions)).Int("merges", len(result.Merges)).Msg("prune: LLM response parsed")
			allDecisions = append(allDecisions, result.Decisions...)
			allMerges = append(allMerges, result.Merges...)
		}
	}

	// Track deleted paths to avoid double-deletion when a path appears in
	// both "forget" decisions and merge source lists.
	deletedPaths := make(map[string]bool)

	log.Info().Int("decisions", len(allDecisions)).Int("merges", len(allMerges)).Msg("prune: applying results")

	// Apply decisions.
	for _, d := range allDecisions {
		switch d.Action {
		case "keep":
			// no-op
		case "forget":
			msg := fmt.Sprintf("synthesize-%s: forget %s", recipe.Name, d.Path)
			deletedPaths[d.Path] = true
			if err := gs.DeleteFile(d.Path, msg); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("forget %s: %v", d.Path, err)})
				continue
			}
			if err := idx.Delete(d.Path); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("index delete %s: %v", d.Path, err)})
			}
			onProgress(ProgressEvent{Phase: "detail-forget", Message: d.Path})

		case "update":
			content, err := gs.ReadFile(d.Path)
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update read %s: %v", d.Path, err)})
				continue
			}
			fact, err := mcp.ParseFact(d.Path, content)
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update parse %s: %v", d.Path, err)})
				continue
			}
			fact.Confidence = d.Confidence
			updated := mcp.SerializeFact(fact)
			msg := fmt.Sprintf("synthesize-%s: update confidence %s → %.2f", recipe.Name, d.Path, d.Confidence)
			if err := gs.WriteFile(d.Path, updated, msg); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update write %s: %v", d.Path, err)})
				continue
			}
			head, _ := gs.HeadCommit()
			if err := idx.Upsert(store.FactRecord{
				Path:       fact.Path,
				Title:      fact.Title,
				Body:       fact.Body,
				Domain:     fact.Domain,
				Entities:   fact.Entities,
				Confidence: fact.Confidence,
				Sources:    fact.Sources,
				Refs:       fact.Refs,
				CommitHash: head,
			}); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("index upsert %s: %v", d.Path, err)})
			}
			onProgress(ProgressEvent{Phase: "detail-update", Message: d.Path})
		}
	}

	// Apply merges.
	for _, m := range allMerges {
		mf := m.Merged
		merged := mcp.Fact{
			Path:       mf.Path,
			Title:      mf.Title,
			Body:       mf.Body,
			Domain:     mf.Domain,
			Confidence: mf.Confidence,
			Sources:    mf.Sources,
			Entities:   mf.Entities,
			Refs:       mf.Refs,
		}
		content := mcp.SerializeFact(merged)
		msg := fmt.Sprintf("synthesize-%s: merge %s", recipe.Name, strings.Join(m.Paths, ", "))
		if err := gs.WriteFile(mf.Path, content, msg); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge write %s: %v", mf.Path, err)})
			continue
		}
		head, _ := gs.HeadCommit()
		_ = idx.Upsert(store.FactRecord{
			Path:       mf.Path,
			Title:      mf.Title,
			Body:       mf.Body,
			Domain:     mf.Domain,
			Entities:   mf.Entities,
			Confidence: mf.Confidence,
			Sources:    mf.Sources,
			Refs:       mf.Refs,
			CommitHash: head,
		})
		if err := idx.GraphAddDerivedFrom(mf.Path, m.Paths); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("derived_from %s: %v", mf.Path, err)})
		}

		// Delete source facts (unless already forgotten).
		for _, src := range m.Paths {
			if deletedPaths[src] {
				continue
			}
			srcMsg := fmt.Sprintf("synthesize-%s: subsumed by %s", recipe.Name, mf.Path)
			if err := gs.DeleteFile(src, srcMsg); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge delete source %s: %v", src, err)})
				continue
			}
			_ = idx.Delete(src)
			deletedPaths[src] = true
		}
		onProgress(ProgressEvent{Phase: "detail-merge", Message: mf.Path})
	}

	tagName := fmt.Sprintf("learn/synthesize-%s-prune", recipe.Name)
	if err := gs.Tag(tagName); err != nil {
		onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("tag %s: %v", tagName, err)})
	}

	onProgress(ProgressEvent{Phase: "prune-done", Message: fmt.Sprintf("%d decisions, %d merges", len(allDecisions), len(allMerges))})
	return nil
}
