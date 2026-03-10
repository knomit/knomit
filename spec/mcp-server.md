# Knomit MCP Server Specification

## 1. Overview

The Knomit MCP server exposes a personal knowledge base to AI agents via the Model Context Protocol. It abstracts all Git operations behind high-level cognitive tools — agents think in terms of learning, querying, and exploring, not branches and commits.

Each agent operates on its own Git branch. Consensus (merging into `main`) is handled externally — by a Librarian agent, CI, or manual merge. The MCP server never writes to `main`.

## 2. Stack

| Layer | Choice |
| --- | --- |
| Language | TypeScript |
| Runtime | Bun |
| MCP SDK | `@modelcontextprotocol/sdk` |
| Git operations | `git` CLI via `Bun.spawn` |
| YAML parsing | `yaml` (npm) |
| Distribution | Compiled binary (`bun build --compile`) + npm package |

### Git Dependency

The server requires a `git` binary for branch, merge, and tag operations. At startup it resolves the git binary path using the following strategy:

1. **System git** — check for `git` in `PATH` (covers most developer machines)
2. **Vendored git** — fall back to a static `git` binary shipped alongside the compiled executable at `<exec_dir>/vendor/git`

This means:

- **Binary releases** bundle a vendored git per platform — truly zero external dependencies.
- **npm / bunx** expects `git` in PATH (npm users are developers — they have git).
- **Docker** includes git in the image.

### Distribution Channels

- **Binary releases** — compiled standalone executables for macOS, Linux, and Windows via GitHub Releases. Each archive includes a vendored static `git` binary for the target platform.
- **npm** — `bunx knomit-mcp` or `npx knomit-mcp` for users who prefer package managers. Requires `git` in PATH.
- **Docker** — for containerized deployments. Git included in the image.

## 3. Configuration

```json
{
  "mcpServers": {
    "knomit": {
      "command": "knomit-mcp",
      "env": {
        "KNOMIT_REPO": "~/knomit-knowledge"
      }
    }
  }
}
```

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `KNOMIT_REPO` | yes | — | Path to the knowledge repository |
| `KNOMIT_AGENT_ID` | no | system hostname | Identifies this agent's branch: `agent/<id>` |

If `KNOMIT_REPO` points to a path that does not exist, the server initializes a new Git repository with a root `know.md` manifest on first run.

## 4. Git Model

### Agent Branch

The MCP server operates exclusively on the branch `agent/<machine_id>`. All reads and writes target this branch.

- `main` is the consensus branch — never written to by the MCP server
- Machine branch is created from `main` (or initial commit) on first run
- Learning moments are marked by tags on the agent branch

### Sync Behavior

Every tool call begins with a sync step:

1. `git fetch origin` (no-op if no remote configured)
2. Check if `origin/main` has commits not yet in the agent branch
3. If yes: `git merge origin/main` into the agent branch
4. If merge conflict: return conflict details to agent, tool does not proceed

After every write operation:

1. `git push origin agent/<id>` (no-op if no remote configured)

### Consensus Flow

The MCP server does not participate in consensus. The expected external flow is:

1. Each agent pushes its branch to the remote
2. An external process (Librarian agent, CI, manual) merges agent branches into `main`
3. The same or another process merges `main` back into each agent branch
4. The MCP server picks up consensus changes via fetch+merge on the next tool call

## 5. Tools

### 5.1 `knomit_learn`

**Purpose:** "I learned something." Creates one or more facts as a learning moment.

**Input:**

```typescript
{
  moment_name: string        // e.g. "alice-music-2025"
  facts: Array<{
    path: string             // e.g. "know/people/alice/alice-likes-rock-music.md"
    domain: string[]         // e.g. ["personal", "music"]
    confidence: number       // 0.0 - 1.0
    sources: number          // independent corroborations
    entities: string[]       // e.g. ["alice", "rock_music"]
    refs?: string[]          // e.g. ["episodic://event_88"]
    title: string            // e.g. "Alice likes rock music"
    body: string             // natural language description
  }>
}
```

**Output:**

```typescript
{
  moment_tag: string         // e.g. "learn/alice-music-2025"
  commits: Array<{
    hash: string             // commit hash (fact identity)
    file: string             // file path
  }>
}
```

**Internal behavior:**

1. Sync
2. For each fact: writes the markdown file with YAML frontmatter, commits (one fact per commit)
3. Tags the last commit: `learn/<moment_name>`
4. Push

### 5.2 `knomit_query`

**Purpose:** "What do we know about X?" Searches facts by entity, domain, or world path.

**Input:**

```typescript
{
  entities?: string[]        // match facts containing ANY of these entities
  domain?: string[]          // match facts tagged with ANY of these domains
  path?: string              // scope search to a world subtree
  min_confidence?: number    // filter by minimum confidence (default: 0.0)
}
```

