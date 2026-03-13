# Knomit Specification: Markdown-Based Executable Knowledge Graph (MBEKG)

## 1. Overview

Knomit (**kno**wledge + co**mmit**) is a knowledge representation system where facts are stored as markdown files in a Git repository. Git's native capabilities (commits, branches, tags, history) handle identity, lineage, timestamps, and versioning — the file itself carries only what Git cannot infer. The spec is agnostic to where or how the Git repository is stored — any backing store that implements Git's object model and protocols is valid.

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
| Fact identity | File path (the path is the stable identity; the commit is the temporal version) |
| Creation date | First commit timestamp |
| Last verified / updated | Latest commit timestamp |
| Author / source agent | `git log --author` |
| Modification history | Git log for file |
| Derived-from lineage | Git commit graph (branch history, parent commits) |
| Reinforcement count | Count of commits touching file |

## 3. The Ontology (World Structure)

### 3.1 Directory Tree as Ontology

The repository's directory structure defines a strict "is-within" hierarchy called the **ontology**. It represents containment relationships that are unambiguous — a house is on a street, in a city, in a country.

```
know/
  earth/
    uk/
      london/
        london-rain-in-april.md
        baker-street/
          221b/
            house-has-two-floors.md
            heating-is-gas.md
```

### 3.2 Rules

- **Facts are placed at the most specific level they apply to.** "It rains in London in April" goes in `london/`, not `uk/`.
- **Facts at higher levels are inherited by everything below.** "The UK drives on the left" applies to London, Manchester, and every address under `uk/`.
- **Folders may have a sibling manifest file** with the same name and `.md` extension. This file describes what that level of the hierarchy represents. It follows the same fact file schema. Manifests are optional — directories can exist without them.
- **The root is `know/`.** All facts live under this prefix.
- **The ontology is not limited to physical space.** `know/digital/github/repo-x/` is equally valid.

### 3.3 Manifests

A manifest is an optional fact file that describes the ontology level itself, not something within it. It sits as a sibling to the folder it describes (e.g., `know/earth/uk/london.md` describes the `london/` directory).

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
2. Reads `london.md` (sibling manifest, if present) — understands what "London" is
3. Reads fact files in `london/` — learns what is true about London
4. Walks up to `uk.md`, `earth.md` (if present) — inherits higher-level truths
5. Walks down into subdirectories — explores more specific contexts

## 4. Evidence and References

### 4.1 The `refs` Field

Each ref is a single string. The format determines how the MCP resolves it:

| Format | Meaning | Example |
|---|---|---|
| Local path | Fact in the same repo | `know/people/alice/alice-likes-rock-music.md` |
| `git://` URI | Fact in another knowledge repo | `git://other-repo/know/path/to/fact.md` |
| `episodic://` URI | Raw event in an episodic database | `episodic://event_88` |
| `https://` URI | External URL | `https://example.com/source` |

The protocol acts as the type. The MCP parses the scheme and delegates to the appropriate handler.

### 4.2 Two Evidence Mechanisms

Evidence chains are resolved through two complementary mechanisms:

**Implicit (Git-native):** Facts committed together in the same learning moment are evidence for each other. The agent finds siblings by looking up the learning moment tag and listing all commits under it. No explicit linking needed.

**Explicit (`refs`):** When a fact is derived from facts across different learning moments (e.g., a Weaver agent synthesizing a pattern from 10 independently contributed facts), the `refs` field points to the source fact paths. The version of each referenced fact is pinned implicitly — resolving a local path ref at the referencing fact's commit yields the exact content the agent saw when it drew its conclusion.

### 4.3 Evidence Traversal ("Why is this true?")

**Step 1 — Find the fact.**
Locate the fact file by entity, domain, or path within the `know/` tree.

**Step 2 — Find the learning moment.**
Walk the Git log for the fact file to find the tagged commit.

