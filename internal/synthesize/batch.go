package synthesize

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog/log"
	"knomit/internal/llm"
)

// pruneChunkJob tracks one chunk's metadata through the batch pipeline.
type pruneChunkJob struct {
	label      string
	chunk      []factForLLM
	inputPaths []string
	system     string
	user       string
	retryUser  string // rendered lazily
	data       PromptData
}

// pruneBatch submits all prune chunks as a Gemini batch job. It returns
// aggregated decisions and merges (same semantics as the sequential loop).
func pruneBatch(ctx context.Context, ba llm.BatchAdapter, llmGroups [][]factForLLM, recipe Recipe, step RecipeStep, profile Profile, onProgress func(ProgressEvent)) ([]PruneDecision, []MergeEntry, error) {
	// 1. Pre-render all prompts.
	var jobs []pruneChunkJob
	for gi, group := range llmGroups {
		chunks := chunkFacts(group, profile.MaxChunkBytes)
		for ci, chunk := range chunks {
			label := fmt.Sprintf("cluster %d/%d chunk %d/%d (%d facts)", gi+1, len(llmGroups), ci+1, len(chunks), len(chunk))
			factsJSON, _ := json.MarshalIndent(chunk, "", "  ")
			data := PromptData{
				Facts:        string(factsJSON),
				RecipePrompt: recipe.Prompt,
				StepPrompt:   step.Prompt,
			}
			sys, err := RenderTemplate(profile.Name, "prune", "system", data)
			if err != nil {
				return nil, nil, fmt.Errorf("prune batch: render system: %w", err)
			}
			usr, err := RenderTemplate(profile.Name, "prune", "user", data)
			if err != nil {
				return nil, nil, fmt.Errorf("prune batch: render user: %w", err)
			}
			inputPaths := make([]string, len(chunk))
			for j, f := range chunk {
				inputPaths[j] = f.File
			}
			jobs = append(jobs, pruneChunkJob{
				label: label, chunk: chunk, inputPaths: inputPaths,
				system: sys, user: usr, data: data,
			})
		}
	}

	if len(jobs) == 0 {
		return nil, nil, nil
	}

	// 2. Build and submit batch.
	onProgress(ProgressEvent{Phase: "llm", Message: fmt.Sprintf("prune batch: submitting %d chunks", len(jobs))})
	log.Info().Int("chunks", len(jobs)).Msg("prune: submitting batch")

	reqs := make([]llm.BatchRequest, len(jobs))
	for i, j := range jobs {
		reqs[i] = llm.BatchRequest{
			System:   j.system,
			Messages: []llm.Message{{Role: "user", Content: j.user}},
		}
	}
	opts := llm.CompletionOptions{ForceJSON: profile.ForceJSON}
	responses, err := ba.CompleteBatch(ctx, reqs, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("prune batch: %w", err)
	}

	// 3. Parse results, collect passive indices for retry.
	var allDecisions []PruneDecision
	var allMerges []MergeEntry
	var retryIndices []int

	for i, resp := range responses {
		result, err := parsePruneResponse(resp)
		if err != nil {
			return nil, nil, fmt.Errorf("prune batch parse %s: %w", jobs[i].label, err)
		}
		if verr := validatePrunePaths(result, jobs[i].inputPaths); verr != nil {
			log.Warn().Err(verr).Str("label", jobs[i].label).Msg("prune batch: invalid paths")
			result = PruneResult{}
		}
		if isPrunePassive(result) && profile.RetryOnPassive {
			retryIndices = append(retryIndices, i)
			continue
		}
		allDecisions = append(allDecisions, result.Decisions...)
		allMerges = append(allMerges, result.Merges...)
	}

	// 4. Retry passive responses as a second batch.
	if len(retryIndices) > 0 {
		log.Info().Int("retries", len(retryIndices)).Msg("prune: submitting retry batch")
		onProgress(ProgressEvent{Phase: "retry", Message: fmt.Sprintf("prune batch: retrying %d passive chunks", len(retryIndices))})

		retryReqs := make([]llm.BatchRequest, len(retryIndices))
		for ri, idx := range retryIndices {
			retryPrompt, err := RenderTemplate(profile.Name, "prune", "retry", jobs[idx].data)
			if err != nil {
				return nil, nil, fmt.Errorf("prune batch: render retry: %w", err)
			}
			retryReqs[ri] = llm.BatchRequest{
				System:   jobs[idx].system,
				Messages: []llm.Message{{Role: "user", Content: retryPrompt}},
			}
		}
		retryResponses, err := ba.CompleteBatch(ctx, retryReqs, opts)
		if err != nil {
			return nil, nil, fmt.Errorf("prune batch retry: %w", err)
		}
		for ri, resp := range retryResponses {
			idx := retryIndices[ri]
			result, err := parsePruneResponse(resp)
			if err != nil {
				return nil, nil, fmt.Errorf("prune batch retry parse %s: %w", jobs[idx].label, err)
			}
			if verr := validatePrunePaths(result, jobs[idx].inputPaths); verr != nil {
				log.Warn().Err(verr).Str("label", jobs[idx].label).Msg("prune batch: retry invalid paths, discarding")
				continue
			}
			allDecisions = append(allDecisions, result.Decisions...)
			allMerges = append(allMerges, result.Merges...)
		}
	}

	return allDecisions, allMerges, nil
}

