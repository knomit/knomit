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

### Reset

Wipe the git repo and search index for a clean start:

```sh
knomit --reset
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
refs: []
---
Alice prefers rock music over jazz.
```

The directory tree under `worlds/` forms an ontological hierarchy. Facts placed at higher levels apply to everything below them — a fact at `worlds/earth/` is inherited by `worlds/earth/uk/london/`.

Each learning moment is an atomic git commit tagged with `learn/<moment-name>`, giving full provenance tracking.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `KNOMIT_REPO` | `~/.knomit` | Path to the git repository |
| `KNOMIT_CACHE_DIR` | `~/.cache/knomit` | Path to the SQLite index and model cache |
| `KNOMIT_MACHINE_ID` | system hostname | Branch name: `machine/<id>` |
| `KNOMIT_EMBEDDINGS` | `true` | Vector similarity search (`0` or `false` to disable) |
| `KNOMIT_POLL_INTERVAL` | `5000` | TUI remote poll interval in milliseconds |

## Development

```sh
cd src
bun test          # run tests
bun index.ts      # run TUI in dev mode
```
