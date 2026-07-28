# Knomit Specification: Markdown-Based Executable Knowledge Graph (MBEKG)

## 1. Overview

Knomit (**kno**wledge + co**mmit**) is a knowledge representation system where
facts are stored as markdown files in a Git repository and every learning
operation is a Git operation. Git serves as the application protocol here in
roughly the way HTTP does for REST — but what matters is the concrete mapping,
not the analogy. The system is designed for consumption by AI agents; human
readability is a secondary benefit.

This document specifies everything a client needs to read and write a knomit
knowledge base given nothing but a clone of the repository: the fact file
format (§2), the ontology directory structure (§3), and the Git conventions
that encode operations, identity, and history (§4, §5).

### 1.1 Learning Operations Are Git Operations

The heart of the system is a direct correspondence between acts of learning
and acts of Git:

| Learning act | Git effect |
|---|---|
| Learn something new | A commit adding one or more new fact files |
| Update understanding | The fact file edited in place; a commit with the modification |
| Retract a fact | The fact file deleted; a commit with the deletion |
| Merge / subsume facts | One commit that adds the merged fact **and** deletes its source files |
| Trace how a fact evolved | The file's commit history |
| Who learned it, and when | The commit's author and timestamp |
| What was learned together | The other files in the same commit |
| Roll back understanding | Revert the commit |

Because learning *is* committing, the fact file carries only what a commit
cannot infer (§2.10). Identity, timestamps, authorship, modification history,
and retraction are all read from commits, never from the file.

Layered on top of this mapping is a labelling convention: each commit's
author email carries an **operation token** (`+learn`, `+update`, `+retract`,
…) in its `+` subaddress, so history can be filtered by the kind of learning
act that produced each commit. The tokens are the label, not the mechanism;
they are specified in §4.1–§4.2.

### 1.2 Obtaining the Repository

A client's entire view of a knowledge base is **an ordinary Git clone**. The
clone is complete and authoritative: facts, history, operations, and
identities are all reconstructible from it, and nothing outside it is part of
this specification.

A server is not required to keep its object store as a `.git` directory, so
the `git` CLI cannot be assumed to work against live server storage. History
is reached by cloning — either from the remote the server publishes to, or
from a read-only Git endpoint that supports fetch but not push.

Implementations may maintain derived indexes (search, embeddings, graphs) to
accelerate retrieval. Such indexes are private, rebuildable state: they are
never committed to the repository, and the Git history is the sole authority.
A client MUST NOT depend on their existence.

### 1.3 Client Orientation

To read a knowledge base:

1. Clone the repository and check out the branch of interest (§4.4).
2. Read `domains/ontology.yaml` for the taxonomy (§3.2). If absent, assume
   an embedded default (§3.3).
3. Walk the ontology root (default `kb/`) for `.md` files. Skip `kb.md` and
   any file that does not parse as a fact; treat every file that does parse
   as a fact, wherever it sits (§3.8).
4. For each fact, parse the YAML frontmatter and the title heading per §2.
5. Resolve `refs` — local paths, external URLs, or cross-repo `kb://`
   pointers — per §2.9.
6. For metadata the file does not carry — timestamps, authorship, operation,
   revision history — read the commit log, first-parent, per §4.

To write: commit fact files to your **own** agent branch only, using the
author-email operation convention of §4.1, and push only that branch (§4.5).

### 1.4 Scope

This document specifies the file format, the repository structure, and the
Git conventions. An implementation may additionally expose an HTTP API or a
set of agent tools; neither is part of this specification.

## 2. The Fact File

A fact is a single markdown file: YAML frontmatter between `---` delimiters,
then a markdown body whose first line is the title heading.

### 2.1 File Shape and Parse Rules

```yaml
---
kind: pragmatic            # OMITTED for epistemic facts (the default)
type: <type>
domain: [<string>, ...]
confidence: <float 0.0-1.0>
sources: <integer>
evidence_weight: <float>   # derived; OMITTED when 0 (see §5.2)
origin: <origin>           # conditionally omitted (see §2.5)
entities: [<string>, ...]
refs: [<string>, ...]
---
# <Fact Title>

<Natural language description of the fact, with any nuance or context
that an agent would need to understand and apply it.>
```

Parsing rules:

1. CRLF line endings are normalized to LF before anything else.
2. The file MUST begin with exactly `---` followed by a newline.
3. The closing delimiter is the next line consisting exactly of `---`. The
   split is **textual, not YAML-aware**: a `---` line inside a YAML block
   scalar would terminate the frontmatter early. Writers MUST NOT emit
   frontmatter containing such a line.
4. Unknown frontmatter keys MUST be ignored on read — and are therefore
   silently dropped on the next serialize. There is no extension mechanism.
5. The body is the whitespace-trimmed remainder. Its first line MUST be the
   title heading (§2.7); a missing heading is a parse error.
6. Missing list fields normalize to empty lists, so parse → serialize is a
   fixed point on canonical files.

Serialization writes `---`, the frontmatter, `---`, then `# ` + title. A
non-empty body follows after one blank line and ends with a trailing newline.

### 2.2 Emission Order

