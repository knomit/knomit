# Knomit Tool Specifications — Go Port Reference

This document specifies every MCP tool, the MCP prompt, and the synthesize CLI pipeline for the Go rewrite of knomit. A Go developer should be able to implement the full system from this document alone, without consulting the TypeScript source.

---

## System Architecture Overview

Knomit is a Git-backed knowledge base exposed as an MCP server. Facts are YAML-frontmatter Markdown files stored under the `know/` directory tree. The server operates on a branch named `agent/<hostname>` (configurable); `main` is a read-only consensus branch the server never writes to.

### Fact file format

Every fact file is a UTF-8 Markdown file with a YAML frontmatter block:

```
---
domain:
  - string
  - ...
confidence: 0.0
sources: 1
entities:
  - string
  - ...
refs:
  - string
  - ...
---
# Fact Title

Body text in Markdown.
```

Field semantics:

| Field | Type | Description |
|---|---|---|
| `domain` | `[]string` | Topic tags (e.g. "architecture", "testing") |
| `confidence` | `float64` | 0.0–1.0 — how certain the fact is |
| `sources` | `int` | How many independent sessions confirmed it |
| `entities` | `[]string` | Named entities (people, projects, tools) |
| `refs` | `[]string` | External anchors: URLs, `knomit:` URIs, file paths |

The title is extracted from the first `# Heading` line. The body is everything after that heading. Serialization: YAML frontmatter delimited by `---` lines, then `# Title`, blank line, body, trailing newline.

### Path conventions

- Ontology directory: `know/`
- Ontology root manifest: `know.md`
- Branch prefix: `agent`
- Default branch: `agent/<hostname>`
- Fact paths are relative to the repo root (e.g. `know/projects/myapp/conventions.md`)
- When accepting a path from the caller: if it does not start with `know/`, prepend `know/`; if it does not end with `.md`, append `.md`

### Git conventions

- Repo is configured with `user.email = knomit@local`, `user.name = knomit`, `commit.gpgsign = false`
- Commit messages for learn: `learn: <title>` (optional body paragraph with reason)
- Commit messages for update: `update: <title>` (optional body paragraph with reason)
- Commit messages for forget: `forget(<moment_name>): <file>` (optional body paragraph with reason)
- Learning moment tags: `learn/<sanitized-name>` — sanitize = replace any char not in `[a-zA-Z0-9._/-]` with `-`
- Forget moment tags: `forget/<sanitized-name>` — same sanitization rule
- If a tag already exists (name collision), append `-<unix-seconds>` and create the suffixed tag instead
- Short commit hashes are 7 characters (`git rev-parse --short HEAD`)

### Sync operation

`sync()` is called at the start of every tool handler. Steps:

1. Check whether a remote named `origin` exists (`git remote get-url origin`). If not, return immediately with `synced=false` (offline mode — proceed anyway).
2. `git fetch origin` — if fetch fails, log a warning and return `synced=false` (proceed offline).
3. Check whether `origin/main` exists (`git rev-parse --verify origin/main`). If not, return `synced=false`.
4. Count commits in `origin/main` not in the current agent branch: `git rev-list --count <branch>..origin/main`. If 0, return `synced=false`.
5. `git merge origin/main --no-edit`. If the merge fails (exit code non-zero):
   - `git merge --abort`
   - Parse the stderr output for lines containing `CONFLICT` to extract conflicting file paths
   - Return `synced=false` with `conflict={files, message}`
6. On success, return `synced=true`.

When a conflict is returned, all tool handlers must immediately return an error:

```
Merge conflict from origin/main. Conflicting files: <comma-separated files>. Resolve with knomit_update then retry.
```

### Push operation

`push()` is called after every write operation. Steps:

1. Check whether `origin` exists. If not, return silently.
2. `git push origin <branch-name>`. If it fails, log a warning and return (non-fatal).

### Git lock contention

All `git` subprocess calls should retry up to 5 times on lock contention. Detect lock contention from stderr: presence of `"could not lock"` or `".lock"`. Backoff: `50ms * 2^attempt + random(0, 50ms)`.

---

## Common Subsystems

### Path validation

Before any file read or write, validate that resolving the path against the repo root does not escape the repo root (path traversal protection). Return an error if the resolved absolute path does not start with the resolved repo root.

### Search index (SQLite)

The search index is an optional SQLite database stored at `<cache-dir>/index.db`. When unavailable, tools fall back to `git grep` and direct file reads.

#### Schema

```sql
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE synthesis_log (
    recipe          TEXT    PRIMARY KEY,
    last_commit     TEXT    NOT NULL,
    run_at          TEXT    NOT NULL,
    facts_processed INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE facts (
    path        TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    body        TEXT NOT NULL,
    domain      TEXT NOT NULL,   -- JSON array
    entities    TEXT NOT NULL,   -- JSON array
    confidence  REAL NOT NULL,
    sources     INTEGER NOT NULL,
    refs        TEXT NOT NULL,   -- JSON array
    commit_hash TEXT NOT NULL
);

-- FTS5 content-sync'd virtual table
CREATE VIRTUAL TABLE facts_fts USING fts5(
    title, body, entities, domain,
    content='facts',
    content_rowid='rowid'
);

-- Vector table (only when embeddings enabled, requires sqlite-vec extension)
CREATE VIRTUAL TABLE facts_vec USING vec0(
    path      TEXT PRIMARY KEY,
    embedding FLOAT[384]
);
```

Database pragmas: `PRAGMA journal_mode = WAL`, `PRAGMA busy_timeout = 5000`.

