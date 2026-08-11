package mcp

import (
	"fmt"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
)

// lensInstructions builds the session addendum that makes a lens legible to an
// agent (RFC §9.4): the mount table (name ↔ 12-hex id ↔ branch ↔ role ↔
// source), each read mount's topic coverage, the kb:// qualified-path
// convention, and the rule that read-mount facts are read-only through this
// lens. Returns "" for a lens-of-one — single-repo sessions keep today's
// byte-for-byte instructions.
func lensInstructions(b *repos.Binding) string {
	if !b.IsLens() {
		return ""
	}
	var sb strings.Builder

	sb.WriteString("\n\n## Federated knowledge base (lens)\n\n")
	sb.WriteString(fmt.Sprintf(
		"You are connected to the lens %q: a virtual knowledge base federating several repos. "+
			"You WRITE to exactly one of them (the write repo); the others are READ-ONLY through this lens.\n\n",
		b.Name()))

	sb.WriteString("### Mounts\n\n")
	sb.WriteString("| repo | id | branch | role | source |\n")
	sb.WriteString("|---|---|---|---|---|\n")
	for _, rt := range b.Reads() {
		role := "read"
		// Gate on WriteOK() for parity with knomit_repos (repos.go): a binding
		// whose write mount is a non-writable branch is a read-only view, so its
		// write-repo row is still just "read". Latent today — a lens always has
		// WriteOK() == true — but kept in lockstep with the repos-table rule.
		if rt.RI == b.Write() && b.WriteOK() {
			role = "read+write"
		}
		src := rt.Source
		if src == "" {
			src = "—"
		}
		sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s |\n",
			rt.RI.Name(), federate.ID12(rt.RI.ID()), rt.Branch, role, src))
	}

	// The branch column is the READ branch of each mount. Writes never go there:
	// they always commit to the write repo's agent branch (RFC decision 19 / M-4).
	sb.WriteString(fmt.Sprintf(
		"\nThe branch column shows the READ branch of each mount. Your writes always commit to "+
			"the write repo's branch `%s`, regardless of the branch it is read at above.\n",
		b.Write().AgentBranch()))

	sb.WriteString("\n### Addressing (qualified paths)\n\n")
	sb.WriteString(
		"Every fact path in a result is EITHER a bare `" + b.Write().OntologyRoot() + "/…` path " +
			"(the write repo) OR a qualified `kb://<repo-id>/…` path (a read mount). " +
			"The `<repo-id>` is the 12-hex id from the Mounts table above. " +
			"Use the qualified path verbatim as the `file` argument to `knomit_explain` to read that fact, " +
			"and store it verbatim in a fact's `refs` to cite across repos — the server never rewrites it.\n\n")
	// A concrete example using the first read mount that is not the write repo.
	for _, rt := range b.Reads() {
		if rt.RI != b.Write() {
			sb.WriteString(fmt.Sprintf("Example: a fact in %q reads as `kb://%s/%s/…`.\n\n",
				rt.RI.Name(), federate.ID12(rt.RI.ID()), rt.RI.OntologyRoot()))
			break
		}
	}

	sb.WriteString("### Correcting a read-mount fact\n\n")
	sb.WriteString(
		"Facts from read mounts are READ-ONLY through this lens: `knomit_update` and `knomit_retract` on a " +
			"`kb://<read-mount-id>/…` path are rejected. To correct such a fact, connect to that repo's own " +
			"endpoint (or a lens whose write repo is that repo) and edit it there.\n\n")

	sb.WriteString("### Read coverage\n\n")
	sb.WriteString("Topics available per mount (a query fans out to all mounts; a mount lacking a topic simply contributes nothing):\n\n")
	for _, rt := range b.Reads() {
		topics := "(none)"
		if o := rt.RI.Ontology(); o != nil {
			if names := o.TopicNames(); len(names) > 0 {
				topics = strings.Join(names, ", ")
			}
		}
		sb.WriteString(fmt.Sprintf("- **%s** (`%s`): %s\n", rt.RI.Name(), federate.ID12(rt.RI.ID()), topics))
	}

	return sb.String()
}

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

## Private State

Machinery that is not knowledge — a periodic job's bookkeeping — is written by passing path to knomit_learn instead of topic/category, with the path under %s/<area>/ (any area name you choose). Such a fact is INVISIBLE to knomit_query, the UI and export, by design: address it afterwards by its exact path, never by search. knomit_update, knomit_retract and knomit_explain all work on it normally — explain's revision history is the point, and is how a job reconstructs what past runs did. learn allocates the slot once and fails if it already exists; every later write is an update.

