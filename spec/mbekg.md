# Knomit Specification: Markdown-Based Executable Knowledge Graph (MBEKG)

## 1. Overview

Knomit (**kno**wledge + co**mmit**) is a knowledge representation system where facts are stored as markdown files in a Git repository. Git's native capabilities (commits, branches, history) handle lineage, timestamps, and versioning — the file itself carries only what Git cannot infer.

The system is designed for consumption by AI agents. Human readability is a secondary benefit.

## 2. The Fact File

A fact is a single markdown file consisting of YAML frontmatter and a markdown body.

### 2.1 Schema

```yaml
---
kind: <epistemic|pragmatic>
type: <type>
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

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `kind` | string | no | `epistemic` | Classification family: `epistemic` (descriptive — "what is") or `pragmatic` (prescriptive — "what to do"). Determines which `type` values are allowed. |
| `type` | string | no | `observation` (epistemic only) | Leaf type within the chosen `kind`. Epistemic: `observation`, `concept`, `process`, `principle`, `pattern`, `reference`, `synthesis`, `insight`, `hypothesis`, `methodology`. Pragmatic: `policy`, `heuristic` (no default — must be specified). |
| `domain` | string[] | yes | | Flexible categorization tags. A fact can belong to multiple domains. Not tied to directory structure. |
| `confidence` | float | yes | | 0.0 to 1.0. How strongly this fact should be weighted. Guides agent decision-making (e.g., 0.3 = weak signal, 0.9 = near-certain). |
| `sources` | integer | yes | | Count of independent corroborations. Distinct from Git commit count — tracks how many independent agents or observations produced this fact. |
| `entities` | string[] | yes | | Flat list of entity tags for discovery. Acts as a lightweight search index. |
| `refs` | string[] | no | `[]` | Evidence pointers: external URLs or local fact file paths. See Section 4. |

### 2.3 Types

Every `type` belongs to exactly one `kind`.

**Epistemic types** (`kind: epistemic` — descriptive, "what is"):

| Type | Meaning |
|---|---|
| `observation` | An empirically observed fact (default) |
| `concept` | A definition or description of a concept |
| `process` | A sequence of steps or workflow |
| `principle` | A guiding rule or causal claim |
| `pattern` | A recurring structure identified across observations |
| `reference` | A pointer to an external resource or standard |
| `synthesis` | A higher-order fact derived from other facts via automated synthesis |
| `insight` | A non-obvious grounded conclusion drawn from connecting facts already trusted |
| `hypothesis` | A falsifiable prediction derived from patterns — carries inherent uncertainty |
| `methodology` | A reasoning-process lesson learned from hypothesis outcomes |

**Pragmatic types** (`kind: pragmatic` — prescriptive, "what to do"; no default):

| Type | Meaning |
|---|---|
| `policy` | A mandatory rule that should always be followed |
| `heuristic` | A rule-of-thumb that biases decisions but is not absolute |

### 2.4 What Is NOT in the File

The following are intentionally omitted because Git handles them natively:

| Concept | Git Equivalent |
|---|---|
| Fact identity | File path within the ontology |
| Creation date | First commit timestamp for the file |
| Last verified / updated | Latest commit timestamp for the file |
| Author / source agent | Commit author email (see Section 5.3) |
| Operation type | Commit author email subaddress (e.g. `+learn@`) |
| Modification history | `git log --follow <file>` |
| Derived-from lineage | The `refs` field pointing to source fact paths |

## 3. The Ontology (Directory Structure)

### 3.1 Topic-Based Hierarchy

Facts are organized in a two-level hierarchy: **topic** → **category** → **fact files**. The top-level directory (the **ontology root**, default `kb/`) contains topic directories, each containing category subdirectories.

```
kb/
  kb.md                              ← root manifest
  domains/
    ontology.yaml                    ← ontology definition
  people/
    individuals/
      a1b2c3d4.md                    ← fact file (UUID filename)
      e5f6g7h8.md
    relationships/
      i9j0k1l2.md
  technology/
    software/
      m3n4o5p6.md
    networking/
      q7r8s9t0.md
  science/
    natural/
      u1v2w3x4.md
```

### 3.2 Ontology Definition

The ontology is defined in a YAML file (`domains/ontology.yaml`) that declares the valid topics and their children:

```yaml
id: general
name: General Knowledge
description: >
  A broad-purpose taxonomy for organizing knowledge by subject area.
topics:
  people:
    description: Individuals, groups, relationships
    children:
      individuals:
        description: Specific persons
      groups:
        description: Teams, organizations, communities
      relationships:
        description: Connections between people
  technology:
    description: Software, hardware, networking, data
    children:
      software:
        description: Languages, frameworks, tools, systems
      hardware:
        description: Devices, components, infrastructure
      ...