Writers MUST emit frontmatter keys in exactly this order, with three
conditional omissions. Note that `origin` sits between `evidence_weight` and
`entities`, not adjacent to `kind`/`type`.

| # | Key | Emitted | Rendering |
|---|---|---|---|
| 1 | `kind` | only when `pragmatic` | plain string |
| 2 | `type` | always | plain string |
| 3 | `domain` | always (empty → `[]`) | inline flow sequence |
| 4 | `confidence` | always | shortest numeric form |
| 5 | `sources` | always | integer |
| 6 | `evidence_weight` | only when `> 0` | shortest numeric form |
| 7 | `origin` | per the elision rule, §2.5.3 | plain string |
| 8 | `entities` | always (empty → `[]`) | inline flow sequence |
| 9 | `refs` | always (empty → `[]`) | inline flow sequence |

"Shortest numeric form" means `1.0` renders as `1`, and very small floats may
render in exponent notation (valid YAML — readers must accept it).
List-valued fields are always inline flow style (`[a, b]`), never block
sequences.

### 2.3 Field Definitions: Emitted vs Validated

**No frontmatter key is required on parse.** Every key may be absent; zero
values apply and are in-bounds (missing `confidence` → 0.0, missing
`sources` → 0). What parsing actually requires is structural: the two
delimiters and the title heading. Do not conflate whether a key is always
*emitted* with whether its *value* is validated; the table separates them.

**A parse default is not a write default.** The column below says what a
*reader* resolves an absent key to; it says nothing about what a *writer*
should store when its caller omits the field. The two differ deliberately for
`sources`: a file with no `sources` key parses as 0, but a write operation
whose caller omitted the count stores 1 — a fact that exists was produced by
at least one agent or observation, and storing 0 would erase it from every
`evidence_weight` computed over it (§5.2 multiplies by `sources`). A write
operation that advertises a default for an optional field must apply it;
an explicit 0 from the caller is a legal value and survives.

| Field | Type | Missing on parse resolves to | Validated | Description |
|---|---|---|---|---|
| `kind` | string | `epistemic` | strict on read and write | Classification family: `epistemic` (descriptive — "what is") or `pragmatic` (prescriptive — "what to do"). Determines which `type` values are allowed. |
| `type` | string | `observation` if epistemic; **parse error** if pragmatic | strict on read and write | Leaf type within the chosen `kind` (§2.4). |
| `domain` | string[] | `[]` | not validated at parse; ontology rules may apply on write (§3.4) | Flexible categorization tags. Not tied to directory structure. |
| `confidence` | float | `0.0` | strict on read and write: must lie in `[0.0, 1.0]` inclusive | How strongly the fact should be weighted (0.3 = weak signal, 0.9 = near-certain). |
| `sources` | integer | `0` | strict on read and write: must be `>= 0` | Count of independent corroborations. Distinct from commit count — tracks how many independent agents or observations produced this fact. |
| `evidence_weight` | float | `0` | **no bounds check on read or write**; the value only gates emission (`> 0`) | Derived corroboration score (§5.2). Never authored by hand. |
| `origin` | string | type-aware default (§2.5.1) | **asymmetric**: normalized on read, rejected on write (§2.5.4) | How the fact came to exist: `authored`, `distilled`, `discovered`. |
| `entities` | string[] | `[]` | not validated | Flat entity tags for discovery; a lightweight search index. |
| `refs` | string[] | `[]` | not validated; stored verbatim | Evidence pointers (§2.9). |

### 2.4 Kinds and Types

Every `type` belongs to exactly one `kind`.

**Epistemic types** (`kind: epistemic` — descriptive; default type
`observation`):

| Type | Meaning |
|---|---|
| `observation` | An empirically observed fact (the default) |
| `concept` | A definition or description of a concept |
| `process` | A sequence of steps or workflow |
| `principle` | A guiding rule or causal claim |
| `pattern` | A recurring structure identified across observations |
| `reference` | A pointer to an external resource or standard |
| `synthesis` | A higher-order fact derived from other facts |
| `insight` | A non-obvious grounded conclusion connecting already-trusted facts |
| `hypothesis` | A falsifiable prediction derived from patterns — inherently uncertain |
| `methodology` | A reasoning-process lesson learned from hypothesis outcomes |

**Pragmatic types** (`kind: pragmatic` — prescriptive; **no default**):

| Type | Meaning |
|---|---|
| `policy` | A mandatory rule that should always be followed |
| `heuristic` | A rule-of-thumb that biases decisions but is not absolute |

The asymmetry is deliberate: a `kind: pragmatic` file with no `type` is a
**parse error**, while an epistemic file with no `type` falls back to
`observation`.

Writing `kind: epistemic` explicitly is legal to read but is not a fixed
point under rewrite — conformant writers never emit it.

### 2.5 Origin

`origin` is a third classification axis, orthogonal to `kind` and `type`:
where `kind`/`type` describe *what a fact says*, `origin` records *how it
came to exist*.

| Origin | Meaning |
|---|---|
| `authored` | Hand-written by a human, or by an agent via the learn operation. The default. |
| `distilled` | Produced by synthesis from existing facts. |
| `discovered` | Emergent — inferred by a discovery process; a fact nobody wrote down. |

