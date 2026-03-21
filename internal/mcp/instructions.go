package mcp

import (
	"fmt"
	"strings"

	"knomit/internal/fact"
)

// baseInstructionsText returns the base MCP server instructions with the given ontology root.
func baseInstructionsText(ontologyRoot string, ontology *fact.Ontology) string {
	// Build dynamic topic list from ontology.
	var topicList string
	if ontology != nil {
		var lines []string
		for _, name := range ontology.TopicNames() {
			node := ontology.Topics[name]
			if node.Description != "" {
				lines = append(lines, fmt.Sprintf("  %s — %s", name, node.Description))
			} else {
				lines = append(lines, fmt.Sprintf("  %s", name))
			}
		}
		topicList = strings.Join(lines, "\n")
	} else {
		topicList = "  (no ontology loaded)"
	}
	return fmt.Sprintf(`You are connected to a knomit knowledge base. Use the available tools to learn, query, and manage knowledge.

## Ontology Structure

Facts are organized under %s/ using a two-level classification:

  %s/<topic>/<category>/<uuid>.md

- **topic**: A validated top-level folder. Available topics:
%s
- **category**: A freeform path you choose within the topic (e.g. "cameras/smart-home", "go/concurrency", "alice")
- **uuid**: Server-generated — you never provide or see this. Each fact gets a unique ID automatically.

### Choosing topic and category

Pick the topic that best fits the subject area. Then build a category path that creates a navigable hierarchy — think of it as a file system for knowledge. A good category path has 2–4 segments that narrow from general to specific:

- "technology" + "languages/go/concurrency" — not just "go" or "go/concurrency"
- "technology" + "hardware/cameras/smart-home" — not just "cameras"
- "people" + "colleagues/alice" — not just "alice"
- "science" + "natural/physics/quantum-mechanics" — not just "physics"
- "business" + "companies/acme-corp/products" — not just "acme-corp"
- "technology" + "infrastructure/kubernetes/networking" — not just "kubernetes"
- "health" + "nutrition/supplements/vitamin-d" — not just "vitamin-d"

Avoid flat categories. "technology" + "react" is too shallow — prefer "technology" + "frameworks/frontend/react". The goal is that browsing the tree at any level reveals a manageable number of subcategories, not hundreds of siblings. Think: would someone navigating this tree understand where to look?

## Fact Frontmatter

Each fact has YAML frontmatter with:
- **type**: epistemic type (defaults to "observation" if omitted):
  - observation: concrete, specific statements ("Alice likes Japanese tea")
  - concept: definitions, mental models ("Japanese tea culture emphasizes mindfulness")
  - process: procedures, workflows, how-to ("How to brew matcha")
  - principle: rules, heuristics, causal claims ("Brew below boiling to avoid bitterness")
  - pattern: recurring solutions, idioms ("When X, do Y")
  - reference: specs, measurements, enumerations ("Sencha steeps at 70°C for 60s")
  - synthesis: higher-order facts derived from other facts (set automatically by the synthesize pipeline)
  - hypothesis: predictions derived from patterns — carries inherent uncertainty, not grounded in direct observation
  - methodology: reasoning process lessons learned from hypothesis outcomes (lives in meta/reasoning/)
- **domain**: cross-cutting tags from additional classification systems (not the primary ontology path)
- **entities**: all entities this fact mentions (for search and graph queries)
- **confidence**: 0.0–1.0 certainty level
- **sources**: number of independent sources
- **refs**: external URLs or source-file lineage

## Tools

- **knomit_learn**: store new knowledge — provide topic, category, title, body, and metadata. The server handles deduplication automatically within the same category.
- **knomit_query**: search existing knowledge. Filters:
  - text: semantic search across all facts
  - entities: filter to facts mentioning specific entities (all must match)
  - domain: filter by cross-cutting domain tags (prefix match — "tech" matches "tech/cloud")
  - path: filter by path prefix — use this to scope searches to a topic or category. Examples:
    - path: "%s/technology" → all technology facts
    - path: "%s/technology/go" → all Go-related facts
    - path: "%s/people/alice" → all facts about Alice
  - min_confidence: minimum confidence threshold (0–1)
- **knomit_explain**: explain a fact by traversing its provenance graph — follows local refs breadth-first, returning referenced facts as they existed at the root fact's commit time. Returns paginated results. Use file to start, pass cursor for next page. External URL refs are returned for you to inspect.
- **knomit_update**: modify an existing fact's fields
- **knomit_retract**: remove outdated knowledge
- **knomit_explore**: browse facts ordered by most recently updated. Returns paginated results (25 per page). Call with no arguments to start; pass the returned cursor to get the next page. Use path to scope to a subtree (e.g. path: "%s/technology"). Use knomit_explain for history on individual facts.

## knomit_review — Knowledge Base Maintenance

Call this tool to review and maintain the knowledge base. It works as a multi-turn conversation:

1. Call knomit_review with no arguments to start a review session
2. You'll receive a prompt describing facts to evaluate and a response_schema
3. Reason about the facts, then call knomit_review again with:
   - session_id: the ID from the previous response
   - response: your JSON decisions matching the response_schema
4. Repeat until the response contains "done": true

You may stop at any time — progress is saved and the next session picks up remaining work.

## knomit_hypothesize — Hypothesis Generation

Call this tool to generate hypotheses from synthesis facts. Works the same way as knomit_review:

1. Call knomit_hypothesize with no arguments to start a session
2. You'll receive a synthesis fact to investigate
3. Use knomit_query (with path: "%s/meta/reasoning/" + domain/entity filters) to find applicable methodology
4. Use knomit_explain on the synthesis fact to trace its provenance
5. Gather additional evidence as needed
6. If a hypothesis is warranted, call knomit_learn with type: hypothesis
7. Call knomit_hypothesize with session_id to get the next synthesis fact
8. Repeat until done

Hypothesis body must contain: hypothesis statement, evidence chain (with confidence/sources for each cited fact), reasoning step, known gaps, and falsification condition.

Important: hypotheses must only cite observations and synthesis facts as evidence — never other hypotheses.`, ontologyRoot, ontologyRoot, topicList, ontologyRoot, ontologyRoot, ontologyRoot, ontologyRoot, ontologyRoot)
}

// ProfileInstructions returns the MCP server instructions for the given profile.
// Valid profiles: "code", "chat", "generic". Unknown profiles fall back to "code".
func ProfileInstructions(profile, ontologyRoot string, ontology *fact.Ontology) string {
	addendum, ok := profileAddenda[profile]
	if !ok {
		addendum = profileAddenda["code"]
	}
	return baseInstructionsText(ontologyRoot, ontology) + "\n\n" + addendum
}

var profileAddenda = map[string]string{
	"code": `You are assisting with software development. When learning new facts, prefer structured technical knowledge: architecture decisions, API contracts, debugging findings, conventions, and system behaviors. Use domain tags like "architecture", "debugging", "conventions", "api". Reference source code locations in refs when applicable.`,

	"chat": `You are in a conversational context. When learning new facts, capture insights, preferences, decisions, and context from the conversation. Use natural language for fact bodies. Prefer broader domain tags. Keep confidence scores conservative for subjective knowledge.`,

	"generic": `You are a general-purpose knowledge assistant. Store and retrieve knowledge across any domain. Use descriptive domain and entity tags. Maintain clear, self-contained fact bodies that can be understood without additional context.`,
}
