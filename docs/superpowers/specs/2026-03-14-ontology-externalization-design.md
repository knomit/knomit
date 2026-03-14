# Externalized Ontology Definition

## Problem

Topics are hardcoded as Go constants in `internal/fact/topic.go` — 12 fixed values compiled into the binary. Every knomit repo gets the same ontology with no way to customize it. The ontology should be a data file that travels with the repo, chosen at init time.

## Decision

Externalize the ontology into a YAML file stored in git at `domains/ontology.yaml`. A default ontology is embedded in the binary and written on init. Users can optionally provide their own ontology definition when initializing a repo. Once initialized, the ontology is loaded from git on every open.

## Ontology Model

The model and YAML parsing live in `internal/fact/`.

### Structs

```go
// Ontology defines the topic hierarchy for a knomit knowledge base.
type Ontology struct {
    ID          string                   `yaml:"id"`
    Name        string                   `yaml:"name"`
    Description string                   `yaml:"description"`
    Topics      map[string]*OntologyNode `yaml:"topics"`
}

// OntologyNode is a single node in the ontology tree.
type OntologyNode struct {
    Description string                   `yaml:"description"`
    Children    map[string]*OntologyNode `yaml:"children,omitempty"`
}
```

### Functions

- `ParseOntology(data []byte) (*Ontology, error)` — YAML parsing + validation:
  - `id` and `name` are required
  - At least one topic must be defined
  - Topic and node keys must be lowercase, alphanumeric with hyphens only (URL-safe slugs)
  - Tree depth is unbounded (arbitrary nesting of children)
- `(*Ontology) ValidatePath(path string) error` — anchored validation: walk path segments against the tree as deep as nodes are defined, allow freeform below the last matched node.
- `(*Ontology) Serialize() ([]byte, error)` — round-trip back to YAML with sorted keys for deterministic output (no spurious git diffs).
- `(*Ontology) TopicNames() []string` — stable-sorted list of top-level topic keys.
- `DefaultOntology() *Ontology` — returns the embedded default (parsed once via `sync.Once`).

### Anchored Validation

`ValidatePath` splits the path into segments and walks the ontology tree:

1. The first segment must match a top-level topic key.
2. For each subsequent segment, if the current node has a `Children` entry matching the segment, descend into it.
3. If the current node has no matching child, stop — remaining segments are freeform and accepted.
4. An empty path or a path with no matching first segment is an error.

Examples with the default ontology:
- `technology` — valid (matches topic)
- `technology/software` — valid (matches topic + child)
- `technology/software/go/concurrency` — valid (matches through `software`, freeform below)
- `technology/quantum` — valid (`technology` exists, `quantum` is freeform below it)
- `cooking` — invalid (no such topic)

### Embedded Default

A default YAML file is embedded via `//go:embed ontology_default.yaml`.

`DefaultOntology()` parses it once (via `sync.Once`) and returns the result. This is used when no custom ontology is provided at init time.

## Default Ontology YAML

File: `internal/fact/ontology_default.yaml`

12 topics with 2 levels of depth, aligned with Wikipedia's Main Topic Classifications:

```yaml
id: general
name: General Knowledge
description: >
  A broad-purpose taxonomy for organizing knowledge by subject area,
  aligned with Wikipedia's Main Topic Classifications.
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
      networking:
        description: Protocols, connectivity, distributed systems
      data:
        description: Databases, storage, data engineering
  science:
    description: Formal, natural, and applied sciences
    children:
      formal:
        description: Mathematics, logic, CS theory
      natural:
        description: Physics, chemistry, biology, earth science
      applied:
        description: Engineering, medicine, agriculture
  society:
    description: Economics, politics, law, education
    children:
      economics:
        description: Markets, trade, finance
      politics:
        description: Governance, policy, international relations
      law:
        description: Legal systems, regulation, rights
      education:
        description: Learning, academia, pedagogy
  culture:
    description: Art, literature, music, design, language
    children:
      art:
        description: Visual arts, performing arts, film
      literature:
        description: Writing, poetry, publishing
      music:
        description: Composition, performance, genres
      design:
        description: Graphic, industrial, UX design
      language:
        description: Linguistics, translation, communication
  geography:
    description: Physical, political, and virtual spaces
    children:
      physical:
        description: Landforms, climate, natural features
      political:
        description: Countries, regions, borders
      urban:
        description: Cities, infrastructure, planning
      virtual:
        description: Digital spaces, online communities
  history:
    description: Past and ongoing events
    children:
      ancient:
        description: Pre-medieval events and civilizations
      modern:
        description: Post-medieval to present
      ongoing:
        description: Current events, developing situations
  health:
    description: Medicine, wellness, public health
    children:
      medicine:
        description: Diagnosis, treatment, pharmacology
      wellness:
        description: Fitness, nutrition, mental health
      public-health:
        description: Epidemiology, policy, systems
  philosophy:
    description: Metaphysics, epistemology, ethics
    children:
      metaphysics:
        description: Nature of reality, existence
      epistemology:
        description: Knowledge, belief, justification
      ethics:
        description: Morality, values, normative theory
  religion:
    description: Traditions, spirituality, theology
    children:
      traditions:
        description: Organized religions, denominations
      spirituality:
        description: Personal practice, mysticism
      theology:
        description: Doctrine, scripture, comparative study
  business:
    description: Organizations, products, markets, operations
    children:
      organizations:
        description: Companies, nonprofits, institutions
      products:
        description: Goods, services, brands
      markets:
        description: Industries, competition, trends
      operations:
        description: Management, logistics, strategy
  reference:
    description: Standards, measurements, terminology
    children:
      standards:
        description: Specifications, protocols, conventions
      measurements:
        description: Units, metrics, benchmarks
      terminology:
        description: Glossaries, definitions, nomenclature
```