On macOS, the system SQLite does not support `loadExtension`. Replace it with Homebrew's SQLite (`/opt/homebrew/opt/sqlite/lib/libsqlite3.dylib` on Apple Silicon, `/usr/local/opt/sqlite/lib/libsqlite3.dylib` on Intel) before opening the database. Embeddings require the `sqlite-vec` extension loaded at runtime.

#### Upsert

To upsert a fact (must be done inside a transaction):

1. Look up existing row for `path` — save `rowid, title, body, entities, domain`.
2. If it exists: emit FTS5 delete command: `INSERT INTO facts_fts(facts_fts, rowid, title, body, entities, domain) VALUES ('delete', ?, ?, ?, ?, ?)`.
3. `INSERT OR REPLACE INTO facts ...` (all fields).
4. Query the new `rowid` for the path.
5. `INSERT INTO facts_fts(rowid, title, body, entities, domain) VALUES (?, ?, ?, ?, ?)`.
6. Commit.
7. Outside the transaction: if embeddings enabled, compute embedding text as `"<title> <body> <entities joined by space> <domain joined by space>"`, embed it, then `DELETE FROM facts_vec WHERE path = ?` followed by `INSERT INTO facts_vec (path, embedding) VALUES (?, ?)`.

#### Remove

1. Look up existing row.
2. If it exists: in a transaction, emit FTS5 delete command, then `DELETE FROM facts WHERE path = ?`.
3. Outside transaction: `DELETE FROM facts_vec WHERE path = ?` (ignore errors if table absent).

#### Sync (incremental update)

The `meta` table stores `last_commit`. On each sync call:

1. Read `last_commit` from meta. If absent, do a full rebuild instead.
2. Get current HEAD commit.
3. If `last_commit == HEAD`, no-op (return false — no changes).
4. Compute `git diff --name-status <last_commit> HEAD -- know/`. Parse output into added/modified/deleted file lists.
5. For added/modified `.md` files: read file content, parse, upsert. Commit hash = `git log -1 --format=%H -- <path>` (fallback to HEAD if not found).
6. For deleted `.md` files: remove from index.
7. Update `meta.last_commit = HEAD`.

#### Rebuild (full)

Truncate `facts`, emit `INSERT INTO facts_fts(facts_fts) VALUES ('delete-all')`, optionally truncate `facts_vec`. Walk `know/` recursively, index every `.md` file. Set `last_commit = HEAD`.

#### Reindex

Same as rebuild — truncate and re-walk from current HEAD. Used at the start of each synthesize step.

#### Search

Input: `{ text?, entities?, domain?, path?, min_confidence?, limit? }` (limit default 20).

1. If `text` is set: tokenize by whitespace, quote each token with FTS5 double-quote escaping (`"` → `""`), join with spaces. Run:
   ```sql
   SELECT f.*, fts.rank as score
   FROM facts_fts fts
   JOIN facts f ON f.rowid = fts.rowid
   WHERE facts_fts MATCH ?
   ORDER BY fts.rank
   LIMIT ?  -- limit * 5
   ```
   BM25 scores are negative; closer to zero means better match.
2. If `text` is absent: `SELECT *, 0 as score FROM facts LIMIT ?` (limit * 5).
3. Post-filter in memory:
   - `entities`: keep rows where any element of `result.entities` appears in the query `entities` list.
   - `domain`: same intersection logic for `domain`.
   - `path`: keep rows where `result.path` starts with the query `path` string.
   - `min_confidence`: keep rows where `confidence >= min_confidence`.
4. If embeddings enabled and `text` is set: hybrid re-ranking.
   - Embed the query text.
   - `SELECT path, distance FROM facts_vec WHERE embedding MATCH ? ORDER BY distance LIMIT ?` (limit * 5).
   - Build a map `path → distance`.
   - Normalize BM25 scores to [0, 1]: `norm_bm25 = (max_abs_bm25 - abs(score)) / range` (where range = max - min of absolute values).
   - For each result: `score = 0.6 * norm_bm25 + 0.4 * (1 - vec_distance)`.
   - Also add vector-only results not in FTS results, provided their distance < 0.8, with score = `0.4 * (1 - distance)`.
   - Sort descending by score.
5. Truncate to `limit`.
6. Normalize final scores to [0, 100]:
   - If all scores are non-positive (pure BM25 case): `score = round((max_abs - abs(score)) / range * 100)`.
   - If any score is positive (hybrid case): `score = round(max(0, score) / max_score * 100)`.
7. Discard results with score < 10.
8. Return results with all fields plus the normalized integer score.

---

## Tool 1: `knomit_learn`

### Description (LLM-visible)

Persist knowledge to a Git-backed knowledge base. Call this AUTOMATICALLY whenever the user states a preference, makes a decision, or you jointly arrive at a conclusion worth remembering across sessions. Creates one or more facts as a learning moment.

WHEN TO CALL: Decisions, preferences, architectural choices, debugging insights, conclusions. NOT transient discussion, obvious facts, or things easily re-derived.

FACT QUALITY:
- path: organize under know/ by domain (e.g. know/projects/myapp/conventions.md). Durable facts at higher levels, ephemeral facts in sub-directories.
- title: concise and descriptive — this is the primary search surface.
- body: include reasoning, not just conclusions.
- confidence: 0.9+ for explicit user statements, 0.7-0.8 for inferred conclusions, 0.5-0.6 for tentative observations.
- entities: people, projects, tools — anything worth querying by.
- domain: topic tags like "architecture", "testing", "workflow".
- refs: anchor to source using knomit: URIs. For code facts: "knomit://github.com/org/repo/blob/<commit>/<path>". For knowledge base cross-refs: "knomit:blob/<commit>/<path>". Also plain URLs, issue numbers.
- sources: set to 1 for new facts; increment via knomit_update when multiple sessions confirm the same thing.

