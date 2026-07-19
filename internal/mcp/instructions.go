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

## Writing Fact Bodies

Consumers act on the slogan they compress a fact into, not on the fact itself. A fact can be entirely true and still cause failures if its natural one-line summary drops a condition. Write bodies that survive careless compression:

- **State compound conditions in full** — name every component, every time. "Refunds auto-approve under $50 for accounts older than 90 days" must never be stored or summarized as "refunds under $50 are automatic"; the dropped component is how a true fact becomes a false slogan.
- **State the operational consequence** — what should a reader do, or never do, because this fact is true?
- **Name the foreseeable misreading** — for rules and policies, if there is an attractive-but-false corollary a hurried reader would draw, add a line saying explicitly what the fact does NOT mean.
- **Anchor high-confidence rules in refs** — a rule should reference the authoritative source that verifies it; a rule with no anchor cannot be checked and earns less trust.

Discipline: do not drown facts in speculative caveats — name only misreadings you can actually foresee.

## Fact Frontmatter

Each fact has YAML frontmatter with:
- **kind**: classification family (defaults to "epistemic" if omitted):
  - epistemic: descriptive knowledge — what is
  - pragmatic: prescriptive knowledge — what to do
- **type**: leaf type within the chosen kind. Allowed values depend on kind.
  - epistemic types (default "observation" if omitted):
    - observation: concrete, specific statements ("Alice likes Japanese tea")
    - concept: definitions, mental models ("Japanese tea culture emphasizes mindfulness")
    - process: procedures, workflows, how-to ("How to brew matcha")
    - principle: rules, causal claims ("Brew below boiling to avoid bitterness")
    - pattern: recurring solutions, idioms ("When X, do Y")
    - reference: specs, measurements, enumerations ("Sencha steeps at 70°C for 60s")
    - synthesis: higher-order facts derived from other facts (set automatically by the synthesize pipeline)
    - insight: a non-obvious grounded conclusion drawn from connecting facts you already trust ("X and Y together imply Z")
    - hypothesis: predictions derived from patterns — carries inherent uncertainty, not grounded in direct observation
    - methodology: reasoning process lessons learned from hypothesis outcomes (lives in meta/reasoning/)
  - pragmatic types (must be specified — no default):
    - policy: mandatory rule that should always be followed ("Always rotate secrets quarterly")
    - heuristic: rule-of-thumb to bias decisions, not absolute ("Prefer small PRs")
- **domain**: cross-cutting tags from additional classification systems (not the primary ontology path)
- **entities**: all entities this fact mentions (for search and graph queries)
- **confidence**: 0.0–1.0 certainty level
- **sources**: number of independent sources
- **refs**: external URLs or source-file lineage
- **origin**: which pipeline minted the fact — NOT where the information came from. authored = anything you write yourself, the default; this includes facts transcribed from sources you read. distilled = synthesis-pipeline output (type synthesis). discovered = discovery-engine output (type synthesis or hypothesis). Immutable after write — knomit_update cannot change it.

## Tools

- **knomit_learn**: store new knowledge — provide topic, category, title, body, and metadata. The server handles deduplication automatically within the same category. Leave origin unset for facts you write directly (defaults to authored). When you persist a proposal from a discover or distill work-item, set the origin that work-item's prompt specifies (discovered for a cross-cluster bridge, distilled for a regular cluster) — origin reflects how the candidate group was formed, not whether you previewed it.
- **knomit_query**: search existing knowledge. Filters:
  - text: semantic search across all facts
  - entities: filter to facts mentioning specific entities (all must match)
  - domain: filter by cross-cutting domain tags (prefix match — "tech" matches "tech/cloud")
  - path: filter by path prefix — use this to scope searches to a topic or category. Examples:
    - path: "%s/technology" → all technology facts
    - path: "%s/technology/go" → all Go-related facts
    - path: "%s/people/alice" → all facts about Alice
  - min_confidence: minimum confidence threshold (0–1)
  - sort: set to "recent" to browse facts ordered by most recently committed (paginated, 25 per page). Use path to scope to a subtree. Pass the returned cursor to get the next page.
- **knomit_explain**: explain a fact by walking its versioned provenance graph. Anchored at a commit — pass commit to explain the fact AS OF that version (the graph is rewound to how it stood then), or omit it for HEAD. Every referenced fact is read at the exact version the referrer pointed to, recursively. The root fact comes back in full with its evolution history (recent revisions + confidence/content diffs); every other fact is a lean summary (no body) flagged summary:true — re-call knomit_explain with that fact's path AND commit to read it in full and walk its subtree. A summary may be flagged deleted:true (source retracted since the edge formed) or superseded:true (source still live but changed since the referrer reasoned over it). Use file to start, pass cursor for next page. External URL refs are returned for you to inspect.
- **knomit_update**: modify an existing fact's fields. List fields (domain, entities, refs) are replaced wholesale — send the complete new list, because any existing entry you leave out is dropped; omit a field entirely to leave it unchanged. Prior revisions keep their refs in history, so replacing refs never erases past provenance. It cannot change origin or the topic/category path — fixing those requires knomit_retract plus a fresh knomit_learn.
- **knomit_retract**: remove outdated knowledge

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
2. You'll receive a synthesis fact to investigate, with applicable methodology already loaded into the work-item instructions
3. Use knomit_explain on the synthesis fact to trace its provenance
4. Gather additional evidence as needed
5. If a hypothesis is warranted, call knomit_learn with type: hypothesis
6. After writing the hypothesis, call knomit_learn with type: methodology, topic: "meta", category: "reasoning" to record the reasoning process. Set domain and entities to the union of the synthesis fact's tags plus the standard methodology markers (meta, reasoning, methodology) — inherit, don't reinvent.
7. Call knomit_hypothesize with session_id to get the next synthesis fact
8. Repeat until done

Hypothesis body must contain: hypothesis statement, evidence chain (with confidence/sources for each cited fact), reasoning step, known gaps, and falsification condition.

Important: hypotheses must only cite observations and synthesis facts as evidence — never other hypotheses.`, ontologyRoot, ontologyRoot, topicList, ontologyRoot, ontologyRoot, ontologyRoot)
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
	"code": `You are assisting with software development. When learning new facts, prefer structured technical knowledge: architecture decisions, API contracts, debugging findings, conventions, and system behaviors. Use domain tags like "architecture", "debugging", "conventions", "api". Reference source code locations in refs when applicable. Invariants and policies MUST anchor to the code that enforces them via src://<source>/<path>@<commit> refs — a rule with no code anchor cannot be verified against the current tree and should be treated as low-trust. When stating a compound key or condition, quote it verbatim from the code (every component), never a paraphrase.`,

	"chat": `You are in a conversational context. When learning new facts, capture insights, preferences, decisions, and context from the conversation. Use natural language for fact bodies. Prefer broader domain tags. Keep confidence scores conservative for subjective knowledge.`,

	"generic": `You are a general-purpose knowledge assistant. Store and retrieve knowledge across any domain. Use descriptive domain and entity tags. Maintain clear, self-contained fact bodies that can be understood without additional context.`,
}