## Git Init Changes

### InitWithStorer

New signature:

```go
func InitWithStorer(s *storegit.Storer, initFiles map[string]string) (*Store, error)
```

The root manifest and all `initFiles` entries are assembled into a single tree (via iterative `upsertEntry` calls) before creating one init commit. When `initFiles` is nil, only the manifest is created.

The root manifest filename changes from `general.md` to `kb.md` to match the new default `ontologyRoot`.

`Init(dbPath)` also gains the `initFiles` parameter.

### Callers

**`initCmd`**: accepts optional `--ontology` flag (path to a YAML file on disk). If provided, reads the file, parses it via `fact.ParseOntology()` for validation, then serializes back to YAML. If not provided, uses `fact.DefaultOntology()`. Passes `{"domains/ontology.yaml": serializedYAML}` to `InitWithStorer`.

**`serveCmd` auto-init path**: uses `fact.DefaultOntology()`, passes the file map to `InitWithStorer`.

### On Open

After `git.OpenWithStorer()` succeeds, read `domains/ontology.yaml` from git via `gs.ReadFile()` and parse with `fact.ParseOntology()`. The resulting `*Ontology` is passed to all handlers.

If `domains/ontology.yaml` is missing (pre-ontology repo), fall back to `fact.DefaultOntology()` and log a warning. The repo is not modified.

## Downstream Plumbing

### ontologyRoot

`config.OntologyRoot` stays in the config struct. Default changes from `"general"` to `"kb"`. This is the fixed filesystem prefix where facts live. It is independent of the ontology — it defines where facts are stored, not what topics are valid.

Path structure: `<ontologyRoot>/<topic>/<category>/<uuid>.md` → `kb/<topic>/<category>/<uuid>.md`

### Handler signatures

All handlers that currently accept `ontologyRoot string` now also accept `*fact.Ontology`:

```go
func NewServer(gs GitStore, idx SearchIndex, llmAdapter llm.LLMAdapter, profile string, ontologyRoot string, ontology *fact.Ontology) *server.MCPServer
```

- `LearnHandler(gs, idx, ontologyRoot, ontology)` — validates `topic + "/" + category` via `ontology.ValidatePath()` instead of `fact.Topic.Validate()`. The full combined path is validated so that defined subcategories are enforced while deeper segments remain freeform.
- `WhyHandler(gs, ontologyRoot)`  — unchanged (no topic validation)
- `UpdateHandler(gs, idx, ontologyRoot)` — unchanged (path is immutable on update, no ontology validation needed)
- `ExploreHandler(gs, ontologyRoot)` — unchanged
- `RetractHandler(gs, idx, ontologyRoot)` — unchanged
- `ProfileInstructions(profile, ontologyRoot, ontology)` — dynamically lists topics from `ontology.TopicNames()` with descriptions
- `web.NewRouter(...)` — `ontologyRoot` param stays, `*fact.Ontology` added

### buildFactPath

Signature changes from `buildFactPath(ontologyRoot string, topic fact.Topic, category string)` to `buildFactPath(ontologyRoot string, topic string, category string)` since `fact.Topic` is deleted. Uses `ontologyRoot` (from config) as the prefix. Default value changes from `"general"` to `"kb"`.

### Instructions

`baseInstructionsText` dynamically generates the topic list and descriptions from `*Ontology` instead of hardcoding them. Uses `ontologyRoot` for path examples.

## Scoping

### All fact operations scoped to ontologyRoot

`ListAll()`, index sync (`idx.Sync()`), fact gathering, browse — all operate under `ontologyRoot` (`kb/` by default). The ontology YAML at `domains/ontology.yaml` is naturally outside this prefix and is never processed as a fact.

Git remote sync covers the entire repo, including the ontology file.

### Deletions

- `internal/fact/topic.go` — deleted
- `internal/fact/topic_test.go` — deleted

All references to `fact.Topic`, `fact.AllTopics()`, `fact.TopicPeople`, etc. are replaced with `ontology.ValidatePath()` calls.

## Not In Scope

- Ontology mutation via MCP tools (read-only after init)
- HTTP API for passing ontology at init (will be added later)
- Multiple ontologies per repo
- Migration of existing repos (early-stage software — fresh init)