Before learning, query first to avoid duplicating an existing fact — use knomit_update instead if one exists.

### Input schema

```
moment_name  string    required  Name for this learning moment. Becomes tag "learn/<sanitized>".
facts        []Fact    required  One or more facts to persist.
```

Each `Fact` object:

```
path        string    required  Relative path. Auto-prefixed with "know/" if missing. Auto-suffixed with ".md" if missing.
domain      []string  required  Topic tags.
confidence  float64   required  0.0–1.0
sources     int       required  Number of confirming sessions.
entities    []string  required  Named entities.
refs        []string  optional  Default []. External anchors.
title       string    required  Concise descriptive title.
body        string    required  Markdown body with reasoning.
```

Validation: `confidence` must be in [0, 1]. `sources` must be a non-negative integer.

### Behavior

1. Call `sync()`. If conflict, return error.
2. For each fact in `facts` (in order):
   a. Normalize path: if not prefixed with `know/`, prepend `know/`; if not suffixed with `.md`, append `.md`.
   b. Serialize the fact to Markdown with YAML frontmatter.
   c. Write file to disk (create parent directories as needed).
   d. `git add <path>`.
   e. `git commit -m "learn: <title>"`.
   f. Record returned short commit hash and normalized path.
   g. Upsert into search index (if available).
3. Create tag: sanitize `moment_name`, call `git tag learn/<sanitized>`. If tag exists, use suffixed form.
4. Call `push()`.
5. Return result.

### Git operations

- `sync()` at entry (fetch + merge origin/main)
- One `git add + git commit` per fact
- One `git tag` for the learning moment
- `push()` at exit

### Search index interactions

After each fact commit: upsert the fact with its commit hash.

### Output schema

```json
{
  "moment_tag": "learn/my-moment",
  "commits": [
    { "file": "know/path/to/fact.md", "hash": "abc1234" }
  ]
}
```

### Error cases

| Condition | Error |
|---|---|
| Sync conflict | `"Merge conflict from origin/main. Conflicting files: <files>. Resolve with knomit_update then retry."` |
| Path escapes repo | `"Path escapes repository: <path>"` |
| Git commit fails | Propagate git error |

---

## Tool 2: `knomit_query`

### Description (LLM-visible)

Search the persistent knowledge base. Query by free text, entity names, domain tags, or path prefix.

USE PROACTIVELY: When starting work on a project or topic, query by project name, entity, or domain to load relevant context from previous sessions before responding. Start broad, then narrow by path if needed. Check refs against current state — a fact anchored to an old commit may be outdated.

### Input schema

```
text           string    optional  Full-text search query.
entities       []string  optional  Filter: fact must contain at least one of these entities.
domain         []string  optional  Filter: fact must contain at least one of these domains.
path           string    optional  Path prefix filter (e.g. "know/projects/myapp").
min_confidence float64   optional  Default 0. Minimum confidence threshold (inclusive).
```

Validation: at least one of `text`, `entities`, `domain`, `path` must be non-empty/non-nil. Return error if all are absent.

### Behavior (with search index)

1. Call `sync()`. If conflict, return error.
2. Call `searchIndex.sync(repo)` — incremental update to reflect any commits since last sync.
3. Call `searchIndex.search({ text, entities, domain, path, min_confidence, limit: 20 })`.
4. For each result, call `git log --follow --format=%H|%aI|%s|%D -- <file>` to get the most recent log entry (date and hash).
5. Build and return the result.

### Behavior (without search index — fallback)

1. Call `sync()`. If conflict, return error.
2. Validate that at least one filter is provided.
3. Collect candidate files:
   - For each entity in `entities`: `git grep -rl -- <entity> know/` — add all returned paths.
   - For each domain tag in `domain`: `git grep -rl -- <domain> know/` — add all returned paths.
   - For `path`: recursively walk the directory, add all `.md` files.
4. For each candidate `.md` file:
   a. Read and parse the file (skip on error).
   b. Verify entity match: if `entities` specified, at least one must appear in `frontmatter.entities` (not just body text — grep may over-match).
   c. Verify domain match: if `domain` specified, at least one must appear in `frontmatter.domain`.
   d. Verify path prefix: if `path` specified, file path must start with `path`.
   e. Apply `min_confidence` filter.
   f. Run `git log` for last commit date and hash.
   g. Add to results.
5. Return results.

### Git operations

- `sync()` at entry
- `git grep -rl` for entity/domain fallback search
- `git log` per result for date/commit metadata

### Search index interactions

- `searchIndex.sync(repo)` — incremental update before searching
- `searchIndex.search(query)` — run the FTS5/hybrid search

### Output schema

```json
{
  "facts": [
    {
      "file": "know/path/to/fact.md",
      "frontmatter": {
        "domain": ["..."],
        "confidence": 0.9,
        "sources": 1,
        "entities": ["..."],
        "refs": ["..."]
      },
      "body": "Markdown body text",
      "title": "Fact Title",
      "last_modified": "2024-01-15T10:30:00Z",
      "commit": "abc1234567890"
    }
  ]
}
```

`last_modified` is an ISO 8601 timestamp. `commit` is the full commit hash. Both are empty strings if git log fails or returns nothing.

### Error cases

| Condition | Error |
|---|---|
| Sync conflict | Conflict error message |
| No filters provided | `"At least one of text, entities, domain, or path must be provided."` |

---

## Tool 3: `knomit_why`

### Description (LLM-visible)

Explain the provenance of a fact: when it was learned, what learning moment it belongs to, and what sibling facts were learned at the same time.

### Input schema