## Fact Frontmatter

Each fact has YAML frontmatter with:
- **kind**: classification family (defaults to "epistemic" if omitted):
%s
- **type**: leaf type within the chosen kind. Allowed values depend on kind.
  - epistemic types (default "observation" if omitted):
%s
  - pragmatic types (must be specified — no default):
%s
- **domain**: cross-cutting tags from additional classification systems (not the primary ontology path)
- **entities**: all entities this fact mentions (for search and graph queries)
- **confidence**: 0.0–1.0 certainty level
- **sources**: number of independent sources
- **refs**: citations. For a fact in THIS repo use the bare path as knomit_query returned it (kb/topic/.../id.md); the server rewrites it to the canonical kb://<repo-id>/<path> form on write, so you never supply this repo's id. For a fact in ANOTHER repo, copy the kb://<repo-id>/<path> form verbatim from the result that gave it to you; knomit_query and knomit_explain already return other repos' paths that way, and knomit_repos lists every mounted repo's id. For source use src://<source-repo-id>/<path>@<commit>:<blob>, where the id is the SOURCE repo's root commit, not a knomit id. A ref to a fact in THIS repo must resolve when the call lands — write the cited fact in the same call, or in an earlier one. A ref that will not resolve rejects the whole call and names every offending ref.
- **origin**: which pipeline minted the fact — NOT where the information came from. %s Immutable after write — knomit_update cannot change it.

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

Important: hypotheses must only cite observations and synthesis facts as evidence — never other hypotheses.`,
		ontologyRoot, ontologyRoot, topicList,
		fact.PrivateRoot,
		// The frontmatter vocabulary is rendered from the shared tables in
		// factschema.go rather than restated here, so the instructions and the
		// knomit_learn/knomit_update JSON schemas can never drift apart on
		// what a type or origin means. Only the surrounding prose — the
		// headers and the default/no-default notes — is literal, because that
		// framing is instructional rather than definitional.
		instructionKindLines("  "),
		instructionTypeLines(fact.AllEpistemicTypes(), "    "),
		instructionTypeLines(fact.AllPragmaticTypes(), "    "),
		originGlossSentence(),
		ontologyRoot, ontologyRoot, ontologyRoot)
}

// BindingInstructions computes a session's instructions from its binding: the
// write repo's ontology + the write-repo profile addendum (single-repo output,
// unchanged), followed by the lens addendum when the binding federates
// (empty for a lens-of-one). Profile is the WRITE repo's — authoring guidance
// describes what you write, which is always the write repo (RFC §8, §9.4).
func BindingInstructions(b *repos.Binding, profile string) string {
	w := b.Write()
	return ProfileInstructions(profile, w.OntologyRoot(), w.Ontology()) + lensInstructions(b)
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
	"code": `You are assisting with software development. When learning new facts, prefer structured technical knowledge: architecture decisions, API contracts, debugging findings, conventions, and system behaviors. Use domain tags like "architecture", "debugging", "conventions", "api". Reference source code locations in refs when applicable. Invariants and policies MUST anchor to the code that enforces them via src://<repo-id>/<path>@<commit>:<blob> refs — a rule with no code anchor cannot be verified against the current tree and should be treated as low-trust. Produce the three components with: git rev-list --max-parents=0 HEAD | cut -c1-12 (the repo id), git rev-parse HEAD (the commit, full 40 hex), git rev-parse <commit>:<path> (the blob, full 40 hex). That last command failing IS the check — verify the file exists at that commit BEFORE writing the ref, because knomit holds no source objects and cannot check it for you. Add #L<start>-L<end> when the fact is about specific lines rather than a whole file. A blob-anchored ref stays retrievable with git cat-file blob <blob> even after the file is renamed or deleted, which a commit-only ref does not. When stating a compound key or condition, quote it verbatim from the code (every component), never a paraphrase.`,

	"chat": `You are in a conversational context. When learning new facts, capture insights, preferences, decisions, and context from the conversation. Use natural language for fact bodies. Prefer broader domain tags. Keep confidence scores conservative for subjective knowledge.`,

	"generic": `You are a general-purpose knowledge assistant. Store and retrieve knowledge across any domain. Use descriptive domain and entity tags. Maintain clear, self-contained fact bodies that can be understood without additional context.`,
}
