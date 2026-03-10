# Rename worlds→know, machine→agent

**Goal:** Replace the `worlds/` ontology directory with `know/`, the `machine/` branch prefix with `agent/`, and introduce constants so these names are defined once. Also review and fix README inaccuracies.

## Constants

Add to `src/constants.ts`:

```ts
export const ONTOLOGY_DIR = "know";
export const ONTOLOGY_ROOT = "know.md";
export const BRANCH_PREFIX = "agent";
```

All code references use these constants instead of string literals.

## Renames

### worlds → know

- Directory prefix: `worlds/` → `know/`
- Root manifest: `worlds.md` → `know.md`
- All path literals in source, tests, instructions, specs, and docs

### machine → agent

- Branch prefix: `machine/<id>` → `agent/<id>`
- Env var: `KNOMIT_MACHINE_ID` → `KNOMIT_AGENT_ID` (clean break, no fallback)
- Property/variable: `machineId` → `agentId` throughout
- Comments and log messages

## README Fixes

Beyond the renames, fix these inaccuracies:

- `--mcp` described as an arg but is actually a subcommand (`knomit mcp`)
- Missing `--repo` and `--cache-dir` global args documentation
- Missing `--profile` arg for mcp subcommand
- Missing keyboard shortcuts: backspace/left-arrow (go back), escape (exit history/search)
- Missing subcommands in usage section (synthesize, reset are documented but not as subcommands)
- Update env var table with `KNOMIT_AGENT_ID`

## What Does NOT Change

- `knomit:` URI scheme format (paths naturally use new directory name)
- Git tag format `learn/<name>`
- Repo path `~/.knomit`
- Tool names (`knomit_learn`, etc.)
