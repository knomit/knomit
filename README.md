# knomit

Git-backed knowledge base for AI agents. Knowledge + commit.

Knomit stores structured facts as markdown files in a Git repository, organized by an ontological hierarchy. Each machine gets its own branch; consensus lives on `main`.

## Building

Requires [Bun](https://bun.sh).

```sh
cd src
bun install
bun build --compile index.ts --outfile ../dist/knomit
```

## Usage

### MCP Server

Knomit supports different instruction profiles via `--mcp[=profile]`:

| Profile | Use case |
|---------|----------|
| `code` | Code editors (default) — anchors facts to git commits |
| `chat` | Conversational tools — anchors facts to URLs, documents |
| `generic` | Minimal instructions for any integration |

#### Claude Code

Add to your project's `.mcp.json` (or `~/.claude/mcp.json` for global):

```json
{
  "mcpServers": {
    "knomit": {
      "command": "/path/to/knomit",
      "args": ["--mcp"]
    }
  }
}
```

Knomit's tool descriptions carry all the behavioral guidance the model needs — no `CLAUDE.md` setup required. The model will automatically query for context and learn decisions as they arise.

#### Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "knomit": {
      "command": "/path/to/knomit",
      "args": ["--mcp=chat"]
    }
  }
}
```

Knomit works automatically via tool descriptions — no manual activation needed. Optionally use the **knomit-save** prompt at the end of a conversation to flush any remaining learnings.

#### Gemini CLI / Other tools

Use `--mcp` (defaults to `code` profile) or `--mcp=generic` for minimal instructions. Configure according to your tool's MCP server documentation.

### TUI

Run without flags to browse your knowledge base interactively:

```sh
knomit
```

Keyboard shortcuts:

| Key | Action |
|-----|--------|
| `↑` `↓` | Navigate |
| `↵` | Open item |
| `←` `→` | Switch panels |
| `/` | Search |
| `:` | Command mode |
| `h` | Toggle history |
| `q` | Quit |

### Synthesize

Automated knowledge base maintenance — prune stale/duplicate facts and distill higher-order insights using an LLM.

```sh
knomit synthesize                      # default: prune + distill on changes since last run
knomit synthesize --recipe cve-review  # run a specific recipe
knomit synthesize --all                # run all recipes in .knomit/synthesize/
knomit synthesize --verbose            # show per-fact decisions and reasons
```

#### LLM Configuration

Set the model and API key via environment variables:

| Provider   | Variables                                                                                                |
|------------|----------------------------------------------------------------------------------------------------------|
| Anthropic  | `KNOMIT_LLM_MODEL=claude-sonnet-4-6` `ANTHROPIC_API_KEY=...`                                             |
| Gemini     | `KNOMIT_LLM_MODEL=gemini-2.0-flash` `GOOGLE_AI_API_KEY=...`                                              |
| Bedrock    | `KNOMIT_LLM_MODEL=us.anthropic.claude-sonnet-4-6-v1` `AWS_ACCESS_KEY_ID=...` `AWS_SECRET_ACCESS_KEY=...` |
| Claude CLI | `KNOMIT_LLM_PROVIDER=claude-cli` — uses the `claude` CLI (no API key needed, works with Anthropic Max)   |
| Gemini CLI | `KNOMIT_LLM_PROVIDER=gemini-cli` — uses the `gemini` CLI (no API key needed, works with Google AI Pro)   |

The default model is `claude-sonnet-4-6` (Anthropic). The provider is auto-detected from the model name for API providers. CLI providers must be set explicitly via `KNOMIT_LLM_PROVIDER`.

The CLI adapters pass `--model` to the underlying CLI tool, so `KNOMIT_LLM_MODEL` works with all providers.

#### Recipes

Recipes are YAML files in `<repo>/.knomit/synthesize/`. Example:

```yaml
name: cve-review
prompt: "Review security CVEs for staleness and patterns"
scope:
  domain: [security]
  entities: [libfoo]
auto_merge: false
steps:
  - mode: prune
    prompt: "Find stale or superseded CVEs"
  - mode: distill
    prompt: "Identify vulnerability patterns"