**Step 3 — Find sibling facts (implicit evidence).**
Find the tag containing the commit, then list all commits under that tag to discover facts from the same learning moment.

**Step 4 — Follow explicit refs.**
Read the fact's `refs` field. For local paths, resolve the file at the referencing commit to see the version the agent relied on. For URIs, resolve via the appropriate protocol handler.

**Step 5 — Go deeper (recursive).**
Each evidence fact can itself be traversed using the same steps, producing a full evidence tree.

## 5. Learning Operations as Git Operations

### 5.1 Operation Mapping

| Learning Operation | Git Operation |
|---|---|
| Learn something new | Create branch, commit new fact file(s), open PR |
| Reinforce a fact | Edit file on branch, bump `confidence`/`sources`, commit, PR |
| Contradict / update a fact | Edit file on branch, rewrite body and metadata, commit, PR |
| Accept knowledge | Merge PR into `main` |
| Name a learning moment | Tag the commit (see Section 5.4) |
| Reject knowledge | Close PR without merging, delete branch |
| Trace fact history | `git log --follow <file>` |
| Trace a learning moment | `git log main..<tag>` |
| Identify contributor | `git log --author` |
| Roll back understanding | `git revert <commit>` |

### 5.2 The Learning Lifecycle

```
1. Agent creates branch:      learn/alice-music-2025
2. Agent commits fact files:
   - alice-likes-rock-music.md
   - alice-bought-album-x.md
   - alice-attended-concert-y.md
3. Agent opens PR:             "Learning: Alice's music preferences"
4. Review & merge into main
5. Tag merge commit:           git tag learn/alice-music-2025
6. Delete branch:              learn/alice-music-2025
```

### 5.3 Rules

- **Agents never commit directly to `main`.** All knowledge enters through PRs.
- **`main` is the accepted truth.** It represents the swarm's consensus.
- **A fact's identity is its file path.** The commit hash is the temporal version marker. Multiple facts may share a commit.
- **One learning moment per branch.** Related facts are grouped by their shared branch.
- **Branches are ephemeral.** After merge, the tag preserves the learning moment. The branch is deleted.
- **Fact evolution is in-place.** When understanding changes, the same file is edited and recommitted. Git history shows how the fact evolved.

### 5.4 Tag Taxonomy

Every commit that modifies knowledge is tagged to encode how the knowledge evolved. The tag prefix acts as the operation type:

| Tag Prefix | Meaning | Example |
|---|---|---|
| `learn/` | New fact created | `learn/alice-music-2025` |
| `update/` | Existing fact modified (metadata or body) | `update/alice-confidence-bump` |
| `subsume/` | Higher-order fact synthesized from multiple sources | `subsume/alice-seasonal-pattern` |
| `retract/` | Fact removed from the knowledge base | `retract/outdated-album-preference` |

Tags are permanent markers in the Git history. Even after branches are deleted, tags preserve the full record of how each fact entered, changed, or left the knowledge base.

## 6. Cross-Moment Synthesis (The Weaver Pattern)

When an agent identifies patterns across multiple independently learned facts (from different learning moments, potentially from different agents), it creates a higher-order fact:

1. Creates a new learning branch
2. Commits a new fact file whose `refs` point to the source fact paths
3. Opens a PR like any other learning event
4. On merge, the synthesized fact becomes part of accepted truth

The synthesized fact is structurally identical to any other fact. The only difference is that its `refs` contain local fact paths pointing to the facts it was derived from, rather than external URIs.

## 7. Full Example

### Repository State

```
know/
  earth/
    uk/
      london/
        london-rain-in-april.md
  people/
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
  - know/people/alice/alice-likes-rock-music.md
  - know/people/alice/alice-bought-album-x.md
  - know/people/alice/alice-attended-concert-y.md
---
# Alice's music taste shifts seasonally

Alice shows a consistent pattern of shifting music preferences
with the seasons. Rock and metal dominate in summer months
around festival season, while hip-hop listening increases
in winter.
```