#### 2.5.1 Type-Aware Resolution

A missing `origin` resolves by the **resolved leaf type**: `type: synthesis`
resolves to `distilled` (every pre-`origin` synthesis fact in existing
corpora was pipeline-distilled); every other type resolves to `authored`.
Because resolution keys on the *resolved* type, a file with no `type` at all
resolves the type first (`observation`) and then the origin (`authored`).
Note `hypothesis` defaults to `authored` even though `discovered` is also
legal on it.

#### 2.5.2 Legality

| Origin | Legal on |
|---|---|
| `authored` | every type |
| `distilled` | `synthesis` only |
| `discovered` | `synthesis` and `hypothesis` only |

#### 2.5.3 The Elision Rule

Whether the `origin` line is written is **not** "omit when equal to the
default":

| Origin value | Type | `origin` line | Why |
|---|---|---|---|
| unset | any | omitted | An unset origin means "let the reader resolve it" (§2.5.1). |
| `authored` | any non-synthesis | omitted | Keeps pre-origin corpora byte-identical under rewrite. |
| `authored` | `synthesis` | **written** | Omitting it would resolve on read to `distilled`, silently converting a human-authored synthesis fact into pipeline output. |
| `distilled` | `synthesis` | **written** | Matches the read default, but omitting it would churn the frontmatter of the most common synthesis fact in existing corpora. |
| `discovered` | `synthesis`, `hypothesis` | written | Never a default; always explicit. |

#### 2.5.4 Read Normalizes, Write Rejects

Origin validation is asymmetric by design:

- **Readers MUST NOT fail** on an unknown origin or an illegal origin × type
  pairing. They MUST normalize it to the type's default origin (§2.5.1).
- **Writers MUST reject** an unknown origin or illegal pairing.

Rationale: repositories exist containing illegal pairings committed before
enforcement existed. Failing on read would make those files permanently
unreadable, with no repair path — the repair itself requires reading the
fact. Type is authoritative, origin yields to it, the normalized value is
always legal, and the next write self-heals the file. Strict writing stops
the corpus accumulating more bad pairings.

### 2.6 Validation Summary (Per-Axis)

The read/write asymmetry is **per-axis**, not a blanket rule in either
direction:

| Axis | On read | On write |
|---|---|---|
| `kind`, `type` | strict, parse error | strict, reject |
| `confidence` ∈ [0, 1] inclusive | strict | strict |
| `sources` >= 0 | strict | strict |
| `evidence_weight` | no bounds check | no bounds check; only gates emission on `> 0` |
| `origin` | normalized silently (§2.5.4) | strict, reject |

Because `confidence` and `sources` are strict in both directions, an
out-of-range value cannot survive a round-trip, so a conformant writer can
never persist one.

### 2.7 Title and Body

The title is **not frontmatter** — it is the first `#` heading of the
markdown part. On parse, the first line of the trimmed body region MUST
start with `#`; all leading `#` characters are stripped, the result is
whitespace-trimmed, and the trimmed title MUST be non-empty. On serialize,
the title is always written as exactly `# ` + title.

**Title heading level is therefore lossy**: `## Foo` parses to title "Foo"
and re-serializes as `# Foo`.

The body is everything after the title line, whitespace-trimmed.

### 2.8 List Serialization and Escaping

Enum values (`kind`, `type`, `origin`) and **every list item** MUST be
emitted so that YAML reads them back as strings — in practice, quoting any
item YAML would otherwise reinterpret. Without this, `entities: [No, yes,
null, true]` reads back as booleans and a null, with the null typically
dropped entirely. Numeric fields, by contrast, are emitted bare (`0.85`,
`1`). Readers must accept both quoted and bare list items.

### 2.9 References (`refs`)

Each ref is a plain string, stored and returned **verbatim** — never
rewritten, resolved, or normalized on read or write. There are three forms:

| Form | Meaning | Example |
|---|---|---|
| Local fact path | Another fact in this repository | `kb/technology/software/a1b2c3d4.md` |
| External URL | An external resource | `https://example.com/source` |
| Cross-repo pointer | A fact in another knomit repository | `kb://1a2b3c4d5e6f/kb/gotchas/store/9f8e7d6c.md` |

Classification: a ref is **local** when it does *not* start with `kb://` and
*does* end with `.md`; everything else is external. The order of those two
tests is load-bearing — a `kb://` ref ends in `.md` but MUST classify as
external. A suffix-only classifier would miscount cross-repo pointers as
local provenance and pollute evidence-weight computation (§5.2). When refs
are gathered as evidence sources, a fact's own path is excluded — a fact is
never its own evidence source.

The cross-repo form is `kb://<id12>/<repo-relative-path>`, where `<id12>` is
the first **12 lowercase-hex characters of the target repository's
root-commit hash** (§4.8) and the path remainder is non-empty. Do not
conflate the two identifier lengths in play: repo ids are 12 hex characters
of a commit hash; fact *filenames* are 8 characters of a UUID (§3.5).

Refs are one of **two evidence mechanisms**:

