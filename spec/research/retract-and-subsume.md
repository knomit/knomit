# Research: Retract & Subsume (replacing `knomit_forget`)

## Problem

The current `knomit_forget` tool conflates two semantically distinct operations:

1. A fact is **no longer true** — "the API uses REST" turns out to be wrong, a CVE was patched, a preference changed.
2. A fact was **absorbed into a higher-level synthesis** — "Alice likes rock," "Alice likes jazz," "Alice likes blues" became "Alice has broad music taste, especially rock."

Calling both "forget" is misleading. Nothing is forgotten — git history preserves everything. The difference matters for merge reconciliation, provenance tracking, and the distill/prune cycle.

## Two Operations

### Retract

A fact is marked as no longer true. The file is deleted; git history retains what was believed and when.

- **When:** An agent discovers a fact is outdated, incorrect, or no longer relevant.
- **Commit message:** `retract(reason): path/to/fact.md`
- **Tag:** `retract/{reason}`
- **Inputs:** `{ file: string, moment_name: string }`
- **Merge behavior:** A retraction on any branch wins over a stale version on another branch (Case 4 in merge-cases.md — obsolescence). More recent + intentional deletion takes precedence.

Examples:
- CVE was patched → retract the vulnerability fact
- Project switched from REST to GraphQL → retract the REST fact, learn the GraphQL fact
- A preference changed → retract old preference, learn new one

### Subsume

A fact is absorbed into a higher-level synthesis. The file is deleted, but the synthesized fact that replaces it is recorded as the target.

- **When:** The distill/prune cycle produces a synthesis that fully captures one or more original facts.
- **Commit message:** `subsume(synthesis-name): path/to/original.md → path/to/synthesis.md`
- **Tag:** `subsume/{synthesis-name}`
- **Inputs:** `{ file: string, target: string, moment_name: string }` — `target` is the synthesized fact that now covers this one.
- **Merge behavior:** If a subsumed fact is modified on another branch, the merge must check whether the modification is already captured by the synthesis target. If not, the modification should be folded into the synthesis rather than silently dropped.

Examples:
- Three music preference facts → subsumed into one broad taste profile
- Five separate meeting notes about a project decision → subsumed into one decision record
- Multiple small observations about a person's communication style → subsumed into a behavioral summary

## Why Two Tools, Not One

| Concern | Retract | Subsume |
|---------|---------|---------|
| Semantics | "This was wrong/stale" | "This is captured elsewhere now" |
| Inputs | file + reason | file + target + reason |
| Git history signal | Deletion = correction | Deletion = consolidation |
| Merge handling | Retraction wins over stale data | Must verify target still covers the content |
| Downstream effect | Fact is gone from the knowledge base | Fact lives on inside the synthesis |

A single tool with a `reason` enum (`knomit_remove --reason=retract|subsume`) was considered but rejected:
- Different input shapes (subsume needs `target`, retract doesn't)
- Different merge reconciliation logic
- Different provenance chains
- The tool description would be overloaded, making it harder for LLMs to choose correctly

## MCP Tool Definitions

### `knomit_retract`

```
"Remove a fact that is no longer true, relevant, or was stored in error.
The file is deleted from the repo; git history retains provenance.
Use when correcting outdated or incorrect knowledge."

Input: { file: string, moment_name: string }
```

### `knomit_subsume`

```
"Remove a fact that has been fully absorbed into a higher-level synthesis.
The original file is deleted; the synthesized fact that replaces it is
recorded for provenance. Use during distill/prune when a synthesis
captures all the information from the original."

Input: { file: string, target: string, moment_name: string }
```

## Impact on Existing Code

- `src/tools/forget.ts` → split into `src/tools/retract.ts` and `src/tools/subsume.ts`
- `src/index.ts` → register both new tools, remove `registerForgetTool`
- `src/tui/commands.ts` → update command type from `"forget"` to `"retract"` (TUI unlikely to subsume interactively)
- Tags change from `forget/` prefix to `retract/` or `subsume/`
- Commit messages change from `forget(...)` to `retract(...)` or `subsume(...)`
- Research docs referencing "forget" should be updated to use the new terminology

## Impact on Synthesis

The distill/prune cycle (fact-synthesis.md) currently references `forget` in its LLM output plan:

```
{ learn: [...], update: [...], forget: [...] }
```

This becomes:

```
{ learn: [...], update: [...], retract: [...], subsume: [...] }
```

The LLM must distinguish: is this fact being removed because it's wrong (retract), or because a new synthesis captures it (subsume)? The prompt should make this explicit. In practice, prune mostly produces retractions and distill mostly produces subsumptions.

## Rejected Alternatives

| Name | Why rejected |
|------|-------------|
| `forget` | Implies the knowledge is lost; it isn't — git history preserves it |
| `revoke` | Implies authority/permission, not knowledge correction |
| `archive` | Too passive; implies storage, not active removal |
| `deprecate` | Implies "still works, just discouraged" — wrong for facts |
| `supersede` | Good for subsume case but doesn't cover "just wrong now" |
| `remove` | Generic; doesn't encode *why* — loses the semantic signal |
