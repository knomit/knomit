// Package synthesize — LLM interaction for the distill step: prompt
// construction, response parsing, and per-group LLM calls.
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog/log"
	"knomit/internal/llm"
)

// DistillResult is the LLM JSON response for a distill step.
type DistillResult struct {
	Synthesize []distillFact `json:"synthesize"`
	Forget     []string      `json:"forget"`
}

// distillFact is a synthesized fact returned by the LLM in a distill step.
type distillFact struct {
	Path       string      `json:"path"`
	Title      string      `json:"title"`
	Body       string      `json:"body"`
	Domain     flexStrings `json:"domain"`
	Confidence float64     `json:"confidence"`
	Entities   flexStrings `json:"entities"`
	Refs       flexStrings `json:"refs"`
}

// parseDistillResponse parses the LLM JSON response for a distill step.
func parseDistillResponse(text string) (DistillResult, error) {
	raw := extractJSON(text)
	var result DistillResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return DistillResult{}, fmt.Errorf("parseDistillResponse: %w (raw: %.200s)", err, raw)
	}
	return result, nil
}

// runDistillOnGroup sends one group of facts to the LLM and returns synthesized facts + paths to forget.
func runDistillOnGroup(ctx context.Context, gs GitStore, idx SearchIndex, adapter llm.LLMAdapter, group []factForLLM, step RecipeStep, recipe Recipe, profile Profile, onProgress func(ProgressEvent)) ([]distillFact, []string, error) {
	chunks := chunkFacts(group, profile.MaxChunkBytes)

	var synthesized []distillFact
	var forget []string

	for i, chunk := range chunks {
		log.Debug().Int("chunk", i+1).Int("total", len(chunks)).Int("facts", len(chunk)).Msg("distill: sending to LLM")
		onProgress(ProgressEvent{Phase: "llm", Message: fmt.Sprintf("distill chunk %d/%d (%d facts)", i+1, len(chunks), len(chunk))})

		factsJSON, _ := json.MarshalIndent(chunk, "", "  ")
		data := PromptData{
			Facts:        string(factsJSON),
			RecipePrompt: recipe.Prompt,
			StepPrompt:   step.Prompt,
		}

		systemPrompt, err := RenderTemplate(profile.Name, "distill", "system", data)
		if err != nil {
			return nil, nil, fmt.Errorf("distill: render system: %w", err)
		}
		userPrompt, err := RenderTemplate(profile.Name, "distill", "user", data)
		if err != nil {
			return nil, nil, fmt.Errorf("distill: render user: %w", err)
		}

		// Collect input paths for validation and passive detection.
		inputPaths := make([]string, len(chunk))
		for j, f := range chunk {
			inputPaths[j] = f.File
		}

		opts := llm.CompletionOptions{ForceJSON: profile.ForceJSON}
		response, err := adapter.Complete(ctx, systemPrompt, []llm.Message{{Role: "user", Content: userPrompt}}, opts, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("distill LLM chunk %d: %w", i+1, err)
		}
		result, err := parseDistillResponse(response)
		if err != nil {
			return nil, nil, fmt.Errorf("distill parse chunk %d: %w", i+1, err)
		}

		// Validate forget paths reference actual input facts.
		if verr := validateDistillPaths(result, inputPaths); verr != nil {
			log.Warn().Err(verr).Int("chunk", i+1).Msg("distill: invalid paths in response")
			result = DistillResult{} // treat as passive to trigger retry
		}

		if isDistillPassive(result, inputPaths) && profile.RetryOnPassive {
			log.Debug().Int("chunk", i+1).Msg("distill: passive response, retrying")
			onProgress(ProgressEvent{Phase: "retry", Message: fmt.Sprintf("distill chunk %d (passive, retrying)", i+1)})

			retryPrompt, err := RenderTemplate(profile.Name, "distill", "retry", data)
			if err != nil {
				return nil, nil, fmt.Errorf("distill: render retry: %w", err)
			}
			response, err = adapter.Complete(ctx, systemPrompt, []llm.Message{{Role: "user", Content: retryPrompt}}, opts, nil)
			if err != nil {
				return nil, nil, fmt.Errorf("distill retry chunk %d: %w", i+1, err)
			}
			result, err = parseDistillResponse(response)
			if err != nil {
				return nil, nil, fmt.Errorf("distill retry parse chunk %d: %w", i+1, err)
			}
			// Validate retry paths too.
			if verr := validateDistillPaths(result, inputPaths); verr != nil {
				log.Warn().Err(verr).Int("chunk", i+1).Msg("distill: retry also has invalid paths, discarding")
				result = DistillResult{}
			}
			if isDistillPassive(result, inputPaths) {
				log.Warn().Int("chunk", i+1).Msg("distill: retry also passive, accepting result")
			}
		}

		log.Debug().Int("synthesized", len(result.Synthesize)).Int("forget", len(result.Forget)).Msg("distill: LLM response parsed")
		synthesized = append(synthesized, result.Synthesize...)
		forget = append(forget, result.Forget...)
	}
	return synthesized, forget, nil
}
