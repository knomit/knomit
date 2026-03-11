# Knomit Specification: Markdown-Based Executable Knowledge Graph (MBEKG)

## 1. Overview

Knomit (**kno**wledge + co**mmit**) is a knowledge representation system where facts are stored as markdown files in a Git repository. Git's native capabilities (commits, branches, tags, history) handle identity, lineage, timestamps, and versioning — the file itself carries only what Git cannot infer.

The system is designed for consumption by AI agents. Human readability is a secondary benefit.

## 2. The Fact File

A fact is a single markdown file consisting of YAML frontmatter and a markdown body.

### 2.1 Schema

```yaml
---
domain: [<string>, ...]
confidence: <float 0.0-1.0>
sources: <integer>
entities: [<string>, ...]
refs:
  - <string>
  - ...
---
# <Fact Title>

<Natural language description of the fact, with any nuance or context
that an agent would need to understand and apply it.>
```

### 2.2 Field Definitions

| Field | Type | Required | Description |
|---|---|---|---|
| `domain` | string[] | yes | Flexible categorization tags. A fact can belong to multiple domains. Not tied to directory structure. |
| `confidence` | float | yes | 0.0 to 1.0. How strongly this fact should be weighted. Guides agent decision-making (e.g., 0.3 = weak signal, 0.9 = near-certain). |
| `sources` | integer | yes | Count of independent corroborations. Distinct from Git commit count — tracks how many independent agents or observations produced this fact. |
| `entities` | string[] | yes | Flat list of entity tags for grep-based discovery. Acts as a lightweight search index. |
| `refs` | string[] | no | Evidence pointers. See Section 4. |

### 2.3 What Is NOT in the File

The following are intentionally omitted because Git handles them natively:

| Concept | Git Equivalent |
|---|---|
| Fact identity | Commit hash |
| Creation date | First commit timestamp |
| Last verified / updated | Latest commit timestamp |
| Author / source agent | `git log --author` |
| Modification history | `git log --follow <file>` |
| Derived-from lineage | Git commit graph (branch history, parent commits) |
| Reinforcement count | `git log --follow <file> \| wc -l` |

## 3. The Ontology (World Structure)

### 3.1 Directory Tree as Ontology

The repository's directory structure defines a strict "is-within" hierarchy called the **ontology**. It represents containment relationships that are unambiguous — a house is on a street, in a city, in a country.

```
know.md
know/
  earth.md
  earth/
    uk.md
    uk/
      london.md
      london/
        london-rain-in-april.md
        baker-street.md
        baker-street/
          221b.md
          221b/
            house-has-two-floors.md
            heating-is-gas.md
```

### 3.2 Rules

- **Facts are placed at the most specific level they apply to.** "It rains in London in April" goes in `london/`, not `uk/`.
- **Facts at higher levels are inherited by everything below.** "The UK drives on the left" applies to London, Manchester, and every address under `uk/`.
- **Each folder has a sibling manifest file** with the same name and `.md` extension. This file describes what that level of the hierarchy represents. It follows the same fact file schema.
- **The root is no exception.** `know.md` describes the knowledge base itself.
- **The ontology is not limited to physical space.** `know/digital/github/repo-x/` is equally valid.

### 3.3 Manifests

A manifest is a fact file that describes the ontology level itself, not something within it. It sits as a sibling to the folder it describes.

```yaml
---
domain: [geography, urban]
confidence: 0.99
sources: 1
entities: [london, uk, city]
refs: []
---
# London

Capital city of the United Kingdom. Population ~9 million.
Located in southeastern England on the River Thames.
```

### 3.4 Agent Navigation

1. Agent enters `know/earth/uk/london/`
2. Reads `london.md` (sibling manifest) — understands what "London" is
3. Reads fact files in `london/` — learns what is true about London
4. Walks up to `uk.md`, `earth.md` — inherits higher-level truths
5. Walks down into subdirectories — explores more specific contexts

## 4. Evidence and References

### 4.1 The `refs` Field

Each ref is a single string. The format determines how the MCP resolves it:

