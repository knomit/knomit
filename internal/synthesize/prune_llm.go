// Package synthesize — LLM interaction for the prune step: prompt construction,
// response parsing, and chunking. Shared types (factForLLM, extractJSON,
// chunkFacts) are also used by distill.
package synthesize

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PruneDecision is the LLM's decision for a single fact.
type PruneDecision struct {
	Path       string  `json:"path"`
	Action     string  `json:"action"` // "keep" | "forget" | "update"
	Confidence float64 `json:"confidence,omitempty"`
}

// mergedFact is the embedded merged fact object from the LLM response.
type mergedFact struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Domain     []string `json:"domain"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
	Entities   []string `json:"entities"`
	Refs       []string `json:"refs"`
}

// MergeEntry groups source paths with the merged replacement fact.
type MergeEntry struct {
	Paths  []string   `json:"paths"`
	Merged mergedFact `json:"merged"`
}

// PruneResult is the full JSON response from the LLM prune call.
type PruneResult struct {
	Decisions []PruneDecision `json:"decisions"`
	Merges    []MergeEntry    `json:"merges"`
}

// factForLLM is the subset of fact fields sent to the LLM.
type factForLLM struct {
	File       string   `json:"file"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Domain     []string `json:"domain"`
	Entities   []string `json:"entities"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
}

// buildPrunePrompt builds the LLM prompt for a prune step.
func buildPrunePrompt(facts []factForLLM, recipePrompt, stepPrompt string) string {
	factsJSON, _ := json.MarshalIndent(facts, "", "  ")

	var sb strings.Builder
	sb.WriteString("You are reviewing facts in a knowledge base for staleness, redundancy, and duplication.\n\n")
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
	sb.WriteString("Facts to review:\n")
	sb.Write(factsJSON)
	sb.WriteString(`

For each fact, decide:
- keep: fact is current and valuable
- forget: fact is obsolete, superseded, or no longer true
- update: fact needs confidence adjusted (provide new value)

Also identify facts that say the same thing and should be merged into a single unified fact.

Respond as JSON (no markdown wrapping):
{
  "decisions": [
    { "path": "...", "action": "keep|forget|update", "confidence": 0.X }
  ],
  "merges": [
    {
      "paths": ["file1.md", "file2.md"],
      "merged": {
        "path": "know/...",
        "title": "...",
        "body": "...",
        "domain": [],
        "confidence": 0.X,
        "sources": 2,
        "entities": [],
        "refs": ["file1.md", "file2.md"]
      }
    }
  ]
}`)
	return sb.String()
}

// extractJSON strips optional markdown code fences from LLM output.
func extractJSON(text string) string {
	text = strings.TrimSpace(text)
	// Strip ```json ... ``` or ``` ... ```
	if strings.HasPrefix(text, "```") {
		end := strings.LastIndex(text, "```")
		if end > 3 {
			inner := text[3:end]
			// strip optional "json" language tag
			if idx := strings.IndexByte(inner, '\n'); idx >= 0 {
				inner = inner[idx+1:]
			}
			return strings.TrimSpace(inner)
		}
	}
	return text
}

// parsePruneResponse parses the LLM JSON response for a prune step.
func parsePruneResponse(text string) (PruneResult, error) {
	raw := extractJSON(text)
	var result PruneResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return PruneResult{}, fmt.Errorf("parsePruneResponse: %w (raw: %.200s)", err, raw)
	}
	return result, nil
}

// chunkFacts splits facts into groups where each group's JSON is ≤ maxBytes.
func chunkFacts(facts []factForLLM, maxBytes int) [][]factForLLM {
	var chunks [][]factForLLM
	var current []factForLLM
	currentSize := 0

	for _, f := range facts {
		b, _ := json.Marshal(f)
		size := len(b)
		if currentSize+size > maxBytes && len(current) > 0 {
			chunks = append(chunks, current)
			current = nil
			currentSize = 0
		}
		current = append(current, f)
		currentSize += size
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}