```
file  string  required  Path to the fact file (e.g. "know/path/to/fact.md").
```

### Behavior

1. Call `sync()`. If conflict, return error.
2. Run `git log --follow --decorate-refs=refs/tags/learn/ --format=%H|%aI|%s|%D -- <file>`. Parse each line: split on `|` into `commit`, `date`, `message`, `decorations`. Extract a `learn/` tag from decorations if present (regex: `tag: learn/([^\s,)]+)`).
3. Walk the log entries newest-first. Once a `learn/` tag is found in an entry's decorations, that tag propagates to all older entries that lack their own tag.
4. If history is empty, return a zero-value result (empty fact, empty learning moment, empty history).
5. Read and parse the fact file from disk.
6. Find the earliest commit (last entry in the ordered list, which is oldest-first by reversing the newest-first log).
7. Run `git tag --contains <earliest-commit>` to get all tags that contain the original commit.
8. Find the first tag starting with `learn/`. This is the learning moment tag.
9. If a learn tag was found, call `commitsBetweenTags(tag)` to find siblings:
   - Get all `learn/` tags and sort them by their commit date (`git log -1 --format=%aI <tag>`).
   - Find the tag's position in the sorted list; the previous tag (if any) forms the start of the range.
   - Run `git log --format=%H|%s --name-only <prevTag>..<tag>` (or just `<tag>` if no previous tag).
   - Parse: lines containing `|` are commit headers; non-empty lines without `|` are file paths touched by the preceding commit.
   - Filter out `input.file` from results.
   - For each sibling: `{ file, title: message with "learn: " prefix stripped, commit }`.
10. Return full result.

### Git operations

- `sync()` at entry
- `git log --follow --decorate-refs=refs/tags/learn/` for file history
- `git tag --contains <commit>` to find the learning moment
- `git log -1 --format=%aI <tag>` per tag to sort them (for `commitsBetweenTags`)
- `git log --format=%H|%s --name-only <range>` for sibling discovery

### Search index interactions

None.

### Output schema

```json
{
  "fact": {
    "file": "know/path/to/fact.md",
    "frontmatter": {
      "domain": ["..."],
      "confidence": 0.9,
      "sources": 1,
      "entities": ["..."],
      "refs": ["..."]
    },
    "title": "Fact Title",
    "body": "Markdown body"
  },
  "learning_moment": {
    "tag": "learn/my-moment",
    "date": "2024-01-15T10:30:00Z",
    "siblings": [
      { "file": "know/other/fact.md", "title": "Other Fact", "commit": "abc1234" }
    ]
  },
  "refs": ["..."],
  "history": [
    { "commit": "abc123", "date": "2024-01-15T10:30:00Z", "message": "learn: Fact Title" }
  ]
}
```

On empty history, all fields are zero values (empty strings, empty arrays, `null` for `learning_moment` is acceptable but prefer `{ tag: "", date: "", siblings: [] }`).

### Error cases

| Condition | Error |
|---|---|
| Sync conflict | Conflict error message |
| File does not exist | Empty result (not an error) — history will be empty |

---

## Tool 4: `knomit_update`

### Description (LLM-visible)

Update an existing fact when a previous belief is reinforced or contradicted. Use this to change confidence, add refs, or correct the body of an existing fact. Prefer this over knomit_learn when a fact on the topic already exists — increment sources when multiple sessions confirm the same thing.

### Input schema

```
file         string   required  Path to the existing fact file.
moment_name  string   required  Name for this update moment. Becomes tag "learn/<sanitized>".
updates      Updates  required  Object with any subset of fields to update.
```

`Updates` object (all fields optional):

```
confidence  float64   optional  New confidence value (replaces existing).
sources     int       optional  New sources count (replaces existing).
body        string    optional  New body text (replaces existing).
title       string    optional  New title (replaces existing).
refs        []string  optional  Additional refs (appended to existing list, not replaced).
domain      []string  optional  New domain list (replaces existing).
entities    []string  optional  New entities list (replaces existing).
```

### Behavior

1. Call `sync()`. If conflict, return error.
2. Check that the file exists. If not, return error.
3. Read and parse the existing fact.
4. Merge updates:
   - `confidence`, `sources`, `body`, `title`, `domain`, `entities`: replace existing value if provided.
   - `refs`: append new values to the existing refs list (do not replace).
5. Serialize the merged fact to Markdown.
6. Write file, `git add`, `git commit -m "update: <title>"` (using the new title if updated, else the existing title).
7. Upsert into search index.
8. Create tag: sanitize `moment_name`, `git tag learn/<sanitized>`.
9. Call `push()`.
10. Return result.

### Git operations

- `sync()` at entry
- `git add + git commit` for the updated file
- `git tag` for the moment
- `push()` at exit

### Search index interactions

Upsert the updated fact after commit.

### Output schema

```json
{
  "commit": "abc1234",
  "moment_tag": "learn/my-moment"
}
```

### Error cases

| Condition | Error |
|---|---|
| Sync conflict | Conflict error message |
| File does not exist | `"File not found: <file>"` |

---

## Tool 5: `knomit_explore`

### Description (LLM-visible)

Browse the knowledge graph hierarchy. Lists worlds (categories) and facts at a given path. Start with 'know' to see top-level categories. Use this to understand what's already stored before learning new facts, or to orient yourself in the ontology.

### Input schema

```
path  string  optional  Default "know". Ontology directory path to explore.
```

### Behavior