| Format | Meaning | Example |
|---|---|---|
| `knomit:blob/<hash>/<path>` | Fact in this repo at a specific commit | `knomit:blob/abc1234/know/foo/bar.md` |
| `knomit://<host>/<repo>/blob/<hash>/<path>` | Fact in a remote repo | `knomit://github.com/org/kb/blob/abc1234/know/foo.md` |
| `episodic://` URI | Raw event in an episodic database | `episodic://event_88` |
| `https://` URI | External URL | `https://example.com/source` |
| Bare hex string | **Deprecated.** Local commit hash, ambiguous across clones | `abc1234` |

The protocol acts as the type. The MCP parses the scheme and delegates to the appropriate handler.

**Preferred format for local refs:** `knomit:blob/<hash>/<path>`. Bare hex hashes are ambiguous — they break when the knowledge base is cloned to a different machine or when history is rewritten. Always use the fully qualified `knomit:` URI form.

When an MCP tool produces refs from local file paths (e.g. synthesize output), it resolves them automatically to `knomit:blob/<7-char-hash>/<path>` using the last commit for that file.

### 4.2 Two Evidence Mechanisms

Evidence chains are resolved through two complementary mechanisms:

**Implicit (Git-native):** Facts committed together in the same learning moment are evidence for each other. The agent finds siblings by looking up the learning moment tag and listing all commits under it. No explicit linking needed.

**Explicit (`refs`):** When a fact is derived from facts across different learning moments (e.g., a Weaver agent synthesizing a pattern from 10 independently contributed facts), the `refs` field points to the source commit hashes. This is the cross-cutting evidence link.

### 4.3 Evidence Traversal ("Why is this true?")

**Step 1 — Find the fact.**
```
grep -rl "entity_name" --include="*.md" know/
```

**Step 2 — Find the learning moment.**
```
git log --follow --format="%H %s" know/path/to/fact.md
```

**Step 3 — Find sibling facts (implicit evidence).**
```
git tag --contains <commit_hash>
git log main..<tag> --format="%H %s"
```

**Step 4 — Follow explicit refs.**
Read the fact's `refs` field. For each bare hash, read the file at that commit. For URIs, resolve via the appropriate protocol handler.

**Step 5 — Go deeper (recursive).**
Each evidence fact can itself be traversed using the same steps, producing a full evidence tree.

## 5. Learning Operations as Git Operations

### 5.1 Operation Mapping

| Learning Operation | Git Operation |
|---|---|
| Learn something new | Commit new fact file(s) to agent branch, tag, push |
| Reinforce a fact | Edit file on agent branch, bump `confidence`/`sources`, commit, tag, push |
| Contradict / update a fact | Edit file on agent branch, rewrite body and metadata, commit, tag, push |
| Accept knowledge | Merge agent branch into `main` (human or automated) |
| Name a learning moment | Tag HEAD of agent branch at time of commit |
| Forget a fact | Delete file from agent branch, commit, tag `forget/<name>`, push |
| Trace fact history | `git log --follow <file>` |
| Trace a learning moment | Find tag, then `git log <tag-parent>..<tag>` |
| Identify contributor | `git log --author` |
| Roll back understanding | `git revert <commit>` |

### 5.2 The Agent Branch Model

Each agent (MCP server instance) operates on a **long-lived personal branch** named `agent/<id>`, where `<id>` is typically the machine hostname but is configurable.

```
main              ← accepted truth, never written by agents directly
agent/laptop      ← agent on "laptop" commits here
agent/server      ← agent on "server" commits here
synthesize/daily  ← temporary branch for a synthesis run
```

**Write flow (learn, update, forget):**
```
1. sync()         pull + merge origin/main into agent branch
2. commit         write the fact file(s), one commit per fact
3. tag            tag HEAD with learn/<name> or forget/<name>
4. push           push agent branch to origin
```

**Read flow (query, why, explore):**
```
1. sync()         ensure agent branch is up-to-date with main
2. read           read files directly from working tree / HEAD
```

**Synthesis flow:**
```
1. Create synthesize/<recipe> branch from agent branch
2. Execute prune/distill steps, committing results
3. Tag each step: learn/synthesize-<recipe>-prune, learn/synthesize-<recipe>-distill
4. Either auto-merge into agent branch and delete, or push for review
```

### 5.3 Tag Naming Conventions

| Tag Pattern | Created By | Meaning |
|---|---|---|
| `learn/<name>` | learn, update | A learning moment; name is caller-supplied |
| `forget/<name>` | forget | A forgetting moment |
| `learn/synthesize-<recipe>-prune` | synthesize prune step | Pruning run completed |
| `learn/synthesize-<recipe>-distill` | synthesize distill step | Distillation run completed |

