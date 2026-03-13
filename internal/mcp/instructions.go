package mcp

import "fmt"

// baseInstructionsText returns the base MCP server instructions with the given ontology root.
func baseInstructionsText(ontologyRoot string) string {
	return fmt.Sprintf(`You are connected to a knomit knowledge base. Use the available tools to learn, query, and manage knowledge.

Key concepts:
- Facts are stored as markdown files under %s/ with YAML frontmatter (domain, entities, confidence, sources, refs)
- Each fact has a path like %s/topic/subtopic/fact-name.md
- Use knomit_learn to store new knowledge, knomit_query to search, knomit_why for provenance
- Use knomit_update to modify existing facts, knomit_retract to remove outdated knowledge
- Use knomit_explore to browse the knowledge tree`, ontologyRoot, ontologyRoot)
}

// ProfileInstructions returns the MCP server instructions for the given profile.
// Valid profiles: "code", "chat", "generic". Unknown profiles fall back to "code".
func ProfileInstructions(profile, ontologyRoot string) string {
	addendum, ok := profileAddenda[profile]
	if !ok {
		addendum = profileAddenda["code"]
	}
	return baseInstructionsText(ontologyRoot) + "\n\n" + addendum
}

var profileAddenda = map[string]string{
	"code": `You are assisting with software development. When learning new facts, prefer structured technical knowledge: architecture decisions, API contracts, debugging findings, conventions, and system behaviors. Use domain tags like "architecture", "debugging", "conventions", "api". Reference source code locations in refs when applicable.`,

	"chat": `You are in a conversational context. When learning new facts, capture insights, preferences, decisions, and context from the conversation. Use natural language for fact bodies. Prefer broader domain tags. Keep confidence scores conservative for subjective knowledge.`,

	"generic": `You are a general-purpose knowledge assistant. Store and retrieve knowledge across any domain. Use descriptive domain and entity tags. Maintain clear, self-contained fact bodies that can be understood without additional context.`,
}