1. Call `sync()` unless `skipSync` is set (an internal option for TUI use; external callers always sync). If conflict, return error.
2. Determine manifest path: `<path>.md` (e.g. for `know/projects` the manifest is `know/projects.md`).
3. If the manifest file exists: read and parse it; record `{ file, title, body }`.
4. List directory entries at `path/`:
   - For each directory entry: create a child entry with `type="world"`. Try to read `<path>/<name>.md` (the potential manifest for that subdirectory); if it parses successfully, set `summary = parsed.title`.
   - For each `.md` file entry: create a child entry with `type="fact"`. Try to parse it; if successful, set `summary = parsed.title`.
   - Skip hidden files (names starting with `.`).
5. Collect inherited facts by walking up parent directories:
   - Start at `currentPath = path`.
   - Loop while `currentPath != "know"` and `currentPath != "."`:
     - Set `parentDir = dirname(currentPath)`.
     - If `parentDir == currentPath`, stop (reached filesystem root).
     - List directory entries at `parentDir`.
     - Build a set of directory names at `parentDir`.
     - For each `.md` file in `parentDir`:
       - Compute `nameWithoutExt = filename without ".md"`.
       - If `nameWithoutExt` is in the set of directory names at `parentDir`, this file is a manifest — skip it.
       - Otherwise: read and parse the file; add `{ file: fullPath, title, confidence, from_level: parentDir }` to `inherited_facts`.
     - Set `currentPath = parentDir`.
6. Return result.

### Git operations

- `sync()` at entry (skippable)
- File existence checks and reads via repo abstraction

### Search index interactions

None.

### Output schema

```json
{
  "manifest": {
    "file": "know/projects.md",
    "title": "Projects",
    "body": "Overview of all projects."
  },
  "children": [
    { "name": "myapp", "type": "world", "summary": "MyApp Project" },
    { "name": "conventions.md", "type": "fact", "summary": "Code Conventions" }
  ],
  "inherited_facts": [
    {
      "file": "know/global-rule.md",
      "title": "Global Rule",
      "confidence": 0.95,
      "from_level": "know"
    }
  ]
}
```

`manifest` is `null` if no manifest file exists. `summary` is absent if the file cannot be parsed.

### Error cases

| Condition | Error |
|---|---|
| Sync conflict | Conflict error message |

---

## Tool 6: `knomit_forget`

### Description (LLM-visible)

Remove a fact from the knowledge base. The file is deleted from the repo; git history retains provenance. Use when a fact is no longer true, relevant, or was stored in error.

### Input schema

```
file         string  required  Path to the fact file to delete.
moment_name  string  required  Name for this forget moment. Becomes tag "forget/<sanitized>".
```

### Behavior

1. Call `sync()`. If conflict, return error.
2. Check that the file exists. If not, raise an error (`git rm` of a missing file would fail).
3. `git rm <file>`.
4. `git commit -m "forget(<moment_name>): <file>"`.
5. Remove from search index.
6. Create tag: sanitize `moment_name`, `git tag forget/<sanitized>`. If tag exists, use suffixed form.
7. Call `push()`.
8. Return result.

### Git operations

- `sync()` at entry
- `git rm <file>`
- `git commit`
- `git tag`
- `push()` at exit

### Search index interactions

Remove the entry from the `facts` table and `facts_fts` virtual table.

### Output schema

```json
{
  "file": "know/path/to/fact.md",
  "commit": "abc1234",
  "moment_tag": "forget/my-moment"
}
```

### Error cases

| Condition | Error |
|---|---|
| Sync conflict | Conflict error message |
| File does not exist | `"File not found: <file>"` |

---

## Prompt: `knomit-save`

This is an MCP prompt, not a tool. When the MCP client invokes it, the server returns a single user-role message instructing the LLM to review the conversation and persist relevant facts.

### Prompt name

`knomit-save`

### Prompt description

`"Save decisions, preferences, and conclusions from this conversation."`

### Returned message (role: user)

```
Review our conversation and identify decisions, preferences, architectural choices, or conclusions worth remembering across sessions.

Before persisting, query knomit for existing facts on each topic to avoid duplicates. If a fact already exists and just needs updating, use knomit_update instead of knomit_learn.

For each new fact, call knomit_learn with:
- Confidence: 0.9+ for explicit user statements, 0.7–0.8 for inferred conclusions. Skip anything below 0.6.
- Refs: include source URLs, commit hashes (as origin-url@hash), or file paths when available.
- Entities and domain tags for discoverability.

Do NOT persist: transient discussion, obvious facts, things easily re-derived, or anything already captured.
```

---

## MCP Resource: `knomit://instructions`

The server exposes a single resource at URI `knomit://instructions` with MIME type `text/plain`. The content is the profile-specific instruction text (profile is selected at server startup via `--profile=[code|chat|generic]`; defaults to `code`). Profile selection affects only this resource content — all tool behavior is identical across profiles.

---

## Synthesize Pipeline

The synthesize pipeline is a **CLI subcommand**, not an MCP tool. It is invoked as:

```
knomit synthesize --recipe recipe.yaml [--repo <path>] [--cache-dir <path>]
```

### Recipe format (YAML)

```yaml
name: string              # required; used for branch name and tags; must be non-empty
prompt: string            # optional, default ""; context given to LLM for all steps
scope:                    # optional; if absent = auto-discovery (delta) mode
  domain: [string, ...]   # filter by domain tags; default []
  entities: [string, ...] # filter by entity tags; default []
  search: [string, ...]   # one or more FTS text queries; default []
  path: string            # path prefix filter; default ""
auto_merge: boolean       # default false; if true, merge branch into caller branch
steps:                    # required; at least one step
  - mode: prune|distill   # required
    model: string         # optional; overrides default LLM model for this step
    prompt: string        # optional, default ""; additional step-specific instructions
    max_depth: int        # optional, default 1; RAPTOR recursion depth (distill only)
    umap_dimensions: int  # optional, default 5; UMAP output dimensions (distill only)
    min_cluster_size: int # optional, default 3; minimum HDBSCAN cluster size (distill only)
```

