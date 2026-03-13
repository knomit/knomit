// Package synthesize — Prune step of the synthesis pipeline: gathers facts,
// clusters by semantic similarity, sends clusters to an LLM for review, and
// applies keep/forget/update/merge decisions.
package synthesize

import (
	"context"
	"encoding/json"
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
func executePruneStep(ctx context.Context, gs GitStore, idx SearchIndex, embedder Embedder, adapter llm.LLMAdapter, step RecipeStep, recipe Recipe, profile Profile, onProgress func(ProgressEvent)) error {
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

	// Dedup pass: merge near-duplicates within each cluster before LLM review.
	threshold := step.DedupThreshold
	if threshold <= 0 {
		threshold = defaultDedupThreshold
	}
	for gi := range groups {
		surviving, err := dedupCluster(ctx, groups[gi], gs, idx, threshold, recipe.Name, onProgress)
		if err != nil {
			return fmt.Errorf("prune: dedup cluster %d: %w", gi, err)
		}
		groups[gi] = surviving
	}

	// Filter out clusters that shrank to ≤1 fact (nothing for LLM to reason about).
	var llmGroups [][]factForLLM
	for _, g := range groups {
		if len(g) > 1 {
			llmGroups = append(llmGroups, g)
		}
	}
	if len(llmGroups) == 0 {
		onProgress(ProgressEvent{Phase: "prune-done", Message: "all clusters resolved by dedup"})
		return nil
	}

	// Try batch path if adapter supports it.
	if ba, ok := adapter.(llm.BatchAdapter); ok && ba.BatchEnabled() {
		decisions, merges, err := pruneBatch(ctx, ba, llmGroups, recipe, step, profile, onProgress)
		if err != nil {
			return fmt.Errorf("prune batch: %w", err)
		}
		return applyPruneResults(gs, idx, recipe, decisions, merges, onProgress)
	}

	var allDecisions []PruneDecision
	var allMerges []MergeEntry

	for gi, group := range llmGroups {
		chunks := chunkFacts(group, profile.MaxChunkBytes)
		for ci, chunk := range chunks {
			label := fmt.Sprintf("cluster %d/%d chunk %d/%d (%d facts)", gi+1, len(llmGroups), ci+1, len(chunks), len(chunk))
			log.Debug().Str("label", label).Msg("prune: sending to LLM")
			onProgress(ProgressEvent{Phase: "llm", Message: fmt.Sprintf("prune %s", label)})

			factsJSON, _ := json.MarshalIndent(chunk, "", "  ")
			data := PromptData{
				Facts:        string(factsJSON),
				RecipePrompt: recipe.Prompt,
				StepPrompt:   step.Prompt,
			}

			systemPrompt, err := RenderTemplate(profile.Name, "prune", "system", data)
			if err != nil {
				return fmt.Errorf("prune: render system: %w", err)
			}
			userPrompt, err := RenderTemplate(profile.Name, "prune", "user", data)
			if err != nil {
				return fmt.Errorf("prune: render user: %w", err)
			}

			// Collect input paths for validation.
			inputPaths := make([]string, len(chunk))
			for j, f := range chunk {
				inputPaths[j] = f.File
			}

			opts := llm.CompletionOptions{ForceJSON: profile.ForceJSON}
			response, err := adapter.Complete(ctx, systemPrompt, []llm.Message{{Role: "user", Content: userPrompt}}, opts, nil)
			if err != nil {
				return fmt.Errorf("prune: LLM call %s: %w", label, err)
			}

			result, err := parsePruneResponse(response)
			if err != nil {
				return fmt.Errorf("prune: parse response %s: %w", label, err)
			}

			// Validate paths reference actual input facts.
			if verr := validatePrunePaths(result, inputPaths); verr != nil {
				log.Warn().Err(verr).Str("label", label).Msg("prune: invalid paths in response")
				result = PruneResult{} // treat as passive to trigger retry
			}

			// Retry if passive and profile says to retry
			if isPrunePassive(result) && profile.RetryOnPassive {
				log.Debug().Str("label", label).Msg("prune: passive response, retrying")
				onProgress(ProgressEvent{Phase: "retry", Message: fmt.Sprintf("prune %s (passive, retrying)", label)})

				retryPrompt, err := RenderTemplate(profile.Name, "prune", "retry", data)
				if err != nil {
					return fmt.Errorf("prune: render retry: %w", err)
				}
				response, err = adapter.Complete(ctx, systemPrompt, []llm.Message{{Role: "user", Content: retryPrompt}}, opts, nil)
				if err != nil {
					return fmt.Errorf("prune: retry LLM call %s: %w", label, err)
				}
				result, err = parsePruneResponse(response)
				if err != nil {
					return fmt.Errorf("prune: retry parse %s: %w", label, err)
				}
				// Validate retry paths too.
				if verr := validatePrunePaths(result, inputPaths); verr != nil {
					log.Warn().Err(verr).Str("label", label).Msg("prune: retry also has invalid paths, discarding")
					result = PruneResult{}
				}
				if isPrunePassive(result) {
					log.Warn().Str("label", label).Msg("prune: retry also passive, accepting result")
				}
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

	return applyPruneResults(gs, idx, recipe, allDecisions, allMerges, onProgress)
}

// tagOp creates a tag for a synthesize operation, using a counter to avoid collisions.
func tagOp(gs GitStore, prefix, recipeName string, counter *int) {
	*counter++
	tagName := fmt.Sprintf("%s/synthesize-%s-%d", prefix, recipeName, *counter)
	_ = gs.Tag(tagName)
}

// applyPruneResults writes keep/retract/update decisions and merges to git+index.
func applyPruneResults(gs GitStore, idx SearchIndex, recipe Recipe, allDecisions []PruneDecision, allMerges []MergeEntry, onProgress func(ProgressEvent)) error {
	// Track deleted paths to avoid double-deletion when a path appears in
	// both "retract" decisions and merge source lists.
	deletedPaths := make(map[string]bool)
	tagCounter := 0

	log.Info().Int("decisions", len(allDecisions)).Int("merges", len(allMerges)).Msg("prune: applying results")

	// Apply decisions.
	for _, d := range allDecisions {
		switch d.Action {
		case "keep":
			// no-op
		case "retract":
			msg := fmt.Sprintf("synthesize-%s: retract %s", recipe.Name, d.Path)
			deletedPaths[d.Path] = true
			if _, err := gs.DeleteFile(d.Path, msg); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("retract %s: %v", d.Path, err)})
				continue
			}
			if err := idx.Delete(d.Path); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("index delete %s: %v", d.Path, err)})
			}
			tagOp(gs, "retract", recipe.Name, &tagCounter)
			onProgress(ProgressEvent{Phase: "detail-retract", Message: "retract " + d.Path})

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
			commitHash, blobHash, err := gs.WriteFile(d.Path, updated, msg)
			if err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("update write %s: %v", d.Path, err)})
				continue
			}
			if err := idx.Upsert(store.FactRecord{
				Path:       fact.Path,
				Title:      fact.Title,
				BlobHash:   blobHash,
				Domain:     fact.Domain,
				Entities:   fact.Entities,
				Confidence: fact.Confidence,
				Sources:    fact.Sources,
				Refs:       fact.Refs,
				CommitHash: commitHash,
			}); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("index upsert %s: %v", d.Path, err)})
			}
			tagOp(gs, "update", recipe.Name, &tagCounter)
			onProgress(ProgressEvent{Phase: "detail-update", Message: fmt.Sprintf("update %.2f %s", d.Confidence, d.Path)})
		}
	}

	// Apply merges: winner gets update tag, losers get retract tag.
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
		commitHash, blobHash, err := gs.WriteFile(mf.Path, content, msg)
		if err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge write %s: %v", mf.Path, err)})
			continue
		}
		_ = idx.Upsert(store.FactRecord{
			Path:       mf.Path,
			Title:      mf.Title,
			BlobHash:   blobHash,
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
		tagOp(gs, "update", recipe.Name, &tagCounter)

		// Delete source facts (losers get retract tag).
		for _, src := range m.Paths {
			if deletedPaths[src] {
				continue
			}
			srcMsg := fmt.Sprintf("synthesize-%s: subsumed by %s", recipe.Name, mf.Path)
			if _, err := gs.DeleteFile(src, srcMsg); err != nil {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("merge delete source %s: %v", src, err)})
				continue
			}
			_ = idx.Delete(src)
			deletedPaths[src] = true
			tagOp(gs, "retract", recipe.Name, &tagCounter)
		}
		onProgress(ProgressEvent{Phase: "detail-merge", Message: "merge " + mf.Path})
	}

	onProgress(ProgressEvent{Phase: "prune-done", Message: fmt.Sprintf("%d decisions, %d merges", len(allDecisions), len(allMerges))})
	return nil
}
