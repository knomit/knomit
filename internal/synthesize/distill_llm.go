// Package synthesize — LLM interaction for the distill step: prompt
// construction, response parsing, and per-group LLM calls.
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Domain     []string `json:"domain"`
	Confidence float64  `json:"confidence"`
	Entities   []string `json:"entities"`
	Refs       []string `json:"refs"`
}

// buildDistillPrompt builds the LLM prompt for a distill step.
func buildDistillPrompt(facts []factForLLM, recipePrompt, stepPrompt string) string {
	factsJSON, _ := json.MarshalIndent(facts, "", "  ")

	var sb strings.Builder
	sb.WriteString("You are synthesizing facts in a knowledge base to find patterns and higher-order insights.\n\n")
	if recipePrompt != "" {
		sb.WriteString("Context: ")
		sb.WriteString(recipePrompt)
		sb.WriteString("\n")
	}
	if stepPrompt != "" {
		sb.WriteString("Instructions: ")
		sb.WriteString(stepPrompt)
		sb.WriteString("\n")
	}
	sb.WriteString("Facts in scope:\n")
	sb.Write(factsJSON)
	sb.WriteString(`

Identify patterns across these facts. Produce:
1. New higher-order facts that capture patterns
2. Which original facts are fully subsumed and can be forgotten

Respond as JSON (no markdown wrapping):
{
  "synthesize": [
    {
      "path": "know/...",
      "title": "...",
      "body": "...",
      "domain": [],
      "confidence": 0.X,
      "entities": [],
      "refs": ["source-file1.md", "source-file2.md"]
    }
  ],
  "forget": ["file1.md", "file2.md"]
}`)
	return sb.String()
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
func runDistillOnGroup(ctx context.Context, gs GitStore, idx SearchIndex, adapter llm.LLMAdapter, group []factForLLM, step RecipeStep, recipe Recipe, onProgress func(ProgressEvent)) ([]distillFact, []string, error) {
	const maxChunkBytes = 100_000
	chunks := chunkFacts(group, maxChunkBytes)

	var synthesized []distillFact
	var forget []string

	for i, chunk := range chunks {
		log.Debug().Int("chunk", i+1).Int("total", len(chunks)).Int("facts", len(chunk)).Msg("distill: sending to LLM")
		onProgress(ProgressEvent{Phase: "llm", Message: fmt.Sprintf("distill chunk %d/%d (%d facts)", i+1, len(chunks), len(chunk))})
		prompt := buildDistillPrompt(chunk, recipe.Prompt, step.Prompt)
		response, err := adapter.Complete(
			ctx,
			"You are a knowledge base synthesis assistant. Respond only with valid JSON.",
			[]llm.Message{{Role: "user", Content: prompt}},
			llm.CompletionOptions{},
			nil,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("distill LLM chunk %d: %w", i+1, err)
		}
		result, err := parseDistillResponse(response)
		if err != nil {
			return nil, nil, fmt.Errorf("distill parse chunk %d: %w", i+1, err)
		}
		log.Debug().Int("synthesized", len(result.Synthesize)).Int("forget", len(result.Forget)).Msg("distill: LLM response parsed")
		synthesized = append(synthesized, result.Synthesize...)
		forget = append(forget, result.Forget...)
	}
	return synthesized, forget, nil
}
