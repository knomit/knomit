# Knomit Specification: Markdown-Based Executable Knowledge Graph (MBEKG)

## 1. Overview

Knomit (**kno**wledge + co**mmit**) is a knowledge representation system where facts are stored as markdown files in a Git repository. Git's native capabilities (commits, branches, history) handle lineage, timestamps, and versioning — the file itself carries only what Git cannot infer.

The system is designed for consumption by AI agents. Human readability is a secondary benefit.

## 2. The Fact File

A fact is a single markdown file consisting of YAML frontmatter and a markdown body.

### 2.1 Schema

```yaml
---
kind: pragmatic            # OMITTED entirely for epistemic facts (the default)
type: <type>
domain: [<string>, ...]
confidence: <float 0.0-1.0>
sources: <integer>
evidence_weight: <float>   # derived; OMITTED when 0 (see §2.2)
entities: [<string>, ...]
refs: [<string>, ...]
---
# <Fact Title>

<Natural language description of the fact, with any nuance or context
that an agent would need to understand and apply it.>
```

Fields are emitted in exactly the order above. Two keys are conditionally
omitted: `kind` is written only for `pragmatic` facts (epistemic is the
default and renders no `kind` line), and `evidence_weight` is written only
when greater than 0. List-valued fields (`domain`, `entities`, `refs`) are
serialized inline (`[a, b]`), not as block sequences.

### 2.2 Field Definitions

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `kind` | string | no | `epistemic` | Classification family: `epistemic` (descriptive — "what is") or `pragmatic` (prescriptive — "what to do"). Determines which `type` values are allowed. Omitted from the file when `epistemic`. |
| `type` | string | no | `observation` (epistemic only) | Leaf type within the chosen `kind`. Epistemic: `observation`, `concept`, `process`, `principle`, `pattern`, `reference`, `synthesis`, `insight`, `hypothesis`, `methodology`. Pragmatic: `policy`, `heuristic` (no default — must be specified). |
| `domain` | string[] | yes | | Flexible categorization tags. A fact can belong to multiple domains. Not tied to directory structure. |
| `confidence` | float | yes | | Must lie in `[0.0, 1.0]` (validated on read and write). How strongly this fact should be weighted. Guides agent decision-making (e.g., 0.3 = weak signal, 0.9 = near-certain). |
| `sources` | integer | yes | | Must be `>= 0` (validated on read and write). Count of independent corroborations. Distinct from Git commit count — tracks how many independent agents or observations produced this fact. |
| `evidence_weight` | float | no | `0` (omitted) | Derived corroboration score, written only on synthesized/merged facts. Computed as `Σ(confidenceᵢ · sourcesᵢ) / (Σ(confidenceᵢ · sourcesᵢ) + 1)` over the source facts. Omitted from the file when 0. Not authored by hand — recomputed during synthesis. |
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

### 3.2.1 Ontology Presets and Per-Repo Ontology

The ontology is **per-repository**: each repo loads its own `domains/ontology.yaml`
at open time. There is no shared, process-global ontology — a server managing
multiple repos uses each repo's own definition.

The implementation ships two embedded presets that a new repo can be
initialized from (and which a stored ontology can be auto-upgraded toward when
it is a strict subset of a newer preset):

- **`general`** (`id: general`, "General Knowledge") — the broad subject-area
  taxonomy shown above. Ships with **13 top-level topics**: people, technology,
  science, society, culture, geography, history, health, philosophy, religion,
  business, reference, and **meta** (reasoning — holds `methodology` facts).
- **`source-code`** (`id: source-code`, "Source Code Knowledge") — a taxonomy
  for agents working inside a codebase. Top-level topics: invariants,
  architecture, conventions, decisions, gotchas, incidents, meta, principles.

> **Note for implementers:** the default MCP profile (`--mcp`, "code") seeds
> the `source-code` ontology, **not** `general`. A compatible implementation
> should not assume the general taxonomy is in force; always read the repo's
> `domains/ontology.yaml`.

### 3.2.2 Ontology Validation Rules

Any ontology node (root, topic, or any child) may declare a `validations` list.
Each entry is a named rule whose `rule` is a **JavaScript boolean expression**
evaluated against the fact on every write; a fact that fails any applicable
rule is rejected with the rule's `message`.

```yaml
topics:
  principles:
    description: Designer-authored intent
    validations:
      - name: must-be-pragmatic-policy
        message: "principles must declare kind=pragmatic and type=policy"
        rule: "fact.kind === 'pragmatic' && fact.type === 'policy'"
```

The rule runs in a sandboxed JS engine with a single read-only `fact` object
exposing: `kind`, `type`, `domain` (array), `entities` (array), `refs` (array),
`title`, `body`, `path`, and `confidence`. Rules are pure boolean expressions
(no I/O, no host bindings) and are bounded by a short execution timeout.

The default ontology ships with no validation rules; the `source-code` preset
attaches them to `principles` (e.g. requiring `kind=pragmatic`/`type=policy`).

### 3.3 Fact Placement Rules

