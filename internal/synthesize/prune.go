package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/store"
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

// gatherAllFacts reads all .md facts from git and returns them as factForLLM slices.
func gatherAllFacts(gs GitStore) ([]factForLLM, error) {
	paths, err := gs.ListAll()
	if err != nil {
		return nil, fmt.Errorf("gatherAllFacts: list: %w", err)
	}

	facts := make([]factForLLM, 0, len(paths))
	for _, path := range paths {
		if !strings.HasSuffix(path, ".md") {
			continue
		}
		content, err := gs.ReadFile(path)
		if err != nil {
			continue // skip unreadable files
		}
		fact, err := mcp.ParseFact(path, content)
		if err != nil {
			continue // skip non-fact files
		}
		facts = append(facts, factForLLM{
			File:       fact.Path,
			Title:      fact.Title,
			Body:       fact.Body,
			Domain:     fact.Domain,
			Entities:   fact.Entities,
			Confidence: fact.Confidence,
			Sources:    fact.Sources,
		})
	}
	return facts, nil
}

// executePruneStep runs one prune step of the synthesis pipeline.
func executePruneStep(ctx context.Context, gs GitStore, idx SearchIndex, adapter llm.LLMAdapter, step RecipeStep, recipe Recipe, onProgress func(ProgressEvent)) error {
	onProgress(ProgressEvent{Phase: "gather", Message: "loading facts for prune"})

	facts, err := gatherAllFacts(gs)
	if err != nil {
		return fmt.Errorf("prune: gather facts: %w", err)
	}
	if len(facts) == 0 {
		onProgress(ProgressEvent{Phase: "prune", Message: "no facts found"})
		return nil
	}

	const maxChunkBytes = 100_000
	chunks := chunkFacts(facts, maxChunkBytes)

	var allDecisions []PruneDecision
	var allMerges []MergeEntry

	for i, chunk := range chunks {
		onProgress(ProgressEvent{Phase: "llm", Message: fmt.Sprintf("prune chunk %d/%d (%d facts)", i+1, len(chunks), len(chunk))})

		prompt := buildPrunePrompt(chunk, recipe.Prompt, step.Prompt)
		response, err := adapter.Complete(
			ctx,
			"You are a knowledge base maintenance assistant. Respond only with valid JSON.",
			[]llm.Message{{Role: "user", Content: prompt}},
			nil,
		)
		if err != nil {
			return fmt.Errorf("prune: LLM call chunk %d: %w", i+1, err)
		}

		result, err := parsePruneResponse(response)
		if err != nil {
			return fmt.Errorf("prune: parse response chunk %d: %w", i+1, err)
		}
		allDecisions = append(allDecisions, result.Decisions...)
		allMerges = append(allMerges, result.Merges...)
	}

	// Track which paths have been deleted to avoid double-deletion.
	deletedPaths := make(map[string]bool)

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