- **Implicit (Git-native):** facts committed together in one learning moment
  are evidence for each other. A `learn` commit may contain multiple files;
  all facts in that commit are implicitly linked.
- **Explicit (`refs`):** when a fact is derived from other facts, `refs`
  carries the source fact paths. This is the only cross-fact lineage
  recorded in the file itself — Git supplies *version* lineage (the history
  of one path), not *derivation* lineage (which facts fed another).

### 2.10 What Is NOT in the File

The following are intentionally omitted because Git supplies them (details
in §4.9):

| Concept | Git equivalent |
|---|---|
| Fact identity | File path within the ontology + repo root-commit hash |
| Fact version | The triple `(path, blob hash, commit)` |
| Creation date | Committer timestamp of the first commit touching the path |
| Last updated | Committer timestamp of the latest commit touching the path |
| Author / source agent | Commit author name and email |
| Operation type | Commit author email `+` subaddress |
| Modification history | First-parent commit ancestry of the path |
| Retraction | The file's deletion commit — absence at HEAD plus history |

### 2.11 Format Traps

1. The read/write asymmetry is per-axis (§2.6), not global.
2. Origin elision is not "omit the default" (§2.5.3).
3. `evidence_weight` looks authored but is derived (§5.2); clients never
   supply it.
4. Explicit `kind: epistemic` is legal input but never survives a rewrite.
5. The frontmatter split is textual, not YAML-aware (§2.1).
6. Title heading level is lossy (§2.7).

## 3. The Ontology

The ontology is the directory structure that determines what fact paths are
valid.

### 3.1 Topic-Based Hierarchy

Facts live in a two-level hierarchy — **topic** → **category** → fact files —
under the ontology root (default `kb/`):

```
kb/
  kb.md                              ← root manifest (not a fact; §3.8)
  people/
    individuals/
      a1b2c3d4.md                    ← fact file (8-char UUID filename)
      e5f6g7h8.md
    relationships/
      i9j0k1l2.md
  technology/
    software/
      m3n4o5p6.md
domains/
  ontology.yaml                      ← ontology definition (outside the root; §3.2)
```

### 3.2 The Definition File

The ontology is defined in **`domains/ontology.yaml`** — at the top level of
the repository tree, **outside the ontology root**. The location is fixed;
it does not move if the ontology root differs from `kb`.

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
```

Schema: the root carries `id`, `name`, `description`, `topics`, and an
optional `validations` list; each node carries `description`, optional
`children`, and optional `validations`; each validation carries `name`,
`message`, and `rule` (§3.4). Required: `id`, `name`, and at least one
topic.

Topic and category keys MUST match `^[a-z0-9]+(-[a-z0-9]+)*$` (lowercase
kebab-case) at every depth. Writers do not necessarily enforce the grammar
below the first two levels, so deeper keys violating it may exist in real
repositories; readers MUST tolerate them.

The file may be absent or unparseable, in which case the default taxonomy
applies (§3.3). The copy on the branch being read is the one in force.

### 3.3 Default Taxonomies

Two taxonomies are conventional, identified by the `id` field. A client will
encounter one of these — possibly extended — in most repositories, but MUST
NOT assume either: always read `domains/ontology.yaml`.

**`general`** (id `general`) — 13 topics, no validation rules. The default
for new general-purpose repositories:

- people: individuals, groups, relationships
- technology: software, hardware, networking, data
- science: formal, natural, applied
- society: economics, politics, law, education
- culture: art, literature, music, design, language
- geography: physical, political, urban, virtual
- history: ancient, modern, ongoing
- health: medicine, wellness, public-health
- philosophy: metaphysics, epistemology, ethics
- religion: traditions, spirituality, theology
- business: organizations, products, markets, operations
- reference: standards, measurements, terminology
- meta: reasoning

**`source-code`** (id `source-code`) — 8 topics, for agents working inside a
codebase:

- invariants: architecture, data, protocol, concurrency
- architecture: modules, flows, integrations
- conventions: testing, logging, errors, naming, git
- decisions: accepted, superseded, rejected
- gotchas: tools, libraries, runtime
- incidents: bugs, near-misses
- meta: reasoning, ontology
- principles: mission, philosophy, anti-patterns, ux — carries all shipped
  validation rules (§3.4)

`meta` exists in **both** presets with **different children** (`reasoning`
alone vs `reasoning` + `ontology`). Methodology facts (§5.1) are always
written to topic `meta`, category `reasoning`; a custom ontology intended to
support them must provide that address.

A stored ontology whose `id` matches an embedded preset and whose content is
a **subset** of it (every topic key, every child key, every validation
*name* present in the preset — validations match by name only, which is how
presets deliver fixes to existing rules) may be auto-refreshed to the newer
preset. The refresh appears in history as a commit with message
`ontology: refresh to embedded <id> preset` under operation token `updated`
(§4.2). If the stored ontology has diverged, it wins and is left alone.

### 3.4 Validation Rules

Any ontology node — root, topic, or child — may declare a `validations`
list. Each rule is a **JavaScript boolean expression** evaluated against a
fact at write time; a fact failing any applicable rule is rejected with the
rule's `message`.

- The expression is evaluated with a single variable in scope, `fact`,
  exposing exactly: `kind`, `type`, `domain` (array), `entities` (array),
  `refs` (array), `title`, `body`, `path` (normalized lowercase), and
  `confidence`. Nothing else.
- Rules are pure expressions: no I/O, no host access, bounded execution.
- The result is coerced by JavaScript truthiness. The first failing rule
  rejects the write; a rule that throws also rejects.
- Rules attach to the ontology node matching the fact's topic path; a fact
  under an **unknown topic matches no node and passes vacuously**.

The `general` preset ships no rules. The `source-code` preset ships exactly
four, all on `principles`:

```yaml
- name: must-have-designer-entity
  message: "principles must be authored via /knomit-principle (entities must include 'designer')"
  rule: "fact.entities.includes('designer')"
