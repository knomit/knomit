// Package synthesize — Prune step of the synthesis pipeline: gathers facts,
// clusters by semantic similarity, sends clusters to an LLM for review, and
// applies keep/forget/update/merge decisions.
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog/log"
	"knomit/internal/llm"
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
	_, err := ApplyPruneDecisions(gs, idx, allDecisions, allMerges, recipe.Name, onProgress)
	if err != nil {
		return err
	}
	onProgress(ProgressEvent{Phase: "prune-done", Message: fmt.Sprintf("%d decisions, %d merges", len(allDecisions), len(allMerges))})
	return nil
}
