package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/llm"
)

// generatedContent is the LLM's contribution to a fact: everything corpusgen
// itself does NOT decide (structural fields — topic, category, kind/type,
// confidence, sources — are pre-assigned in factSlot; see diversity.go).
// Refs is populated only in real content-source mode (the model's own
// reported real citation URLs); synthetic mode leaves it empty since refs
// there come from factSlot.SharedRefURL instead (see factbuilder.go).
type generatedContent struct {
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Domain   []string `json:"domain"`
	Entities []string `json:"entities"`
	Refs     []string `json:"refs"`
}

// generateBatch asks adapter for content for every slot in a single call,
// returning results in the same order. Batches (rather than one call per
// fact) both keep wall-clock time down and let facts generated together
// share entities/refs/keywords on purpose. contentSource selects "synthetic"
// (invented content, the original design) or "real" (web-search-grounded).
func generateBatch(ctx context.Context, adapter llm.LLMAdapter, o *fact.Ontology, slots []factSlot, contentSource string) ([]generatedContent, error) {
	var system, user string
	if contentSource == "real" {
		system = buildRealSystemPrompt(o)
		user = buildRealUserPrompt(slots)
	} else {
		system = buildSystemPrompt(o)
		user = buildUserPrompt(slots)
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		msg := user
		if attempt > 0 {
			msg += "\n\nREMINDER: reply with ONLY a JSON array, no prose, no markdown code fences, exactly " +
				fmt.Sprintf("%d", len(slots)) + " objects in the given order."
		}
		raw, err := adapter.Complete(ctx, system, []llm.Message{{Role: "user", Content: msg}}, llm.CompletionOptions{ForceJSON: true}, nil)
		if err != nil {
			lastErr = fmt.Errorf("llm call: %w", err)
			continue
		}
		var out []generatedContent
		if err := json.Unmarshal([]byte(extractJSONArray(raw)), &out); err != nil {
			lastErr = fmt.Errorf("parse LLM response as JSON array: %w", err)
			continue
		}
		if len(out) != len(slots) {
			lastErr = fmt.Errorf("LLM returned %d facts, expected %d", len(out), len(slots))
			continue
		}
		return out, nil
	}
	return nil, lastErr
}

// buildRealSystemPrompt is the web-search-grounded counterpart to
// buildSystemPrompt: instead of asking the model to invent plausible
// content, it requires genuine WebSearch-sourced information with a real,
// checkable citation per fact. WebFetch is not authorized non-interactively
// (confirmed empirically), so the model can only work from search-result
// snippets, not full page reads — the prompt is scoped accordingly.
func buildRealSystemPrompt(o *fact.Ontology) string {
	var b strings.Builder
	b.WriteString("You are researching real, verifiable facts for a knowledge-base, using your " +
		"web search tool. Every fact you write MUST be grounded in something you actually found " +
		"via search — never invent or extrapolate beyond what your search results actually say.\n\n")
	b.WriteString("Ontology (topic/category tree these facts are filed under):\n")
	b.WriteString(describeOntology(o))
	b.WriteString("\nFor each requested fact: search for real, specific, genuinely current or " +
		"well-documented information relevant to its topic/category, then write 2-5 sentences of " +
		"markdown prose (no heading) accurately summarizing what you found. You do not need a " +
		"separate search per fact — search as needed and cover multiple distinct facts from what " +
		"you find, as long as each individual fact is genuinely grounded in a real source. You only " +
		"have search-result snippets, not full page contents, so don't claim more specific detail " +
		"than a snippet could actually support.\n\n")
	b.WriteString("CRITICAL: the \"refs\" field must contain ONLY a real URL you actually saw in " +
		"your search results for that specific fact. Never fabricate, guess, or reconstruct a URL. " +
		"If you cannot find genuine, verifiable information for a requested fact, still return an " +
		"entry but leave \"refs\" empty rather than inventing a citation — an empty refs list is " +
		"fine and expected sometimes; a fake URL is not.\n\n")
	b.WriteString("Reply with ONLY a JSON array (no prose, no markdown fences), one object per requested " +
		"fact IN ORDER, each shaped exactly as:\n" +
		`{"title": "...", "body": "...", "domain": ["..."], "entities": ["..."], "refs": ["..."]}` + "\n" +
		"domain: 1-3 lowercase kebab-case tags. entities: 0-4 proper-noun-ish strings (people/orgs/products " +
		"named in the fact). refs: 0-2 real URLs, empty list if none were genuinely found.\n")
	return b.String()
}

func buildRealUserPrompt(slots []factSlot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Research and write %d facts. Requirements per fact (0-indexed, in this order):\n\n", len(slots))
	for _, s := range slots {
		fmt.Fprintf(&b, "%d. topic=%s/%s, type=%s, confidence≈%.2f (hedge the body's language accordingly, and note this should reflect how well-supported the actual source material is)",
			s.Index, s.Topic, s.Category, s.Type, s.Confidence)
		if s.SharedKeyword != "" {
			fmt.Fprintf(&b, ", if genuinely relevant weave in the concept %q — but only if it's actually true of what you found, don't force it", s.SharedKeyword)
		}
		if s.ResearchHint != "" {
			fmt.Fprintf(&b, ", RESEARCH ANGLE: search for %s, then write this fact about ONE specific angle of that same real event/story (other facts in this batch may cover different angles of the same story — that's intentional)", s.ResearchHint)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func buildSystemPrompt(o *fact.Ontology) string {
	var b strings.Builder
	b.WriteString("You are generating realistic, substantively true-to-form knowledge-base " +
		"entries for a software project's fact store. Each entry is a short, plausible " +
		"fact an engineer or AI agent might record while working in this codebase.\n\n")
	b.WriteString("Ontology (topic/category tree these facts are filed under):\n")
	b.WriteString(describeOntology(o))
	b.WriteString("\nFor each requested fact, invent SPECIFIC, concrete, plausible content: real-sounding " +
		"file paths, function names, thresholds, error messages, etc. Do not write generic filler like " +
		"\"this component handles X.\" Vary sentence structure and length across facts. Body should be " +
		"2-5 sentences of markdown prose (no heading — the title is separate).\n\n")
	b.WriteString("Reply with ONLY a JSON array (no prose, no markdown fences), one object per requested " +
		"fact IN ORDER, each shaped exactly as:\n" +
		`{"title": "...", "body": "...", "domain": ["..."], "entities": ["..."]}` + "\n" +
		"domain: 1-3 lowercase kebab-case tags. entities: 0-4 proper-noun-ish strings (function/type/tool names).\n")
	return b.String()
}

func buildUserPrompt(slots []factSlot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Generate %d facts. Requirements per fact (0-indexed, in this order):\n\n", len(slots))
	for _, s := range slots {
		fmt.Fprintf(&b, "%d. topic=%s/%s, type=%s, confidence≈%.2f (hedge the body's language accordingly)",
			s.Index, s.Topic, s.Category, s.Type, s.Confidence)
		if s.SharedKeyword != "" {
			fmt.Fprintf(&b, ", MUST naturally mention the concept %q somewhere in the body", s.SharedKeyword)
		}
		if s.SharedRefURL != "" {
			fmt.Fprintf(&b, ", body should read as if reporting on the same underlying event as other facts citing %s (don't invent a different URL, just write as an independent source covering the same thing)", s.SharedRefURL)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// extractJSONArray strips a leading/trailing markdown code fence if present,
// since some models wrap JSON output in ```json ... ``` even when asked not
// to. Returns s unchanged if no fence is found.
func extractJSONArray(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