```

The default ontology ships with 12 top-level topics: people, technology, science, society, culture, geography, history, health, philosophy, religion, business, reference. Custom ontologies can replace it.

### 3.3 Fact Placement Rules

- **Topic and category are validated** against the ontology definition at write time. Unknown topics are rejected; categories beyond the defined children are allowed (freeform nesting).
- **Fact filenames are server-generated UUIDs** (8-character prefix), e.g. `a1b2c3d4.md`. Agents supply topic, category, title, and body — the server assigns the path.
- **Path format:** `<ontologyRoot>/<topic>/<category>/<uuid>.md`
- **The ontology root is configurable** (default: `kb`).
- **Topic/category keys** must be lowercase kebab-case: `[a-z0-9]+(-[a-z0-9]+)*`

### 3.4 Root Manifest

`kb.md` is a plain markdown file at the ontology root. It describes the knowledge base itself. It is not a fact — it has no frontmatter and is not indexed.

## 4. Evidence and References

### 4.1 The `refs` Field

Each ref is a plain string. There are two kinds:

| Format | Meaning | Example |
|---|---|---|
| Local fact path | Another fact in this repository | `kb/technology/software/a1b2c3d4.md` |
| External URL | An external resource | `https://example.com/source` |

Refs ending in `.md` are treated as local fact references. Everything else is treated as an external URL.

### 4.2 Two Evidence Mechanisms

Evidence chains are resolved through two complementary mechanisms:

**Implicit (Git-native):** Facts committed together in the same learning moment are evidence for each other. A `learn` commit may contain multiple files — all facts in that commit are implicitly linked.

**Explicit (`refs`):** When a fact is derived from other facts (e.g., synthesis merging multiple observations into a pattern), the `refs` field contains the file paths of the source facts. This is the cross-cutting evidence link.

### 4.3 Evidence Traversal ("Why is this true?")

The `explain` MCP tool performs automated breadth-first evidence traversal:

1. Read the root fact, extract its `refs`
2. For each local ref (`.md` path), read the referenced fact at the commit when the referrer was last modified
3. Recursively follow refs up to a maximum depth (default: 10)
4. Track seen paths to prevent cycles
5. Return the evidence tree as a paginated session (25 facts per page)

Manual traversal via git:

```
# Find the fact's history
git log --follow --format="%H %aN <%aE> %s" kb/path/to/fact.md

# Find sibling facts committed together
git show --stat <commit_hash>

# Follow explicit refs
# Read the fact's refs field, then read each referenced .md file
```

## 5. Learning Operations as Git Operations

### 5.1 Operation Mapping

| Learning Operation | Git Operation |
|---|---|
| Learn something new | Commit new fact file(s) to agent branch, push |
| Update a fact | Edit file on agent branch, commit, push |
| Retract a fact | Delete file from agent branch, commit, push |
| Synthesize / merge facts | Write merged fact, delete sources, commit, push |
| Accept knowledge | Merge agent branch into `main` (human or automated) |
| Trace fact history | `git log --follow <file>` |
| Filter by operation type | `git log --author="+learn@"` |
| Identify contributor | `git log --committer` |
| Roll back understanding | `git revert <commit>` |

### 5.2 The Agent Branch Model

Each agent (MCP server instance) operates on a **long-lived personal branch** named `agent/<id>`, where `<id>` is derived from the machine hostname plus a short hash (e.g. `agent/laptop-a1b2c3`).

```
main                 ← accepted truth, never written by agents directly
agent/laptop-a1b2    ← agent on "laptop" commits here
agent/server-c3d4    ← agent on "server" commits here
```

**Write flow (learn, update, retract):**
```
1. sync()         pull + merge origin/main into agent branch
2. commit         write the fact file(s), with operation-typed author signature
3. push           push agent branch to origin
```

**Read flow (query, explain, explore):**
```
1. sync()         ensure agent branch is up-to-date with main
2. read           read files directly from HEAD
```

**Synthesis flow:**
```
1. Execute prune/distill steps on agent branch, committing results
2. Each commit carries the appropriate operation in its author signature
   (subsume for writes, retract for deletions)
```

### 5.3 Operation Identity (Author/Committer Convention)

Operations are classified using **email subaddressing** in the git commit's author field. The committer field carries the stable agent identity.

```
Author:    <identity> <<identity>+<operation>@<domain>>
Committer: <identity> <<identity>@<domain>>
```

#### Agent Commits

| Field | Value | Example |
|---|---|---|
| Author | `<agent-id> <<agent-id>+<op>@agents.knomit.io>` | `laptop-a1b2 <laptop-a1b2+learn@agents.knomit.io>` |
| Committer | `<agent-id> <<agent-id>@agents.knomit.io>` | `laptop-a1b2 <laptop-a1b2@agents.knomit.io>` |

#### Human Commits

Humans use their own email with the `+tag` subaddress convention:

| Field | Value | Example |
|---|---|---|
| Author | `<name> <<email>+<op>>` | `Bob <bob+learn@gmail.com>` |
| Committer | `<name> <<email>>` | `Bob <bob@gmail.com>` |

#### Operations

| Operation | Meaning |
|---|---|
| `learn` | New fact(s) added |
| `update` | Existing fact modified |
| `retract` | Fact deleted |
| `subsume` | Facts merged or synthesized |
| `sync` | Merge commit from remote synchronization |

#### Querying by Operation