// distillChunkJob tracks one distill chunk through the batch pipeline.
type distillChunkJob struct {
	chunk      []factForLLM
	inputPaths []string
	system     string
	user       string
	data       PromptData
}

// distillBatch submits all distill chunks as a Gemini batch job.
func distillBatch(ctx context.Context, ba llm.BatchAdapter, group []factForLLM, step RecipeStep, recipe Recipe, profile Profile, onProgress func(ProgressEvent)) ([]distillFact, []string, error) {
	chunks := chunkFacts(group, profile.MaxChunkBytes)

	var jobs []distillChunkJob
	for _, chunk := range chunks {
		factsJSON, _ := json.MarshalIndent(chunk, "", "  ")
		data := PromptData{
			Facts:        string(factsJSON),
			RecipePrompt: recipe.Prompt,
			StepPrompt:   step.Prompt,
		}
		sys, err := RenderTemplate(profile.Name, "distill", "system", data)
		if err != nil {
			return nil, nil, fmt.Errorf("distill batch: render system: %w", err)
		}
		usr, err := RenderTemplate(profile.Name, "distill", "user", data)
		if err != nil {
			return nil, nil, fmt.Errorf("distill batch: render user: %w", err)
		}
		inputPaths := make([]string, len(chunk))
		for j, f := range chunk {
			inputPaths[j] = f.File
		}
		jobs = append(jobs, distillChunkJob{
			chunk: chunk, inputPaths: inputPaths,
			system: sys, user: usr, data: data,
		})
	}

	if len(jobs) == 0 {
		return nil, nil, nil
	}

	onProgress(ProgressEvent{Phase: "llm", Message: fmt.Sprintf("distill batch: submitting %d chunks", len(jobs))})
	log.Info().Int("chunks", len(jobs)).Msg("distill: submitting batch")

	reqs := make([]llm.BatchRequest, len(jobs))
	for i, j := range jobs {
		reqs[i] = llm.BatchRequest{
			System:   j.system,
			Messages: []llm.Message{{Role: "user", Content: j.user}},
		}
	}
	opts := llm.CompletionOptions{ForceJSON: profile.ForceJSON}
	responses, err := ba.CompleteBatch(ctx, reqs, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("distill batch: %w", err)
	}

	var synthesized []distillFact
	var forget []string
	var retryIndices []int

	for i, resp := range responses {
		result, err := parseDistillResponse(resp)
		if err != nil {
			return nil, nil, fmt.Errorf("distill batch parse chunk %d: %w", i+1, err)
		}
		if verr := validateDistillPaths(result, jobs[i].inputPaths); verr != nil {
			log.Warn().Err(verr).Int("chunk", i+1).Msg("distill batch: invalid paths")
			result = DistillResult{}
		}
		if isDistillPassive(result, jobs[i].inputPaths) && profile.RetryOnPassive {
			retryIndices = append(retryIndices, i)
			continue
		}
		synthesized = append(synthesized, result.Synthesize...)
		forget = append(forget, result.Retract...)
	}

	if len(retryIndices) > 0 {
		log.Info().Int("retries", len(retryIndices)).Msg("distill: submitting retry batch")
		onProgress(ProgressEvent{Phase: "retry", Message: fmt.Sprintf("distill batch: retrying %d passive chunks", len(retryIndices))})

		retryReqs := make([]llm.BatchRequest, len(retryIndices))
		for ri, idx := range retryIndices {
			retryPrompt, err := RenderTemplate(profile.Name, "distill", "retry", jobs[idx].data)
			if err != nil {
				return nil, nil, fmt.Errorf("distill batch: render retry: %w", err)
			}
			retryReqs[ri] = llm.BatchRequest{
				System:   jobs[idx].system,
				Messages: []llm.Message{{Role: "user", Content: retryPrompt}},
			}
		}
		retryResponses, err := ba.CompleteBatch(ctx, retryReqs, opts)
		if err != nil {
			return nil, nil, fmt.Errorf("distill batch retry: %w", err)
		}
		for ri, resp := range retryResponses {
			idx := retryIndices[ri]
			result, err := parseDistillResponse(resp)
			if err != nil {
				return nil, nil, fmt.Errorf("distill batch retry parse chunk %d: %w", idx+1, err)
			}
			if verr := validateDistillPaths(result, jobs[idx].inputPaths); verr != nil {
				log.Warn().Err(verr).Int("chunk", idx+1).Msg("distill batch: retry invalid paths, discarding")
				continue
			}
			synthesized = append(synthesized, result.Synthesize...)
			forget = append(forget, result.Retract...)
		}
	}

	return synthesized, forget, nil
}