### Main execution flow

```
synthesize(repo, searchIndex, recipe, onProgress?):
```

1. Record `originalBranch = currentBranch()`.
2. Delete stale branch `synthesize/<recipe.name>` if it exists (force-delete, ignore error if absent).
3. Create and checkout branch `synthesize/<recipe.name>`.
4. For each step at index `i`:
   a. Emit progress event `{ phase: "step-start", step: i, totalSteps: len(steps), mode: step.mode }`.
   b. Emit `{ phase: "reindex" }`.
   c. Call `searchIndex.reindex(repo)` — full reindex from current HEAD so this step sees previous step's writes.
   d. Execute the step (prune or distill). Collect summary string.
   e. Append summary to `stepSummaries`.
5. On any step error: checkout `originalBranch`, propagate error.
6. Record synthesis log: `setSynthesisLog(recipe.name, headCommit, len(stepSummaries))`.
7. If `recipe.auto_merge`:
   a. Emit `{ phase: "merge" }`.
   b. Checkout `originalBranch`.
   c. `git merge synthesize/<recipe.name>`.
   d. Delete the synthesis branch.
   e. Return `{ branch, stepSummaries, merged: true }`.
8. If not `auto_merge`:
   a. Emit `{ phase: "push" }`.
   b. `pushBranch("synthesize/<recipe.name>")`.
   c. Checkout `originalBranch`.
   d. Return `{ branch, stepSummaries, merged: false }`.
9. Emit `{ phase: "done", stepSummaries, elapsed_ms }`.

### Fact gathering

**Scope mode** (when `recipe.scope` is defined):

Run one base search with no text query using any non-empty domain/entities/path filters (limit 10,000). Then for each string in `scope.search`, run a text search with those same filters (limit 10,000 each). Deduplicate results by path (use a map). Combine all results into a single slice.

**Delta (auto-discovery) mode** (when `recipe.scope` is absent):

1. Look up `getSynthesisLog(recipe.name)` from the search index.
2. If no log entry (first run): search with no filters (limit 100,000) — return all facts.
3. If log entry exists: call `git diff --name-status <lastCommit> HEAD -- know/`. Collect added and modified `.md` files. Read and parse each file; skip failures. Return the parsed facts.
4. If no changed files, return empty slice (skip the step).

Emit progress event `{ phase: "gather", facts: count, mode: "scope"|"delta", firstRun: bool }` after gathering.

If the gathered slice is empty, skip the step and return `"No facts found in scope."`.

### Fact structure sent to LLM

When sending facts to the LLM (both prune and distill), serialize each fact as:

```json
{
  "file": "know/path/to/fact.md",
  "title": "Fact Title",
  "body": "Body text",
  "domain": ["..."],
  "entities": ["..."],
  "confidence": 0.9,
  "sources": 1
}
```

Note: `refs` is present in the internal struct but omitted from the JSON sent to the LLM (only `file`, `title`, `body`, `domain`, `entities`, `confidence`, `sources` are included in the LLM prompt payload).

### Chunking

Split facts into chunks where each chunk's JSON size (serialized with the above structure) does not exceed 100,000 characters (100 KB). Algorithm:

```
chunks = []
current = []
currentSize = 0
for each fact:
    factSize = len(json(fact))
    if currentSize + factSize > 100_000 and len(current) > 0:
        chunks.append(current)
        current = []
        currentSize = 0
    current.append(fact)
    currentSize += factSize
if len(current) > 0:
    chunks.append(current)
```

### Ref resolution

After the LLM returns refs, resolve local file path refs to `knomit:` URIs:

- If a ref already contains `://` or starts with `knomit:`, pass through unchanged.
- Otherwise: look up the last commit for that path with `git log -1 --format=%H -- <path>`. If found, replace ref with `knomit:blob/<7-char-hash>/<path>`. If not found, leave ref unchanged.

### Prune step

System prompt: `"You are a knowledge base maintenance assistant. Respond only with valid JSON."`

User prompt template:

```
You are reviewing facts in a knowledge base for staleness, redundancy, and duplication.

Context: <recipe.prompt>          (omit line if recipe.prompt is empty)
Instructions: <step.prompt>       (omit line if step.prompt is empty)
Facts to review:
<facts_json>

For each fact, decide:
- KEEP: fact is current and valuable
- FORGET: fact is obsolete, superseded, or no longer true
- UPDATE: fact needs confidence adjusted (provide new value and reason)

Also identify facts that say the same thing and should be merged into a single unified fact.

IMPORTANT — merged fact placement:
- The path MUST be placed in an appropriate ontological location based on the source facts' paths, NOT under "know/synthesized/" or any operational directory.
- If all sources share a common parent directory, place the fact there (e.g. sources from know/projects/webapp/* → know/projects/webapp/combined-name.md).
- If sources span different directories, place the fact at the nearest common ancestor (e.g. sources from know/debugging/* → know/debugging/common-patterns.md).
- Keep paths meaningful: the directory structure IS the ontology. Use descriptive filenames.
- The "refs" field MUST list the source file paths exactly as given (e.g. "know/foo/bar.md") — the system will resolve them to knomit: URIs automatically.

Respond as JSON (no markdown wrapping):
{
  "decisions": [
    { "file": "...", "action": "keep|forget|update", "confidence": 0.X, "reason": "..." }
  ],
  "merges": [
    {
      "sources": ["file1.md", "file2.md"],
      "merged": {
        "path": "know/...",
        "title": "...",
        "body": "...",
        "domain": [...],
        "confidence": 0.X,
        "entities": [...],
        "refs": ["file1.md", "file2.md"]
      }
    }
  ],
  "summary": "one sentence summary of what changed"
}
```