- name: must-be-pragmatic-policy
  message: "principles must declare kind=pragmatic and type=policy"
  rule: "fact.kind === 'pragmatic' && fact.type === 'policy'"
- name: domain-mutually-exclusive
  message: "domain must be either ['global'] or an area path, never both"
  rule: "!(fact.domain.includes('global') && fact.domain.length > 1)"
- name: domain-non-empty
  message: "domain is required"
  rule: "fact.domain.length > 0"
```

### 3.5 Fact Placement

Path format: `<ontologyRoot>/<topic>/<category>/<uuid8>.md`.

- **Fact filenames are writer-generated**: the first 8 characters of a v4
  UUID, plus `.md`. Agents supply topic, category, title, and body; the
  writer assigns the path. There is no collision check — a filename
  collision silently overwrites — so writers must generate fresh random
  ids.
- `category` may itself contain slashes, so real paths can exceed four
  segments.
- Path validation is minimal: non-empty and no `..` segments. The
  kebab-case grammar of §3.2 governs ontology *keys*, not fact paths.
- The **entire path is lowercased** when committed, so the on-disk tree is
  always fully lowercase. Readers should tolerate legacy mixed-case trees
  by matching paths case-insensitively.
- The `<uuid8>.md` shape is a **writer convention, not a validated
  invariant**: nothing checks filename shape or segment depth at read time.

### 3.6 Enforcement Is Uneven — Read Defensively

Ontology placement and validation rules are enforced by writers, not by Git,
and not every writer enforces them. A real repository may therefore contain:

- facts under topics that do not appear in the ontology (the directory
  simply exists in the tree);
- facts that violate declared validation rules.

Readers MUST tolerate both. A fact's presence in the tree — not its ontology
conformance — determines whether it is part of the corpus. Conformant
writers SHOULD validate placement against the ontology and run the
applicable validation rules before committing.

### 3.7 The Ontology Root

Default `kb`. The root is a server-side configuration and is **not recorded
in the repository**; a reader identifies it as the top-level directory
containing topic directories of fact files (alongside `domains/` and any
top-level manifest). Roots other than `kb` are possible but uncommon.

### 3.8 Non-Fact Files

Two non-fact files exist by convention: `kb.md` — the root manifest, a plain
markdown file with no frontmatter, describing the knowledge base; it is part
of the repository's root commit — and `domains/ontology.yaml`.

**Exclusion is parse-failure-based, not path-based.** There is no list of
excluded paths; any file that fails fact parsing (§2.1) is simply not a
fact. The consequence cuts both ways: a stray file that *does* carry valid
frontmatter and a title heading is a fact wherever it sits, under any
filename, and readers MUST treat it as one.

## 4. Git Conventions

§1.1 gave the mapping from learning acts to Git effects. This section
specifies the conventions layered on it: how commits are labelled with
operations, how agents are identified, which branches may be written, and
how history is read back.

### 4.1 Operation Labelling

Commits are labelled with their learning operation by **email subaddressing
in the commit author field**. The committer field carries the stable agent
identity with no operation:

```
Author:    <agent-id> <<agent-id>+<operation>@agents.knomit.io>
Committer: <agent-id> <<agent-id>@agents.knomit.io>
```

`<agent-id>` is the agent branch name with the `agent/` prefix stripped
(branch `agent/laptop-3f9a2b1c` → agent-id `laptop-3f9a2b1c`).

Decoding is domain-agnostic: the operation is everything between the first
`+` and the `@` of the author email, whatever the domain. A human using
ordinary Git tooling therefore participates by putting `+<op>` in their own
email's local part — `Bob <bob+learn@gmail.com>` decodes as a `learn`.
There is no reserved human email format and no enforcement of the convention
on foreign commits.

Commit **messages** are human-oriented, not machine-readable convention:
`learn: <moment>`, `update: <title>`, `retract(<moment>): <path>`,
`merge: <src> into <dst>`. Clients MUST NOT parse them for semantics.

### 4.2 The Operation Vocabulary

The tokens in use:

| Token | Emitted by |
|---|---|
| `learn` | A batch commit of newly learned facts |
| `update` | Fact modification, synthesis confidence adjustments, and fact creation through some write interfaces |
| `retract` | **Every** fact deletion, whoever initiated it |
| `subsume` | Merge and synthesis writes: learn-time dedup merges, synthesis merges, and new synthesized facts |
| `review` | The methodology batch written by reflection |
| `discover` | Facts landed by the discovery process |
| `merge` | Reconcile merge commits (consensus merged into the agent branch) |
| `replay` | Cross-store fact replay when connecting previously disjoint instances |
| `updated` | Ontology refresh (§3.3) — an inconsistency: a `+update@` filter misses these |

Tokens a reader might expect that do **not** exist: there is no
`synthesize`/`synthesis` (synthesis lands as `subsume`), no `hypothesize`
(hypotheses ride the learn operation), no `prune`/`distill`/`reflect`
(internal phase names, never tokens), and no `create` (fact creation commits
as `update`). **Init commits carry no token at all** — author
and committer are `knomit <knomit@local>`, so operation decoding yields the
empty string.

Only `retract` is guaranteed: every deletion carries it. The rest of the
vocabulary is convention, not schema — which is how the `updated` drift
happened. Writers SHOULD emit the table above verbatim; readers MUST
tolerate unknown tokens.

### 4.3 Agent Identity

Each agent operates on a long-lived personal branch:

```
agent/<sanitized-hostname>-<fingerprint8>
```

- The hostname is the agent machine's hostname with characters invalid in
  Git refs (space, `~`, `^`, `:`, `?`, `*`, `[`, `\`) replaced by `-`; an
  unavailable hostname falls back to `local`.
- The fingerprint is **8 hex characters** derived from the agent's own
  long-lived key. The key, not the host, is the identity: two agents on one
  host get distinct branches, and an agent keeps its branch across renames.

Commits may carry a signature. Signatures are informational — nothing in the
protocol verifies them, and clients are not required to sign.

```
main                          ← consensus (§4.4)
agent/laptop-3f9a2b1c         ← agent on host "laptop", key fp 3f9a2b1c
agent/server-7d4e0a55         ← agent on host "server", key fp 7d4e0a55
```

Agent branches are long-lived: one branch per agent, many learning moments
per branch, each moment identified by its commit's author signature.

### 4.4 The Branch Model and Consensus

The **consensus branch** represents accepted truth. Its name defaults to
`main` and is configurable per remote. When first connecting to a remote, a
client selects the consensus branch by preference: a branch literally named
`main`, else the remote's default (HEAD) branch, else `main` — deliberately
ordered so that a remote whose HEAD points at an `agent/*` branch does not
become consensus.

The consensus branch is written by exactly two things: repository
initialization (the root commit, §4.8) and reconciliation, which updates a
client's local copy to match the remote's. It is never a fact-write target,
and clients never push it.

**Agent-to-consensus promotion is not implemented.** Every merge in the
current protocol runs the other direction — consensus merged *into* the
agent branch. Nothing advances the remote consensus branch; a client's local
consensus is a read-only tracking copy. Promotion of agent knowledge into
consensus is a designed capability (remote-side, never an agent push) and
must be treated as **aspirational** in any description of current behavior.

Rules, restated:

- Agents never commit to the consensus branch. Each agent force-pushes only
  its own `agent/<hostname>-<fingerprint>` branch.
- Learn batches multiple facts into a single commit; update and retract
  operate per-fact.
- Fact evolution is in-place: the same file is edited and recommitted, and
  history shows the evolution.

### 4.5 Branch Write-Eligibility

A client MUST write only to **its own agent branch**:

- never the consensus branch — authoring there bypasses reconciliation and
  the watermark model (§4.7);
- never another agent's branch — authoring there corrupts that agent's
  reconciliation state.

Even when a client is reading from some other branch (a pinned read anchor),
its writes still target its own agent branch. This rule is convention, not
something Git enforces, and not every write interface checks it — so a real
repository may contain violations. A conformant client follows it regardless.

### 4.6 Writing and Publishing

A **learn** is one commit containing all facts of the learning moment,
together with any retractions its dedup pass produced (§5.3) — all or
nothing. An **update** is a single-file commit after re-validating the
merged result. A **retract** is a deletion commit. All committed paths are
lowercased (§3.5). Every commit carries the author/committer convention of
§4.1.

**Publishing is decoupled from writing.** Commits accumulate locally and are
published on their own cadence, so a push batches many commits rather than
tracking each write. Publishing a batch means: fetch the remote, update the
local consensus copy, merge consensus into the agent branch (as a
`merge`-token merge commit, or by replaying agent commits on top — replay
rewrites the committer but preserves the original author, author timestamp,
and message, so operation and agent attribution survive), then push the
agent branch.

The remote relationship is narrow, not a mirror: a client pushes only its
own agent branch, and fetches only the consensus branch and that same
branch. Force-pushing the agent branch is safe precisely because no one else
ever writes it.

### 4.7 Reading and History Semantics

**Reads never pull.** A client reads its local clone at the agent branch's
HEAD; freshness comes only from the background fetch cycle.

History semantics are **first-parent**: the revision lineage of a fact is
the first-parent commit chain of its branch, which keeps the merged-in side
of reconcile merges from shadowing the branch's own chronology. Wall-clock
timestamps are never used for ordering.

Reading a fact "as of" a commit supports a **before** mode, which is
exclusive and first-parent: start at the anchor commit's first parent, then
resolve the most recent commit at or before it on the first-parent chain in
which the path was added or modified, **stepping over deletions**. A
retracted fact therefore still resolves to its last valid version; only a
path never added in the ancestry is not-found. When the fallback applies,
report the ancestor commit actually shown, not the anchor.

Reconciliation needs to know which consensus commit an agent branch last
merged from. That bookkeeping is kept in refs outside `refs/heads/`, so it
never rides a push and is invisible to a plain clone. A client MUST NOT
delete or rewrite refs outside `refs/heads/` that it does not own.

### 4.8 Repository Identity

A repository's identity is **its root-commit hash**, reached by a
first-parent walk from the branch head. The 12-lowercase-hex prefix of that
hash is the short form used in `kb://` refs (§2.9). Identity survives
cloning and renaming: every clone shares the root commit, and the repo name
is never part of the identity.

Uniqueness comes from a **nonce in the init commit message**:

```
init: create knowledge base

knomit-repo-nonce: <uuid>
```

The nonce is required because Git timestamps have second precision and
everything else in the init commit is fixed — without it, two repositories
created in the same second could collide. Init commits are authored
`knomit <knomit@local>` and carry no operation token.

One race is documented and accepted: two clients initializing the same
*empty* remote each mint distinct nonces, producing distinct identities — a
silent split-brain, undetectable at push time because each pushes only its
own agent ref and neither pushes consensus.

### 4.9 What Git Supplies Instead of File Fields

The authoritative mapping behind §2.10:

| Concept | Git mechanism |
|---|---|
| Identity | File path + repo root-commit hash; a *version* is `(path, blob hash, commit)` |
| Creation time | Committer timestamp of the first commit touching the path |
| Update time | Committer timestamp of the latest commit touching the path |
| Author / agent | Commit author name and email |
| Operation | `+` subaddress of the author email (§4.1) |
| Revision lineage | First-parent commit ancestry |
| Retraction | The deletion commit; a fact is "not live" when absent at HEAD but present in history |

Cross-fact derivation lineage (`refs`) is the one lineage kind carried in
the file; Git supplies only version lineage.

### 4.10 Querying by Operation

All of the following run against a clone (§1.2):

```sh
# All learn operations from agents
git log --author='+learn@agents.knomit.io' --oneline agent/laptop-3f9a2b1c

# All retractions, agent and human alike (domain-agnostic)
git log --author='+retract@' --oneline

# Everything one agent did (author NAME is the agent-id)
git log --author='^laptop-3f9a2b1c ' --oneline

# All agent commits of any operation
git log --author='@agents.knomit.io' --oneline

# One fact's history (paths are lowercase on disk)
git log --first-parent --format='%H %ae %cI %s' -- kb/gotchas/store/1a2b3c4d.md

# Operation frequency census
git log --format='%ae' | sed -n 's/.*+\(.*\)@.*/\1/p' | sort | uniq -c
```

Caveats:

1. Knomit's history semantics are **first-parent** (§4.7). Plain `git log`
   also descends the merged-in side of reconcile merges; add
   `--first-parent` to match.
2. Filter on `--author`, not committer: the committer carries no operation,
   and replay rewrites the committer while preserving the author (§4.6).
3. A `+update@` filter misses the `updated` token (§4.2).
4. Merge commits carry `+merge@` authors, so broad operation greps surface
   machine-generated reconcile merges alongside learning operations.
5. Init commits match no operation filter at all.

## 5. Synthesis in the Repository

Synthesis refines the corpus — removing redundancy, extracting higher-order
patterns, recording methodology. Its scheduling and internals are out of
scope; what is normative here is what it writes to the repository and under
which token, plus two derived-data rules every writer shares: the
evidence-weight formula and the dedup merge.

### 5.1 What Synthesis Writes

| Outcome | File effect | Operation token |
|---|---|---|
| Keep a fact | none | — |
| Retract a fact | delete the file | `retract` |
| Adjust confidence | rewrite the file with the new confidence only | `update` |
| Merge duplicate facts | write the merged fact; delete the sources | `subsume` (write), `retract` (deletes) |
| Synthesize a higher-order fact | write a new fact with `refs` to its sources | `subsume` |
| Record methodology | one atomic batch of `methodology` facts under `meta/reasoning` | `review` |
| Land a discovery | write the discovered fact(s) | `discover` |

File-visible rules:

- **Newly synthesized facts get fresh writer-assigned paths** (directory +
  new 8-char UUID filename). **Merge outputs do not** — the merged fact may
  keep a proposed path, only root-validated and lowercased.
- **`sources` on every derived output is pooled, never proposed.** A
  synthesized fact sums the `sources` of the local facts it cites; a merge
  output sums the `sources` of the facts it subsumes, computed **before those
  facts are deleted**. This follows from the §2.2 definition — a fact derived
  from N others is corroborated by everything those N rest on — and makes the
  synthesis merge agree with the dedup merge of §5.3, which already sums. The
  count a writer *proposes* for a derived fact is never trusted: the sources
  it pools are deleted or outlive it, so the repository, not the proposer, is
  the authority on what the output rests on.
  - The pooled set is **exactly** the set §5.2 weighs: sources that fail to
    read contribute nothing, and hypothesis-typed sources are skipped. A
    conjecture corroborates nothing, and letting the two numbers disagree
    would have them describe different evidence.
  - The pooled count **floors at 1**. An output that cites no readable local
    source is still one act of synthesis, and a 0 would erase it from every
    downstream weight — §5.2 multiplies by `sources`, so a 0-source fact
    contributes nothing no matter how confident it is.
- Synthesis never emits `type: hypothesis`; hypotheses enter only via the
  learn operation or discovery.
- Synthesis writes may omit `origin`, in which case the read default of
  §2.5.1 applies: an output typed `synthesis` reads back `distilled`, but
  one typed anything else reads back `authored`.

### 5.2 `evidence_weight`

`evidence_weight` is the derived corroboration score on merged and
synthesized facts:

```
weight = Σᵢ (confidenceᵢ × sourcesᵢ) / (Σᵢ (confidenceᵢ × sourcesᵢ) + 1)     ∈ [0, 1)
```

summed over the output's cited **local** source facts (§2.9
classification), read from the repository **at write time, before the
source facts are deleted** — computing after deletion reads nothing and
silently zeroes the weight. Sources that fail to read contribute nothing.
**Hypothesis-typed sources are skipped entirely.** An empty source set
yields 0, which elides the field (§2.2).

It is computed wherever merged or synthesized facts are written, and also
on learn for facts arriving with `origin` of `distilled` or `discovered`
that cite local refs — so a synthesis proposal saved through the learn path
carries the same weight direct application would have produced. Ordinary
authored learns never get one.

### 5.3 Deduplication on Learn

Each incoming fact is checked against **its own category directory** for a
single best near-duplicate by semantic similarity. The similarity measure
and threshold are implementation-calibrated and not part of this
specification; what is normative is the outcome a writer must produce:

1. **No near-duplicate** — the fact is written as-is at a fresh path.
2. **The near-duplicate is a hypothesis and the incoming fact is not** —
   the incoming fact is written at its **own** fresh path with the
   hypothesis's path added to its `refs`, and the hypothesis is retracted
   **in the same commit** (the hypothesis is resolved by the observation).
3. **Genuine duplicate** — a merged fact is written **at the existing
   fact's path**; no new file is created.

Merge semantics: the **winner contributes identity** — title, body, `kind`,
`type`, `origin`. Metadata always **pools** regardless of winner:

| Field | Merged value |
|---|---|
| `domain`, `entities` | union |
| `confidence` | max of the two |
| `sources` | sum of the two |
| `refs` | union, plus the existing fact's on-disk path (verbatim, un-normalized — a lowercased ref to a legacy mixed-case file would dangle) appended as a lineage ref |

Winner rule: higher confidence wins; on a confidence tie the incoming fact
wins unless it has strictly fewer sources.

## 6. Full Example

### 6.1 Repository State

```
kb/
  kb.md
  people/
    individuals/
      a1b2c3d4.md          ← "Alice likes rock music"
    interests/
      e5f6g7h8.md          ← "Alice's music taste shifts seasonally"
  geography/
    urban/
      i9j0k1l2.md          ← "London rain in April"
domains/
  ontology.yaml
```

### 6.2 A Ground-Level Fact

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

No `kind` line (epistemic default), no `evidence_weight` (authored, zero),
no `origin` line (`authored` on a non-synthesis type is omitted, §2.5.3).

### 6.3 A Synthesized Fact

`kb/people/interests/e5f6g7h8.md`

```yaml
---
type: synthesis
domain: [personal, music, behavioral_patterns]
confidence: 0.72
sources: 1
evidence_weight: 0.84
origin: distilled
entities: [alice, music_taste, seasonal_patterns]
refs: [kb/people/individuals/a1b2c3d4.md, kb/people/individuals/m3n4o5p6.md, kb/people/individuals/q7r8s9t0.md]
---
# Alice's music taste shifts seasonally

Alice shows a consistent pattern of shifting music preferences
with the seasons. Rock and metal dominate in summer months
around festival season, while hip-hop listening increases
in winter.
```

`sources: 1` because synthesized facts force it (§5.1). `origin: distilled`
is written explicitly despite matching the read default (§2.5.3).
`evidence_weight: 0.84` follows from the formula: with source products
summing to 5.25, `5.25 / 6.25 = 0.84` (§5.2).

### 6.4 The Commit View

In a clone, the learn commit that produced §6.2 looks like:

```
$ git log --first-parent --format=fuller -1 -- kb/people/individuals/a1b2c3d4.md
commit 8c1f0a2e...
Author:     laptop-3f9a2b1c <laptop-3f9a2b1c+learn@agents.knomit.io>
AuthorDate: ...
Commit:     laptop-3f9a2b1c <laptop-3f9a2b1c@agents.knomit.io>
CommitDate: ...

    learn: alice's listening habits
```

The operation (`learn`), the agent identity (`laptop-3f9a2b1c`), the
timestamps, and the batch grouping (`git show --stat` lists every fact of
the learning moment) are all read from the commit — none of it appears in
the file.
