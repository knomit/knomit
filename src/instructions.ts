const BASE = `You have access to Knomit, a persistent knowledge base that survives
across sessions. It stores structured facts as markdown files in a
Git repository, organized by an ontological hierarchy (know/).

Your knowledge base operates on an agent-specific branch. Other
agents may contribute knowledge that arrives via merges from main.
If a merge conflict occurs, you will be notified and should resolve
it using knomit_update.

DURING CONVERSATION:
- When the user states a preference, makes a decision, or you jointly
  arrive at a conclusion — call knomit_learn to persist it.
- When you need deeper context on a fact — call knomit_why.
- When a previous fact is reinforced or contradicted — call knomit_update.

WHAT TO PERSIST:
- Decisions, preferences, architectural choices, conclusions.
- NOT transient discussion, obvious facts, or things easily re-derived.

STRUCTURING THE ONTOLOGY:
The directory tree under know/ is a hierarchy. Use depth to reflect
scope and durability:

  know/projects/myapp/
    architecture.md          <- durable: rarely changes
    conventions.md           <- durable: coding style, patterns
    caveats.md               <- durable: known pitfalls
    migration/
      plan.md                <- ephemeral: specific to this effort
      current-state.md       <- ephemeral: will become stale

  know/preferences/
    editor.md                <- cross-project user preferences
    workflow.md

  know/tools/
    bun.md                   <- tool-specific knowledge

Group by durability: durable facts at higher levels, ephemeral facts
in sub-directories that can be cleaned up when no longer relevant.
Facts at a directory level are inherited by everything below it.

FACT QUALITY:
- Title: concise, descriptive — this is the primary search surface.
- Body: the actual knowledge. Include reasoning, not just conclusions.
- Confidence: 0.0–1.0. Use 0.9+ for explicit user statements,
  0.7–0.8 for inferred conclusions, 0.5–0.6 for tentative observations.
- Entities: people, projects, tools — anything you'd want to query by.
- Domain: topic tags like "architecture", "testing", "migration".
- Refs: anchor facts to their source using knomit: URIs.
  For facts about code in external repos, use the full form:
  "knomit://github.com/org/repo/blob/<commit>/<path>"
  (mirrors GitHub blob URLs but with the knomit: scheme).
  For facts referencing other knowledge base facts, use the relative form:
  "knomit:blob/<commit>/<path>" (no authority = current repo).
  Also acceptable: plain URLs, issue numbers, or document names.
- Sources: increment when multiple sessions confirm the same fact.

QUERYING:
- Start broad (entity or domain), then narrow by path if needed.
- When working on a project, query by project name/path first.
- Check refs against current state — a fact anchored to an old
  commit may be outdated.`;

const CODE_ADDENDUM = `
CODE EDITOR INTEGRATION:
- At session start, query by the current project name or directory
  to load relevant context before responding.
- When learning facts about a project for the first time, first create
  an identity fact at the project root (e.g. know/projects/myapp/identity.md)
  containing the git remote origin URL (if any), default branch, and a
  brief description. This anchors all child facts to a concrete repository.
- Every fact about project code MUST include a knomit: ref for
  staleness detection. Run "git rev-parse --short HEAD" and optionally
  "git remote get-url origin" (which may fail for local-only repos).
  With remote: "knomit://github.com/org/repo/blob/abc1234/src/file.ts"
  Without remote: "knomit:blob/abc1234/src/file.ts" (relative to project).
- Persist architectural decisions, debugging insights, and patterns
  discovered during code review.
- At session end, review what was decided and learn anything worth
  remembering.`;

const CHAT_ADDENDUM = `
CONVERSATIONAL INTEGRATION:
- At session start, query by relevant entities or domains to load
  context from previous sessions before responding.
- Refs are typically URLs, document names, or conversation topics
  rather than git commits.
- Persist preferences, decisions, and conclusions from discussion.
- At session end, review what was decided and learn anything worth
  remembering.`;

const GENERIC_ADDENDUM = `
- At session start, query relevant entities or domains to load context.
- At session end, persist any decisions or conclusions worth remembering.`;

export type McpProfile = "code" | "chat" | "generic";

export const MCP_PROFILES: McpProfile[] = ["code", "chat", "generic"];

export function getInstructions(profile: McpProfile = "code"): string {
  switch (profile) {
    case "code":
      return BASE + CODE_ADDENDUM;
    case "chat":
      return BASE + CHAT_ADDENDUM;
    case "generic":
      return BASE + GENERIC_ADDENDUM;
  }
}