LLM response parsing: strip any Markdown code fences (detect pattern ` ```json ... ``` ` or ` ``` ... ``` `) before JSON parsing. Fields default to empty arrays/string if absent.

**Applying prune decisions** (process decisions first, then merges):

For each decision:
- `keep`: log, emit `detail-keep` event, no file changes.
- `forget`: call `deleteFact(repo, file, "synthesize-<recipeName>", searchIndex, reason)`. Add path to `deletedPaths` set. Emit `detail-forget` event. Log warnings on failure (don't abort).
- `update` (only if `confidence` is provided): call `updateFact(repo, file, {confidence: decision.confidence}, searchIndex, decision.reason)`. Emit `detail-update` event. Log warnings on failure.

For each merge:
- Resolve refs in `merged.refs` using ref resolution.
- Call `commitFact(repo, mergedFact, searchIndex, mergeReason)` where `mergeReason = "Merged <N> facts: <sources joined by ", ">"`. Set `sources = 1` in the merged fact.
- For each source file not already in `deletedPaths`: call `deleteFact(repo, source, "synthesize-<recipeName>", searchIndex, "Subsumed by merged fact: <merged.path>")`. Add to `deletedPaths`.
- Emit `detail-merge` event. Log warnings on failure.

After all decisions and merges, create tag: `git tag learn/synthesize-<recipeName>-prune`.

Emit `{ phase: "apply", mode: "prune", kept, forgotten, updated, merged }` counts.

Return summary string: `"Prune: <forgotten> forgotten, <updated> updated, <merged> merged. <summaries joined>"`.

### Distill step

Requires embeddings to be enabled. If not, return error: `"Distill mode requires embeddings. Enable embeddings in your SearchIndex configuration."`.

System prompt: `"You are a knowledge base synthesis assistant. Respond only with valid JSON."`

User prompt template:

```
You are synthesizing facts in a knowledge base to find patterns and higher-order insights.

Context: <recipe.prompt>          (omit line if recipe.prompt is empty)
Instructions: <step.prompt>       (omit line if step.prompt is empty)
Facts in scope:
<facts_json>

Identify patterns across these facts. Produce:
1. New higher-order facts that capture patterns
2. Which original facts are fully subsumed and can be forgotten

IMPORTANT — synthesized fact placement:
<same placement rules as prune prompt>

Respond as JSON (no markdown wrapping):
{
  "synthesize": [
    {
      "path": "know/...",
      "title": "...",
      "body": "...",
      "domain": [...],
      "confidence": 0.X,
      "entities": [...],
      "refs": ["source-file1.md", "source-file2.md"]
    }
  ],
  "forget": ["file1.md", "file2.md"],
  "summary": "one sentence summary"
}
```

**RAPTOR recursion loop** (outer loop, runs `max_depth` times or until no new clusters form):

For each depth iteration `d` from 0 to `max_depth - 1`:

1. Emit `{ phase: "raptor-depth", depth: d+1, maxDepth: max_depth }`.
2. Get embeddings:
   - On depth 0: retrieve from `searchIndex.getEmbeddings(factPaths)` — reads `facts_vec` table.
   - On depth > 0: use in-memory embeddings computed in the previous iteration's step 7.
3. **Clustering** (see Clustering subsection below). Pass `currentFacts`, `embeddings`, and step options.
4. If no clusters formed (0 clusters), stop the loop.
5. Emit `{ phase: "cluster", clusters: clusterCount, noise: noiseCount }`.
6. For each cluster (iterate over cluster map in order):
   a. If the cluster's JSON size > 100,000 chars, apply chunking within the cluster.
   b. For each chunk/group: emit LLM progress event, build distill prompt, call LLM, parse response, accumulate `synthesized` and `forget` lists and summaries.
7. If `d + 1 < max_depth` and new synthesized facts were produced:
   - For each synthesized fact, embed the text `"<title> <body> <entities joined> <domain joined>"` using the embedder.
   - Store results in a new in-memory `map[path]→vector`.
   - Set `currentFacts = synthesized facts` (with `sources = 1`).
   - Store the new embedding map for the next depth iteration.

**Applying distill results**:

For each synthesized fact:
- Resolve refs.
- Call `commitFact(repo, fact, searchIndex, refsNote)` where `refsNote = "Distilled from: <refs joined>"` if refs non-empty, else `"Distilled from analysis of related facts"`. Set `sources = 1`.
- Emit `detail-learn` event.
- Log warnings on failure.

Deduplicate the `forget` list (remove duplicate paths). For each unique path:
- Call `deleteFact(repo, file, "synthesize-<recipeName>", searchIndex, "Subsumed by higher-order distilled fact")`.
- Emit `detail-distill-forget` event.
- Log warnings on failure.

After all facts are processed, create tag: `git tag learn/synthesize-<recipeName>-distill`.

Emit `{ phase: "apply", mode: "distill", learned, forgotten }`.

Return summary: `"Distill: <learned> learned, <forgotten> forgotten. <summaries joined>"`.

### Clustering

Function signature: `clusterFacts(facts, embeddings, options) → { clusters map[int][]Fact, noise []Fact }`

Options: `umapDimensions` (default 5), `minClusterSize` (default 3), `minSamples` (default = `minClusterSize`).

Steps:

1. Separate facts into `withEmbeddings` (those with a vector in the embeddings map) and `noise` (those without).
2. If `len(withEmbeddings) < minClusterSize`: move all `withEmbeddings` facts to noise. Return `{ clusters: {}, noise }`.
3. **UMAP**: reduce dimensionality.
   - Input: vectors from `withEmbeddings`, as `[][]float64`.
   - Parameters: `nComponents = umapDimensions`, `nNeighbors = min(15, floor(count/2))`, `minDist = 0.1`, `spread = 1.0`.
   - Output: `[][]float64` of shape `[count][umapDimensions]`.
4. **HDBSCAN**: cluster the reduced vectors.
   - Parameters: `minClusterSize`, `minSamples`.
   - Output: `[]int` label per fact. Label `-1` = noise.
5. Group by label. Label `-1` facts go to noise.
6. **FCA-lite validation** (`splitByMetadata`) on each raw cluster:
   - Build an inverted index: for each fact, for each domain tag prefix `"d:<tag>"` and entity tag prefix `"e:<tag>"`, record the fact's index.
   - Use Union-Find: union all facts that share any domain or entity tag.
   - If no tags exist at all among the cluster's facts, treat the entire cluster as one component.
   - Group facts by their Union-Find root.
   - If only one component results, return it regardless of size.
   - If multiple components, discard any smaller than `minClusterSize` (move those facts to noise).
   - Return surviving components.
7. Assign sequential integer keys to surviving groups to form the `clusters` map.
8. Return `{ clusters, noise }`.

### Progress events

The synthesize function emits structured progress events via a callback. All event types:

| Phase | Fields |
|---|---|
| `step-start` | `step int, totalSteps int, mode string` |
| `gather` | `facts int, mode "scope"\|"delta", firstRun bool` |
| `reindex` | _(no extra fields)_ |
| `cluster` | `clusters int, noise int` |
| `raptor-depth` | `depth int, maxDepth int` |
| `llm` | `step int, totalSteps int, mode string, chunk int, totalChunks int, facts int` |
| `llm-stream` | `step int, totalSteps int, bytes int` |
| `llm-done` | `step int, totalSteps int, mode string, elapsed_ms int` |
| `apply` (prune) | `mode "prune", kept int, forgotten int, updated int, merged int` |
| `apply` (distill) | `mode "distill", learned int, forgotten int` |
| `detail-keep` | `path string, reason string` |
| `detail-forget` | `path string, reason string` |
| `detail-update` | `path string, confidence float64, reason string` |
| `detail-merge` | `sources []string, target string, reason string` |
| `detail-learn` | `path string, body string, refs []string` |
| `detail-distill-forget` | `path string` |
| `merge` | _(no extra fields)_ |
| `push` | _(no extra fields)_ |
| `done` | `stepSummaries []string, elapsed_ms int` |

### LLM interface

The LLM adapter must support a single method:

```
complete(systemPrompt string, messages []Message, onChunk func(string)) (string, error)
```

`Message` has `role` (either `"user"` or `"assistant"`) and `content` (string). The adapter streams tokens via `onChunk` (used to emit `llm-stream` progress events). Returns the full response string.

LLM provider and model are resolved from environment variables. When a recipe step specifies a `model` string, the provider is inferred from the model name; otherwise the default provider and model from environment are used.

### Synthesis log

After all steps complete (before `auto_merge` or push), call:

```
setSynthesisLog(recipe.name, headCommit, len(stepSummaries))
```

This records into the `synthesis_log` table: `recipe`, `last_commit = HEAD`, `run_at = now ISO 8601`, `facts_processed = len(stepSummaries)`. Used by auto-discovery mode to find the delta on the next run.

---

## Cross-cutting Concerns

### Error handling policy

- `sync()` conflicts: always return an error immediately from the tool handler. Never proceed with a conflicted workspace.
- Git push failures: non-fatal; log a warning and continue.
- Git fetch failures: non-fatal; proceed in offline mode.
- Search index failures during upsert/remove: log a warning; don't abort the tool.
- Fact parsing failures during indexing: skip the file silently.
- In synthesize steps, per-fact failures (commit, delete, update): log a warning and continue; don't abort the entire step.

### Search index optionality

All tools must work when the search index is not initialized. The fallback for query is `git grep` + directory walk. The fallback for explore, why, learn, update, forget is direct file I/O — no search index operations are performed. Distill requires embeddings; it returns an error if called without an initialized embedder.

### Profile and instructions

Three profiles exist: `code` (default), `chat`, `generic`. The profile is a server startup flag. It affects only the content of the `knomit://instructions` MCP resource. All six tools and the `knomit-save` prompt behave identically across profiles.

### Server identity

The server is named `"knomit"` with version `"0.1.0"`. It communicates over stdio using the MCP stdio transport.

### Repository initialization

On server startup (`bootstrap`):

1. Determine `repoPath` and `cacheDir` from flags or defaults.
2. If `.git` does not exist in `repoPath`:
   - `git init <repoPath>`
   - Configure `user.email`, `user.name`, `commit.gpgsign = false`.
   - Write the root manifest file to `know.md` (YAML frontmatter with empty arrays, confidence 1.0, sources 1; body `"Root of the Knomit knowledge graph."`).
   - `git add know.md && git commit -m "init: create knowledge base"`.
   - `git branch -M main`.
   - `git checkout -b agent/<hostname>`.
3. If `.git` already exists:
   - Read current branch.
   - If not on `agent/<hostname>`:
     - If `agent/<hostname>` exists: `git checkout agent/<hostname>`.
     - Else: `git checkout -b agent/<hostname>`.
4. Initialize the search index (open SQLite, create tables, load FTS5 and optionally sqlite-vec).
5. Return `{ repo, searchIndex }`.
