package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
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
			// Log a snippet to stderr (not folded into the returned error) so a
			// parse failure deep into a long unattended run is diagnosable
			// instead of just an opaque "invalid character" message.
			snippet := raw
			if len(snippet) > 500 {
				snippet = snippet[:500] + "...(truncated)"
			}
			fmt.Fprintf(os.Stderr, "  [attempt %d/%d] %v\n  raw response: %s\n", attempt+1, maxAttempts, err, snippet)
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
		"named in the fact). refs: 0-2 real URLs, empty list if none were genuinely found.\n" +
		jsonQuoteWarning)
	return b.String()
}

func buildRealUserPrompt(slots []factSlot) string {
	peers := keywordGroupPeers(slots)
	var b strings.Builder
	fmt.Fprintf(&b, "Research and write %d facts. Requirements per fact (0-indexed, in this order):\n\n", len(slots))
	for _, s := range slots {
		fmt.Fprintf(&b, "%d. topic=%s/%s, type=%s, confidence≈%.2f (hedge the body's language accordingly, and note this should reflect how well-supported the actual source material is)",
			s.Index, s.Topic, s.Category, s.Type, s.Confidence)
		if p, ok := peers[s.Index]; ok {
			fmt.Fprintf(&b, "%s", keywordGroupInstruction(p))
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
		"domain: 1-3 lowercase kebab-case tags. entities: 0-4 proper-noun-ish strings (function/type/tool names).\n" +
		jsonQuoteWarning)
	return b.String()
}

// jsonQuoteWarning heads off a real, recurring failure mode: a batch's whole
// JSON array fails to parse whenever any fact's title/body contains a
// literal, unescaped double-quote (e.g. quoting an article title, a nicknamed
// product, or a direct quote from a source) — natural in real content, since
// real news/technical writing quotes things constantly. Found by logging the
// raw response on a parse failure rather than guessing (see generateBatch's
// diagnostic logging) — the response was well-formed JSON syntax except for
// exactly this. Steering the model away from the risky character entirely is
// more reliable than trusting it to escape every quote correctly across a
// whole batch's worth of generated prose.
const jsonQuoteWarning = "IMPORTANT: never put a literal double-quote (\") character inside any string " +
	"value (title, body, domain, entities, refs) — a single unescaped one breaks the entire batch. If you " +
	"need to quote something within the text (an article title, a product nickname, a direct quote), use " +
	"single quotes ' instead.\n"

func buildUserPrompt(slots []factSlot) string {
	peers := keywordGroupPeers(slots)
	var b strings.Builder
	fmt.Fprintf(&b, "Generate %d facts. Requirements per fact (0-indexed, in this order):\n\n", len(slots))
	for _, s := range slots {
		fmt.Fprintf(&b, "%d. topic=%s/%s, type=%s, confidence≈%.2f (hedge the body's language accordingly)",
			s.Index, s.Topic, s.Category, s.Type, s.Confidence)
		if p, ok := peers[s.Index]; ok {
			fmt.Fprintf(&b, "%s", keywordGroupInstruction(p))
		}
		if s.SharedRefURL != "" {
			fmt.Fprintf(&b, ", body should read as if reporting on the same underlying event as other facts citing %s (don't invent a different URL, just write as an independent source covering the same thing)", s.SharedRefURL)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// keywordGroupPeers maps each grouped slot's Index to the Index list of its
// OTHER group members (same KeywordGroupID, excluding itself). Slots with no
// group (KeywordGroupID == 0) are absent from the result.
func keywordGroupPeers(slots []factSlot) map[int][]int {
	byGroup := map[int][]int{}
	for _, s := range slots {
		if s.KeywordGroupID != 0 {
			byGroup[s.KeywordGroupID] = append(byGroup[s.KeywordGroupID], s.Index)
		}
	}
	peers := map[int][]int{}
	for _, members := range byGroup {
		for _, self := range members {
			for _, other := range members {
				if other != self {
					peers[self] = append(peers[self], other)
				}
			}
		}
	}
	return peers
}

// keywordGroupInstruction is appended to a grouped slot's prompt line.
//
// Two prior designs both failed empirically, for two DIFFERENT reasons, both
// traced directly in internal/synthesize/yake.go rather than guessed:
//   - A pre-scripted generic phrase ("technical debt") landed verbatim in
//     every group member's body, but never ranked in any single document's
//     own top-yakeTopK candidates — it was one incidental mid-paragraph
//     mention, competing against other candidate phrases in the same dense
//     text for very few slots.
//   - A specific named entity (a product/protocol/CVE) is structurally
//     excluded regardless of prominence: yakeDedup drops any single-word
//     candidate whenever a longer (yakeMaxN=2) co-occurring phrase scores at
//     least as well, and natural prose almost always supplies one.
//
// What survives on real data (cyberai-kb.db's actual keyword bridges —
// "billion valuation", "active exploitation", "data center") is a two-word
// phrase that is the CENTRAL POINT of an early sentence in its own document,
// not an entity name and not an incidental mention. This instruction asks
// for that specifically: agree on one two-to-three-word phrase, and make it
// the subject of each fact's opening sentence, not a passing reference.
func keywordGroupInstruction(otherIndices []int) string {
	others := make([]string, len(otherIndices))
	for i, idx := range otherIndices {
		others[i] = fmt.Sprintf("#%d", idx)
	}
	return fmt.Sprintf(", KEYWORD GROUP with fact(s) %s: agree on ONE shared two-to-three-word "+
		"descriptive phrase (not a proper noun/product/protocol name — a plain descriptive phrase "+
		"like \"active exploitation\" or \"billion valuation\") and make that phrase the CENTRAL "+
		"SUBJECT of each fact's opening sentence — not a passing mention later in the body",
		strings.Join(others, ", "))
}

// extractJSONArray extracts the JSON array from a completion that may have
// leading and/or trailing prose around it, with or without a markdown code
// fence. Real mode reliably produces this: after doing several WebSearch
// calls, the model often writes a short transition sentence before its
// answer ("I have enough grounded material now, here are the five facts:")
// despite being told to reply with ONLY a JSON array — this recurred across
// multiple real runs, including exhausting all 3 retries in a row once,
// with several different exact phrasings, so no single prompt tweak reliably
// suppresses it. Rather than keep special-casing new prefixes, find the
// actual array boundaries directly: the first '[' and the LAST ']' in the
// response (a fenced or prose-prefixed reply still has the real array
// delimiters intact; only what's outside them is noise). Falls back to
// returning s unchanged if no bracket pair is found, so the caller's own
// json.Unmarshal error still surfaces a sensible message.
func extractJSONArray(s string) string {
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start == -1 || end == -1 || end < start {
		return s
	}
	return s[start : end+1]
}
