# Rename worlds→know, machine→agent Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace `worlds/` with `know/`, `machine/` branch prefix with `agent/`, introduce constants, and fix README inaccuracies.

**Architecture:** Create `src/constants.ts` with `ONTOLOGY_DIR`, `ONTOLOGY_ROOT`, `BRANCH_PREFIX`. All code references use these constants. Mechanical rename of string literals in tests and docs. The `type: "world" | "fact"` discriminator stays unchanged (it's a semantic type, not a directory name).

**Tech Stack:** Bun, TypeScript, React (Ink)

---

### Task 1: Create Constants and Update git.ts Core

**Files:**
- Create: `src/constants.ts`
- Modify: `src/git.ts`

**Step 1: Create constants file**

```ts
// src/constants.ts
export const ONTOLOGY_DIR = "know";
export const ONTOLOGY_ROOT = "know.md";
export const BRANCH_PREFIX = "agent";
```

**Step 2: Update git.ts**

Import constants:
```ts
import { ONTOLOGY_DIR, ONTOLOGY_ROOT, BRANCH_PREFIX } from "./constants";
```

Rename `machineId` → `agentId` (property, constructor param, all internal references):
- Line 48: `readonly agentId: string;`
- Line 52: `constructor(repoPath: string, agentId?: string) {`
- Line 54: `this.agentId = agentId ?? hostname();`
- Line 57-58: `get branchName(): string { return \`${BRANCH_PREFIX}/${this.agentId}\`; }`

Update `init()`:
- Line 131 comment: `// Ensure we're on the agent branch`
- Line 160: variable `worldsMd` → `rootManifest`
- Line 171: `await Bun.write(join(this.repoPath, ONTOLOGY_ROOT), rootManifest);`
- Line 172: `await this.gitOrThrow("add", ONTOLOGY_ROOT);`
- Line 175 comment: `// Rename default branch to main, then create agent branch`

Update `grep()`:
- Line 443: `const searchPath = path ?? \`${ONTOLOGY_DIR}/\`;`

Update `diffFiles()`:
- Line 461: `const result = await this.git("diff", "--name-status", fromCommit, "HEAD", "--", \`${ONTOLOGY_DIR}/\`);`

**Step 3: Run tests**

Run: `bun test src/git.test.ts`
Expected: FAIL (tests still reference old names — that's expected, we update tests in Task 6)

**Step 4: Commit**

```
git add src/constants.ts src/git.ts
git commit -m "refactor: introduce constants, rename machineId→agentId in git.ts"
```

---

### Task 2: Update bootstrap.ts and CLI Files

**Files:**
- Modify: `src/bootstrap.ts`
- Modify: `src/cli/mcp.ts`
- Modify: `src/cli/synthesize.ts`

**Step 1: Update bootstrap.ts**

- Line 26: `const agentId = process.env.KNOMIT_AGENT_ID ?? hostname();`
- Line 29: `const repo = new GitRepo(repoPath, agentId);`
- Line 41: `return { repo, searchIndex, repoPath, agentId, cacheDir };`

**Step 2: Update cli/mcp.ts**

- Line 89: `log.info(\`running on ${BRANCH_PREFIX}/${repo.agentId}, repo=${repo.repoPath}\`);`

Add import:
```ts
import { BRANCH_PREFIX } from "../constants.js";
```

**Step 3: Update cli/synthesize.ts**

Import constants:
```ts
import { ONTOLOGY_DIR } from "../constants.js";
```

- Line 19-21: `function shortPath(path: string): string { return path.replace(new RegExp(\`^${ONTOLOGY_DIR}/\`), ""); }`

**Step 4: Commit**

```
git add src/bootstrap.ts src/cli/mcp.ts src/cli/synthesize.ts
git commit -m "refactor: rename machineId→agentId in bootstrap and CLI"
```

---

### Task 3: Update Tools

**Files:**
- Modify: `src/tools/learn.ts`
- Modify: `src/tools/explore.ts`

**Step 1: Update learn.ts**

Import:
```ts
import { ONTOLOGY_DIR } from "../constants.js";
```

- Line 46: `if (!factPath.startsWith(\`${ONTOLOGY_DIR}/\`)) factPath = \`${ONTOLOGY_DIR}/${factPath}\`;`
- Line 80 (tool description): change `worlds/` → `know/` in the path example: `"path: organize under know/ by domain (e.g. know/projects/myapp/conventions.md)"`

**Step 2: Update explore.ts**

Import:
```ts
import { ONTOLOGY_DIR } from "../constants.js";
```

- Line 9: `path: z.string().optional().default(ONTOLOGY_DIR),`
- Line 44: `log.info(\`explore: path="${input.path ?? ONTOLOGY_DIR}"\`);`
- Line 52: `const path = input.path ?? ONTOLOGY_DIR;`
- Line 113: `while (currentPath !== ONTOLOGY_DIR && currentPath !== ".") {`
- Line 157 (tool description): change `'worlds'` → `'know'`: `"Start with 'know' to see top-level categories."`

**Step 3: Commit**

```
git add src/tools/learn.ts src/tools/explore.ts
git commit -m "refactor: use ONTOLOGY_DIR constant in tools"
```

---

### Task 4: Update Instructions

**Files:**
- Modify: `src/instructions.ts`

**Step 1: Replace all `worlds/` with `know/` in instruction strings**

In `BASE` constant:
- Line 3: `organized by an ontological hierarchy (know/).`
- Line 5: `Your knowledge base operates on an agent-specific branch. Other agents may contribute knowledge that arrives via merges from main.`
- Line 21: `The directory tree under know/ is a hierarchy.`
- Lines 23-37: All path examples `worlds/projects/...` → `know/projects/...`, `worlds/preferences/...` → `know/preferences/...`, `worlds/tools/...` → `know/tools/...`

In `CODE_ADDENDUM`:
- Line 70: `worlds/projects/myapp/identity.md` → `know/projects/myapp/identity.md`

**Step 2: Commit**

```
git add src/instructions.ts
git commit -m "refactor: update instructions with know/ and agent terminology"
```

---

### Task 5: Update TUI Source Files

**Files:**
- Modify: `src/tui/state.ts`
- Modify: `src/tui/App.tsx`
- Modify: `src/tui/RightPanel.tsx`
- Modify: `src/tui/commands.ts`

**Step 1: Update state.ts**

Import:
```ts
import { ONTOLOGY_DIR } from "../constants.js";
```

- Line 66: `currentPath: ONTOLOGY_DIR,`
- Line 70: `statsPath: ONTOLOGY_DIR,`
- Line 235: `if (state.currentPath === ONTOLOGY_DIR) return state;`
- Line 237: `const parentPath = lastSlash > 0 ? state.currentPath.slice(0, lastSlash) : ONTOLOGY_DIR;`

**Step 2: Update App.tsx**

- Line 370: `const result = await exploreHandler(repo, { path: ONTOLOGY_DIR }, { skipSync: true });`

Add import:
```ts
import { ONTOLOGY_DIR } from "../constants.js";
```

**Step 3: Update RightPanel.tsx**

Rename local variable `worlds` → `domains` (line 375):
```ts
const domains = summaryChildren?.filter((c) => c.type === "world") ?? [];
```

Update references to this variable (lines 429, 436).

**Step 4: Update commands.ts**

Import:
```ts
import { ONTOLOGY_DIR } from "../constants.js";
```

- Line 103: `const path = arg ?? ONTOLOGY_DIR;`

**Step 5: Commit**

```
git add src/tui/state.ts src/tui/App.tsx src/tui/RightPanel.tsx src/tui/commands.ts
git commit -m "refactor: use ONTOLOGY_DIR constant in TUI"
```

---

### Task 6: Update All Test Files

**Files:**
- Modify: `src/git.test.ts`
- Modify: `src/git-branches.test.ts`
- Modify: `src/integration.test.ts`
- Modify: `src/fact-ops.test.ts`
- Modify: `src/search-index.test.ts`
- Modify: `src/cluster.test.ts`
- Modify: `src/recipe.test.ts`
- Modify: `src/synthesize.test.ts`
- Modify: `src/synthesize-e2e.test.ts`
- Modify: `src/tui/state.test.ts`
- Modify: `src/tui/RightPanel.test.ts`
- Modify: `src/tui/refs.test.ts`
- Modify: `src/tools/learn.test.ts`
- Modify: `src/tools/explore.test.ts`
- Modify: `src/tools/query.test.ts`
- Modify: `src/tools/update.test.ts`
- Modify: `src/tools/forget.test.ts`
- Modify: `src/tools/why.test.ts`
- Modify: `src/e2e/helpers.ts`
- Modify: `src/e2e/git-ops.e2e.test.ts`
- Modify: `src/e2e/bootstrap.e2e.test.ts`
- Modify: `src/e2e/synthesize.e2e.test.ts`
- Modify: `src/e2e/search-index.e2e.test.ts`
- Modify: `src/e2e/mcp-tools.e2e.test.ts`
- Modify: `src/e2e/tui-state.e2e.test.ts`
- Modify: `src/e2e/recipe-instructions.e2e.test.ts`

**Step 1: Mechanical replacements across all test files**

Apply these replacements in all test files:
- `"worlds/` → `"know/` (path string literals)
- `"worlds"` → `"know"` (bare directory references)
- `"worlds.md"` → `"know.md"` (root manifest)
- `"test-machine"` → `"test-agent"` (test agent IDs)
- `"machine/test-machine"` → `"agent/test-agent"` (branch names)
- `"machine/machine-a"` → `"agent/agent-a"` (multi-agent tests)
- `"machine/machine-b"` → `"agent/agent-b"`
- `"machine/my-machine"` → `"agent/my-agent"`
- `"machine/test-custom-machine"` → `"agent/test-custom-agent"`
- `"machine/test"` → `"agent/test"` (branch checkout tests)
- `"test-custom-machine"` → `"test-custom-agent"`
- `"my-machine"` → `"my-agent"`
- `"machine-a"` → `"agent-a"`
- `"machine-b"` → `"agent-b"`
- `machineId` → `agentId` (variable references in e2e tests)
- `KNOMIT_MACHINE_ID` → `KNOMIT_AGENT_ID`

In `src/e2e/helpers.ts`:
- Line 18: `export async function createTestEnv(agentId = "test-agent"): Promise<TestEnv> {`
- Line 24: `const repo = new GitRepo(repoPath, agentId);`

In `src/git.test.ts`:
- Line 36: Update test name: `"creates a new repo with know.md and agent branch"`
- Line 39 comment: `// Should be on agent branch`

In `src/tui/state.test.ts`:
- Line 4: Update test name: `"initial state starts at know root with breadcrumb selected"`
- Line 172: Update test name containing "worlds": `"NAVIGATE_DOWN in explorer auto-selects facts but shows stats for domains"`

**Step 2: Run all tests**

Run: `bun test src/`
Expected: all pass

**Step 3: Commit**

```
git add src/
git commit -m "test: update all test fixtures for worlds→know and machine→agent renames"
```

---

### Task 7: Update README

**Files:**
- Modify: `README.md`

**Step 1: Fix structural inaccuracies**

The CLI uses citty subcommands, not flag-based args. Rewrite the Usage section to accurately reflect the CLI structure:

```markdown
## Usage

### TUI (default)

Run without arguments to browse your knowledge base interactively:

\`\`\`sh
knomit
\`\`\`

Keyboard shortcuts:

| Key | Action |
|-----|--------|
| `↑` `↓` | Navigate |
| `↵` | Open item |
| `←` `⌫` | Go back / exit history |
| `→` | Focus right panel |
| `/` | Search |
| `:` | Command mode |
| `h` | Toggle history |
| `Esc` | Exit history / search |
| `q` | Quit |

### MCP Server

\`\`\`sh
knomit mcp [--profile code|chat|generic]
\`\`\`

Knomit exposes cognitive tools via MCP. The `--profile` flag selects instruction style:

| Profile | Use case |
|---------|----------|
| `code` | Code editors (default) — anchors facts to git commits |
| `chat` | Conversational tools — anchors facts to URLs, documents |
| `generic` | Minimal instructions for any integration |
```

Update Claude Code config example:
```json
{
  "mcpServers": {
    "knomit": {
      "command": "/path/to/knomit",
      "args": ["mcp"]
    }
  }
}
```

Update Claude Desktop config example:
```json
{
  "mcpServers": {
    "knomit": {
      "command": "/path/to/knomit",
      "args": ["mcp", "--profile", "chat"]
    }
  }
}
```

Update Gemini CLI section:
```markdown
#### Gemini CLI / Other tools

Use `knomit mcp` (defaults to `code` profile) or `knomit mcp --profile generic` for minimal instructions.
```

Add Synthesize subcommand section and Reset subcommand section (already partially there but needs `knomit synthesize` not just `knomit synthesize`).

**Step 2: Apply renames throughout README**

- `worlds/` → `know/`
- `machine/<id>` → `agent/<id>`
- `KNOMIT_MACHINE_ID` → `KNOMIT_AGENT_ID`
- Line 5: `Each agent gets its own branch; consensus lives on \`main\`.`
- Line 183: `The directory tree under \`know/\` forms an ontological hierarchy.`
- Line 193: `knomit:blob/abc1234/know/debugging/pool-fix.md`
- Line 207: `| \`KNOMIT_AGENT_ID\` | system hostname | Branch name: \`agent/<id>\` |`

**Step 3: Add global args documentation**

After the Usage heading or in a new section:

```markdown
### Global Options

| Option | Description |
|--------|-------------|
| `--repo <path>` | Path to the git repository (default: `~/.knomit`) |
| `--cache-dir <path>` | Path to the SQLite index and cache (default: `~/.cache/knomit`) |
```

**Step 4: Commit**

```
git add README.md
git commit -m "docs: fix README inaccuracies, update for worlds→know and machine→agent"
```

---

### Task 8: Update Spec and Research Docs

**Files:**
- Modify: `spec/mcp-server.md`
- Modify: `spec/mbekg.md`
- Modify: `spec/use-cases/cascading-invalidation.md`
- Modify: `spec/research/reconciliation-architecture.md`
- Modify: `spec/research/selective-sharing.md`
- Modify: `spec/research/fact-merge.md`
- Modify: `spec/research/merge-cases.md`
- Modify: `spec/research/cross-repo-knowledge-discovery.md`
- Modify: `docs/research/stratified-distillation.md`
- Modify: All `docs/plans/` files that reference `worlds/` or `machine/`

**Step 1: Mechanical replacements across all spec and docs**

Apply these replacements:
- `worlds/` → `know/`
- `worlds.md` → `know.md`
- `machine/<id>` → `agent/<id>` (and variants like `machine/laptop`, `machine/desktop`)
- `machine branch` → `agent branch`
- `machine-specific` → `agent-specific`
- `KNOMIT_MACHINE_ID` → `KNOMIT_AGENT_ID`

Note: Use judgement in research docs — some may discuss the general concept of "machines" (physical hardware) vs the branch naming convention. Only rename references to the branch prefix, not general computing terms.

**Step 2: Commit**

```
git add spec/ docs/
git commit -m "docs: update spec and research docs for worlds→know and machine→agent"
```

---

### Task 9: Final Verification

**Step 1: Run all tests**

Run: `bun test src/`
Expected: all pass

**Step 2: Grep for any remaining `worlds` or `machine` references in source code**

Run: `grep -r "worlds" src/ --include="*.ts" --include="*.tsx" | grep -v node_modules`
Expected: no matches (or only false positives like "world" type discriminator)

Run: `grep -rn '"machine/' src/ --include="*.ts" --include="*.tsx" | grep -v node_modules`
Expected: no matches

Run: `grep -rn 'KNOMIT_MACHINE' src/ --include="*.ts" --include="*.tsx" | grep -v node_modules`
Expected: no matches

Run: `grep -rn 'machineId' src/ --include="*.ts" --include="*.tsx" | grep -v node_modules`
Expected: no matches

**Step 3: Commit if any cleanup was needed**

```
git add -A
git commit -m "chore: final cleanup for worlds→know and machine→agent rename"
```