Tag names are sanitized: characters outside `[a-zA-Z0-9._/-]` are replaced with `-`.

### 5.4 Rules

- **Agents never commit directly to `main`.** All knowledge enters through agent branches.
- **`main` is the accepted truth.** It represents the swarm's consensus.
- **One fact per commit.** The commit hash is the fact's identity at that point in time.
- **Agent branches are long-lived.** One branch per agent, many learning moments per branch. Learning moments are identified by tags, not branches.
- **Fact evolution is in-place.** When understanding changes, the same file is edited and recommitted. Git history shows how the fact evolved.
- **sync() before every operation.** Agents always merge the latest main before writing, minimizing conflicts.

## 6. Cross-Moment Synthesis (The Weaver Pattern)

When an agent identifies patterns across multiple independently learned facts (from different learning moments, potentially from different agents), it creates a higher-order fact:

1. Creates a new learning branch
2. Commits a new fact file whose `refs` point to the source commit hashes
3. Opens a PR like any other learning event
4. On merge, the synthesized fact becomes part of accepted truth

The synthesized fact is structurally identical to any other fact. The only difference is that its `refs` contain local commit hashes pointing to the facts it was derived from, rather than external URIs.

## 7. Full Example

### Repository State

```
know.md
know/
  earth.md
  earth/
    uk.md
    uk/
      london.md
      london/
        london-rain-in-april.md
  people.md
  people/
    alice.md
    alice/
      alice-likes-rock-music.md
      alice-music-shifts-seasonally.md
```

### A Ground-Level Fact

`know/people/alice/alice-likes-rock-music.md`

```yaml
---
domain: [personal, music]
confidence: 0.85
sources: 3
entities: [alice, rock_music]
refs:
  - episodic://spotify-history-2024
---
# Alice likes rock music

Alice has a strong preference for rock music, demonstrated by
purchasing Album X in 2024 and attending Concert Y in 2025.
```

### A Synthesized Fact

`know/people/alice/alice-music-shifts-seasonally.md`

```yaml
---
domain: [personal, music, behavioral_patterns]
confidence: 0.72
sources: 10
entities: [alice, music_taste, seasonal_patterns]
refs:
  - knomit:blob/abc1234/know/people/alice/alice-likes-rock-music.md
  - knomit:blob/def5678/know/people/alice/alice-bought-album-x.md
  - knomit:blob/ghi9012/know/people/alice/alice-attended-concert-y.md
---
# Alice's music taste shifts seasonally

Alice shows a consistent pattern of shifting music preferences
with the seasons. Rock and metal dominate in summer months
around festival season, while hip-hop listening increases
in winter.
```

## 8. The Search Index (Ephemeral Cache)

Implementations may maintain a local search index to accelerate queries. This index is **not part of the spec** — it is a local optimization that can be rebuilt from the git repo at any time.

### 8.1 Properties

- **Ephemeral.** Never committed to the git repo. Lives only in a local cache directory.
- **Rebuildable.** Can always be reconstructed by walking the ontology tree and parsing every `.md` file from the current HEAD.
- **Incrementally maintained.** Updated as facts are written; synced by diffing the git log against the last indexed commit.
- **Implementation-defined.** Different implementations may use different backends (SQLite, in-memory, etc.).

### 8.2 What it indexes

For each fact file:

- Path, title, body
- Frontmatter: domain, entities, confidence, sources, refs
- Last commit hash

### 8.3 Common capabilities

- **Full-text search** — BM25 ranking over title, body, entities, domain
- **Vector similarity search** — cosine similarity over dense embeddings (e.g. 384-dim BERT); enables semantic search beyond keyword matching
- **Hybrid scoring** — combine BM25 and vector scores (typical split: 60/40)
- **Synthesis log** — tracks the last-processed commit per recipe name, enabling incremental delta-mode synthesis runs

### 8.4 Embeddings

Embeddings are also ephemeral and optional. They are computed locally using an ONNX inference model (e.g. `all-MiniLM-L6-v2`) and stored alongside the search index. If the embedding store is absent or disabled, vector search degrades gracefully to FTS-only.