- **Topic and category are validated** against the ontology definition at write time. Unknown topics are rejected; categories beyond the defined children are allowed (freeform nesting).
- **Ontology validation rules** declared on the matching node(s) are also enforced on write (see §3.2.2).
- **Numeric field bounds** are enforced on both read (parse) and write (serialize): `confidence` must be in `[0.0, 1.0]` and `sources` must be `>= 0`. An out-of-range value fails round-trip, so no write path can persist one.
- **Fact filenames are server-generated UUIDs** (8-character prefix), e.g. `a1b2c3d4.md`. Agents supply topic, category, title, and body — the server assigns the path.
- **Path format:** `<ontologyRoot>/<topic>/<category>/<uuid>.md`
- **The ontology root is configurable** (default: `kb`).
- **Topic/category keys** must be lowercase kebab-case: `[a-z0-9]+(-[a-z0-9]+)*`. Paths are lowercased on construction.

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
| Learn something new | Commit new fact file(s) to agent branch (commit op `learn`); pushed later |
| Update a fact | Edit file on agent branch, commit (op `update`); pushed later |
| Retract a fact | Delete file from agent branch, commit (op `retract`); pushed later |
| Synthesize / merge facts | Write merged fact + delete sources, commit (op `subsume`); pushed later |
| Publish to remote | Force-push the agent branch to origin (never `main`) |
| Accept knowledge | Merge agent branch(es) into the consensus branch — performed remote-side, never by an agent push (see §5.4) |
| Trace fact history | `git log --follow <file>` |
| Filter by operation type | `git log --author="+learn@"` |
| Identify contributor | `git log --committer` |
| Roll back understanding | `git revert <commit>` |

### 5.2 The Agent Branch Model

Each agent (MCP server instance) operates on a **long-lived personal branch** named `agent/<hostname>-<fingerprint>`, where `<hostname>` is the (ref-sanitized) machine hostname — falling back to `local` when unavailable — and `<fingerprint>` is the first 8 hex characters of the SHA-256 of the agent's SSH public key. The key (an ed25519 keypair, generated on first run) is the agent's stable identity, so two agents on the same host still get distinct branches.

```
main                          ← consensus / accepted truth (configurable name; see below)
agent/laptop-3f9a2b1c         ← agent on host "laptop" (key fp 3f9a2b1c) commits here
agent/server-7d4e0a55         ← agent on host "server" (key fp 7d4e0a55) commits here
```

The **consensus branch name defaults to `main` but is configurable per repo** (e.g. `master`). Agents never write it directly.

**Write flow (learn, update, retract):**
```
1. commit         write the fact file(s), with operation-typed author signature
2. push           force-push agent branch to origin (typically batched, not per-write)
```

**Read flow (query, explain, explore):**
```
1. read           read files directly from agent-branch HEAD
```

Reads do **not** trigger a synchronous pull. Keeping the agent branch
current with the consensus branch is handled out of band by a background
reconcile loop (fetch consensus → reconcile into agent branch → push), not as
a per-operation step. A compatible implementation may sync on any cadence it
likes; the only invariant is that reads observe the agent branch's HEAD.

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
| Author | `<agent-id> <<agent-id>+<op>@agents.knomit.io>` | `laptop-3f9a2b1c <laptop-3f9a2b1c+learn@agents.knomit.io>` |
| Committer | `<agent-id> <<agent-id>@agents.knomit.io>` | `laptop-3f9a2b1c <laptop-3f9a2b1c@agents.knomit.io>` |

The `<agent-id>` is the agent branch name with the `agent/` prefix stripped (so branch `agent/laptop-3f9a2b1c` → agent-id `laptop-3f9a2b1c`).

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
| `subsume` | Facts merged or synthesized (synthesis writes use `subsume`) |
| `merge` | Branch merge commit (e.g. consensus reconciled into the agent branch) |

The five tokens above are the operations actually written to commits. There is
no distinct `synthesize` token — synthesis records its writes as `subsume` (and
`update`) and its source deletions as `retract`.

#### Querying by Operation

```sh
# All learn operations (agent + human)
git log --author="+learn@"

# All operations from a specific agent
git log --author="laptop-3f9a2b1c"

# All agent operations (any type)
git log --author="agents.knomit.io"

# Learn operations from a specific agent
git log --author="laptop-3f9a2b1c+learn"
```

### 5.4 Rules

- **Agents never commit directly to the consensus branch.** Agents force-push only their own `agent/<hostname>-<fingerprint>` branch; the consensus branch is written exclusively by the remote-side merge mechanism.
- **The consensus branch is the accepted truth.** It represents the swarm's consensus (default `main`, configurable per repo).
- **Consensus is reached remote-side, not by agent push.** Promotion of agent-branch facts into the consensus branch is performed by a separate merge step on the remote, never by an agent. (An MCP-driven agent→consensus merge is a designed-but-not-yet-implemented capability; this section describes current behavior.)
- **Learn batches multiple facts in a single commit.** All facts in a learning moment share one commit. Update and retract operate on individual facts.
- **Agent branches are long-lived.** One branch per agent, many learning moments per branch. Learning moments are identified by the commit's author signature (operation + agent identity).
- **Fact evolution is in-place.** When understanding changes, the same file is edited and recommitted. Git history shows how the fact evolved.
- **Deduplication on learn.** When a new fact is near-identical to an existing fact in the same category, the facts are merged: higher confidence wins the title/body, metadata is unioned, sources are summed. The near-duplicate threshold defaults to **0.92** cosine similarity but is **embedding-model-dependent** — an implementation calibrates it per model rather than treating 0.92 as universal.

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
refs: [https://example.com/spotify-history-2024]
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
evidence_weight: 0.84
entities: [alice, music_taste, seasonal_patterns]
refs: [kb/people/individuals/a1b2c3d4.md, kb/people/individuals/m3n4o5p6.md, kb/people/individuals/q7r8s9t0.md]
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
- Frontmatter: kind, type, domain, entities, confidence, sources, evidence_weight, refs
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