```sh
# All learn operations (agent + human)
git log --author="+learn@"

# All operations from a specific agent
git log --author="laptop-a1b2"

# All agent operations (any type)
git log --author="agents.knomit.io"

# Learn operations from a specific agent
git log --author="laptop-a1b2+learn"
```

### 5.4 Rules

- **Agents never commit directly to `main`.** All knowledge enters through agent branches.
- **`main` is the accepted truth.** It represents the swarm's consensus.
- **Learn batches multiple facts in a single commit.** All facts in a learning moment share one commit. Update and retract operate on individual facts.
- **Agent branches are long-lived.** One branch per agent, many learning moments per branch. Learning moments are identified by the commit's author signature (operation + agent identity).
- **Fact evolution is in-place.** When understanding changes, the same file is edited and recommitted. Git history shows how the fact evolved.
- **Deduplication on learn.** When a new fact is near-identical (>0.92 similarity) to an existing fact in the same category, the facts are merged: higher confidence wins the title/body, metadata is unioned, sources are summed.

## 6. Synthesis (Prune and Distill)

Synthesis is a two-phase process that refines the knowledge base by removing redundancy and extracting higher-order patterns.

### 6.1 Prune

1. Gather all facts and cluster them by semantic similarity (or graph community detection)
2. For each multi-fact cluster, send to an LLM for review
3. LLM returns decisions per fact: `keep`, `retract` (delete), or `update` (adjust confidence)
4. LLM may also propose `merge` entries: combine multiple facts into one, delete sources
5. Apply decisions: retracted facts are deleted (`retract` operation), merged facts are written (`subsume` operation), source facts deleted (`retract` operation)

### 6.2 Distill

1. Cluster facts at increasing levels of abstraction (RAPTOR-style recursive summarization)
2. At each depth level, send clusters to LLM for synthesis
3. LLM produces new higher-order facts (type: `synthesis`) and identifies facts to retract
4. Synthesized facts are written (`subsume` operation) with `refs` pointing to source fact paths
5. Retracted facts are deleted (`retract` operation)

### 6.3 Review Mode

Synthesis can run in **review mode** — a multi-turn session where an LLM reviews facts incrementally:

1. Identify dirty facts (changed since last review watermark)
2. Cluster dirty facts with their neighbors
3. Generate work items (prune clusters, then distill)
4. Process one work item per turn, applying decisions after each
5. Advance the review watermark to HEAD on completion

## 7. Full Example

### Repository State

```
kb/
  kb.md
  domains/
    ontology.yaml
  people/
    individuals/
      a1b2c3d4.md          ← "Alice likes rock music"
    interests/
      e5f6g7h8.md          ← "Alice's music taste shifts seasonally"
  geography/
    urban/
      i9j0k1l2.md          ← "London rain in April"
```

### A Ground-Level Fact

`kb/people/individuals/a1b2c3d4.md`

```yaml
---
type: observation
domain: [personal, music]
confidence: 0.85
sources: 3
entities: [alice, rock_music]
refs:
  - https://example.com/spotify-history-2024
---
# Alice likes rock music

Alice has a strong preference for rock music, demonstrated by
purchasing Album X in 2024 and attending Concert Y in 2025.
```

### A Synthesized Fact

`kb/people/interests/e5f6g7h8.md`

```yaml
---
type: synthesis
domain: [personal, music, behavioral_patterns]
confidence: 0.72
sources: 1
entities: [alice, music_taste, seasonal_patterns]
refs:
  - kb/people/individuals/a1b2c3d4.md
  - kb/people/individuals/m3n4o5p6.md
  - kb/people/individuals/q7r8s9t0.md
---
# Alice's music taste shifts seasonally

Alice shows a consistent pattern of shifting music preferences
with the seasons. Rock and metal dominate in summer months
around festival season, while hip-hop listening increases
in winter.
```

## 8. The Search Index (Ephemeral Cache)

The search index is a local SQLite database that accelerates queries. It is **not part of the spec** — it is a local optimization that can be rebuilt from the git repo at any time.

### 8.1 Properties

- **Ephemeral.** Never committed to the git repo. Lives in the local database alongside the git storer.
- **Rebuildable.** Reconstructed by walking all `.md` files from HEAD and parsing frontmatter.
- **Incrementally maintained.** Updated on every commit via an observer; synced by diffing the git log against the last indexed commit.

### 8.2 What it indexes

For each fact file:

- Path, title, blob hash
- Frontmatter: type, domain, entities, confidence, sources, refs
- Last commit hash

Additionally, a **commit log** table tracks per-commit metadata:

- Commit hash, file path, timestamp, message
- Operation type and author email (extracted from commit author)
- File action (added, modified, deleted)

### 8.3 Search capabilities

- **Semantic search** — cosine similarity over dense embeddings (384-dim all-MiniLM-L6-v2), with post-filtering by domain, entities, path prefix, and minimum confidence
- **Graph** — DERIVED_FROM edges between synthesized facts and their sources, enabling lineage queries and community detection for clustering

### 8.4 Embeddings

Embeddings are ephemeral and optional. They are computed locally using an ONNX inference model and stored in a `sqlite-vec` virtual table. If embeddings are not available, search falls back to returning all facts matching the filters.