At least one of `entities`, `domain`, or `path` must be provided.

**Output:**

```typescript
{
  facts: Array<{
    file: string             // file path
    frontmatter: {
      domain: string[]
      confidence: number
      sources: number
      entities: string[]
      refs: string[]
    }
    body: string             // markdown content
    last_modified: string    // ISO timestamp from last commit
    commit: string           // latest commit hash for this file
  }>
}
```

### 5.3 `knomit_why`

**Purpose:** "Why is this true?" Returns the evidence chain for a fact.

**Input:**

```typescript
{
  file: string               // path to the fact file
}
```

**Output:**

```typescript
{
  fact: {
    file: string
    frontmatter: object
    body: string
  }
  learning_moment: {
    tag: string              // e.g. "learn/alice-music-2025"
    date: string             // ISO timestamp of the tag
    siblings: Array<{        // other facts committed in the same moment
      file: string
      title: string
      commit: string
    }>
  }
  refs: string[]             // external refs from the fact's frontmatter
  history: Array<{           // how this fact evolved over time
    commit: string
    date: string
    message: string
  }>
}
```

### 5.4 `knomit_update`

**Purpose:** "My understanding changed." Updates an existing fact.

**Input:**

```typescript
{
  file: string               // path to existing fact file
  moment_name: string        // learning moment name for this update
  updates: {
    confidence?: number      // new confidence value
    sources?: number         // new sources count
    body?: string            // new body text (replaces existing)
    title?: string           // new title (replaces existing)
    refs?: string[]          // refs to append to existing list
    domain?: string[]        // replaces domain list
    entities?: string[]      // replaces entities list
  }
}
```

**Output:**

```typescript
{
  commit: string             // commit hash of the update
  moment_tag: string         // e.g. "learn/alice-music-reinforced"
}
```

**Internal behavior:**

1. Sync
2. Reads existing file, merges updates into frontmatter, replaces body/title if provided
3. Commits the updated file
4. Tags: `learn/<moment_name>`
5. Push

### 5.5 `knomit_explore`

**Purpose:** "What's in this world?" Navigates the ontology tree.

**Input:**

```typescript
{
  path?: string              // world path (default: root "know/")
}
```

**Output:**

```typescript
{
  manifest: {                // the manifest for this level (sibling .md file)
    file: string
    frontmatter: object
    body: string
  } | null
  children: Array<{
    name: string             // e.g. "london"
    type: "world" | "fact"   // "world" if folder exists, "fact" if leaf file
    summary?: string         // first line of body if fact, manifest title if world
  }>
  inherited_facts: Array<{   // facts from parent levels that apply here
    file: string
    title: string
    confidence: number
    from_level: string       // which parent directory
  }>
}
```

## 6. Resource: Agent Instructions

The server exposes an MCP resource that agents load at session start.

**URI:** `knomit://instructions`

**Content:**

```
You have access to Knomit, a persistent knowledge base that survives
across sessions. It stores structured facts as markdown files in a
Git repository, organized by an ontological hierarchy (know/).

Your knowledge base operates on a agent-specific branch. Other
machines may contribute knowledge that arrives via merges from main.
If a merge conflict occurs, you will be notified and should resolve
it using knomit_update.

AT SESSION START:
- Call knomit_query with relevant entities or domains to load context
  from previous sessions before responding to the user.

DURING CONVERSATION:
- When the user states a preference, makes a decision, or you jointly
  arrive at a conclusion — call knomit_learn to persist it.
- When you need deeper context on a fact — call knomit_why.
- When a previous fact is reinforced or contradicted — call knomit_update.

AT SESSION END:
- Review what was decided or learned during this session.
- Call knomit_learn for anything worth remembering.

GUIDELINES:
- Not everything needs to be saved. Persist decisions, preferences,
  architectural choices, and conclusions — not transient discussion.
- Use the ontology (know/) to organize facts by where they belong,
  not just what they're about.
- When querying, start broad (domain or entity) then narrow down.
```

## 7. Error Handling

| Scenario | Behavior |
| --- | --- |
| Repo path doesn't exist | Initialize new repo with root manifest, create agent branch |
| No remote configured | Sync and push are no-ops, works fully offline |
| Remote unreachable | Log warning, proceed with local state |
| Merge conflict from `origin/main` | Return conflict details to agent, tool does not proceed |
| File not found (query/why) | Return empty result, not an error |
| Invalid YAML in existing file | Return parse error with file path and line number |
| Git binary not found | Fail at startup with clear error message |
| Machine branch doesn't exist | Create from current `main` or initial commit |
| Tag name already exists | Append timestamp suffix: `learn/name-<unix_timestamp>` |