```

| Field | Description |
|-------|-------------|
| `name` | Recipe identifier (used for branch names and logging) |
| `prompt` | Global context passed to every step |
| `scope` | Filter facts by `domain`, `entities`, `search` queries, or `path` prefix. Omit for auto-discovery (changes since last run). |
| `auto_merge` | `true` merges results back automatically; `false` pushes a branch for review |
| `steps` | Pipeline of `prune` and/or `distill` steps. Each can override `model`. |

Running `knomit synthesize` with no flags uses a built-in default recipe: prune + distill on all facts changed since the last synthesis run, with auto-merge enabled.

### Reset

Wipe the git repo and search index for a clean start:

```sh
knomit reset
```

## MCP Prompts

| Prompt | Description |
|--------|-------------|
| `knomit-save` | End-of-session review. Prompts the agent to persist decisions, preferences, and conclusions from the conversation. |

## MCP Tools

| Tool | Description |
|------|-------------|
| `knomit_learn` | Persist facts — preferences, decisions, conclusions |
| `knomit_query` | Search by free text, entity, domain, or path |
| `knomit_explore` | Browse the ontology hierarchy |
| `knomit_why` | Explain a fact's provenance and learning moment |
| `knomit_update` | Revise an existing fact (confidence, body, refs) |
| `knomit_forget` | Remove a fact (git history retains provenance) |

## How it works

Facts are markdown files with YAML frontmatter:

```markdown
---
confidence: 0.8
domain: [music]
entities: [alice]
sources: 1
refs:
  - knomit://github.com/org/repo/blob/abc1234/src/preferences.ts
---
Alice prefers rock music over jazz.
```

The directory tree under `worlds/` forms an ontological hierarchy. Facts placed at higher levels apply to everything below them — a fact at `worlds/earth/` is inherited by `worlds/earth/uk/london/`.

Each learning moment is an atomic git commit tagged with `learn/<moment-name>`, giving full provenance tracking.

### Refs and the `knomit:` URI scheme

Refs anchor facts to their source material using the `knomit:` URI scheme:

| Form | Meaning | Example |
|------|---------|---------|
| Relative (no authority) | Current knowledge base | `knomit:blob/abc1234/worlds/debugging/pool-fix.md` |
| Absolute (with host) | External repo | `knomit://github.com/org/repo/blob/abc1234/src/main.ts` |
| Plain URL | Any web resource | `https://example.com/doc` |

Relative refs (`knomit:blob/...`) always refer to the local repo. When a remote is added, you can immediately distinguish local refs from external ones. The format mirrors GitHub blob URLs but uses the `knomit:` scheme.

Synthesize automatically resolves file-path refs to `knomit:blob/<commit>/<path>` URIs.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `KNOMIT_REPO` | `~/.knomit` | Path to the git repository |
| `KNOMIT_CACHE_DIR` | `~/.cache/knomit` | Path to the SQLite index and model cache |
| `KNOMIT_MACHINE_ID` | system hostname | Branch name: `machine/<id>` |
| `KNOMIT_EMBEDDINGS` | `true` | Vector similarity search (`0` or `false` to disable) |
| `KNOMIT_POLL_INTERVAL` | `5000` | TUI remote poll interval in milliseconds |
| `KNOMIT_LLM_MODEL` | `claude-sonnet-4-6` | Model name for synthesis LLM calls |
| `KNOMIT_LLM_PROVIDER` | auto-detected | LLM provider: `anthropic`, `gemini`, or `bedrock` |
| `ANTHROPIC_API_KEY` | — | API key for Anthropic (required when provider is `anthropic`) |
| `GOOGLE_AI_API_KEY` | — | API key for Gemini (required when provider is `gemini`) |
| `AWS_ACCESS_KEY_ID` | — | AWS access key (required when provider is `bedrock`) |
| `AWS_SECRET_ACCESS_KEY` | — | AWS secret key (required when provider is `bedrock`) |
| `AWS_REGION` | `us-east-1` | AWS region for Bedrock |

## Manual git operations

Knomit is tolerant of manually committed files — malformed facts are silently skipped by search and explore. However, if you run `git reset`, `git rebase`, or `git commit --amend`, the search index may reference a commit that no longer exists. Rebuild the search index with `:rebuild` in the TUI, or `knomit reset` to wipe everything and start fresh.

## Development

```sh
cd src
bun test              # run tests
bun index.ts          # run TUI in dev mode
bun ../scripts/seed.ts    # seed test data (20 facts across 5 learning moments)
```
