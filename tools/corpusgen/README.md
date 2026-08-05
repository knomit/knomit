# corpusgen

Builds real, standalone knomit knowledge-base repos at a deliberately chosen
size and topic-diversity profile — through the actual write/embed path, not
mocks or in-memory fixtures.

## Why this exists

The bridge-discovery/YAKE keyword-extraction research
(`.claude/plans/yake-*.md`) repeatedly ran every calibration pass against
whatever the live corpus happened to contain (~96 facts in the
`knomit_hypothesize` pool, ~793 in `knomit_review`'s), and flagged corpus
size and lexical diversity as untested variables — e.g.
`yake-df-gate-corpus-relative-bounding-report.md` says outright that
large-corpus behavior "is simulated from the formulas" for lack of a real
corpus past ~1,000 facts to calibrate against.

corpusgen builds real repos at whatever size/diversity a test actually
needs, through the same code paths a live daemon uses (real ontology
validation, real fact serialization, real ONNX embeddings, a real
Louvain-clusterable `SIMILAR_TO` graph) — so `tools/calibrate bridges` (and
the live MCP daemon, and the web UI) can point at them directly.

## What it does

`corpusgen build`:

1. Resolves an ontology preset (`default` or `code`) and bootstraps a new
   repo at `<out>/core.db` — the same `store.Open` → `InitRepo` →
   `SetEmbedder` sequence `repos.Manager` uses, minus the server-lifecycle
   machinery (background sync, SSE observer) a batch tool has no use for.
2. Assigns every fact's **structure** — topic, category, kind/type,
   confidence, sources, and any shared-ref/keyword-overlap group membership
   — deterministically from `--seed`, independent of the LLM.
3. Batches those structural assignments to an LLM (the `claudecli` adapter
   by default — shells out to the `claude` CLI, no separate API key) to
   generate content, one commit per batch (not accumulated to the end — a
   later batch failing still leaves everything already generated durably
   saved).
4. Runs one terminal `IndexManager().Sync` after generation stops (whether
   it finished normally or was cut short by an error) to build the real
   embeddings + `SIMILAR_TO` graph + Louvain clusters `calibrate bridges`
   reads.
5. Writes a `MANIFEST.json` recording exactly how the corpus was built and
   how many facts actually landed.

### Two content sources

- **`--content-source real`** (default): the LLM must use its `WebSearch`
  tool and ground every fact in genuine search results, citing a real URL.
  Every cited URL gets an actual HTTP check (`verify.go`) before being
  trusted into the corpus — a fact with no verifiable citation is dropped
  and regenerated (corpusgen overprovisions ~30% extra slots to absorb
  this). Confirmed empirically that `claude -p --allowedTools WebSearch`
  performs genuine searches headlessly (verified against a query answerable
  only by a live search, not training-data recall) — but `WebFetch` is
  **not** authorized non-interactively, so the model works from
  search-result snippets only, not full page reads. Real mode is
  meaningfully slower (genuine searches per batch) and `--seed` no longer
  makes runs reproducible even structurally-adjacent — which real subjects
  get covered varies with what's actually findable each run.
- **`--content-source synthetic`**: the LLM invents plausible content. Fast,
  no network dependency beyond the CLI itself, but the information in the
  corpus isn't real — it's a fictional system the model makes up (e.g.
  `meridian-gateway`, `event-bus`, entirely invented). **Build this only
  deliberately, as a comparison point — synthetic corpora should not be used
  to draw conclusions about the bridge tool's real-world readiness.** Two
  concrete findings drove this: synthetic bridges are trivially "found"
  because corpusgen scripts the shared entities/keywords itself, and even a
  deliberate synthetic test of keyword-overlap detection produced zero
  YAKE-detected bridges despite the shared phrase genuinely appearing in
  every group member's body text (see the keyword-group note below).

### The keyword-overlap mechanic: two failed designs, and why

**Attempt 1**: assign each group a fixed phrase from a generic list
("technical debt", "race condition", etc.) and instruct every member to
weave it in. Failed empirically — the phrase landed verbatim in every
group member's body text, yet YAKE found zero keyword bridges. Root cause,
traced directly in `internal/synthesize/yake.go` rather than guessed: the
phrase was one incidental mid-paragraph mention, competing against other
candidate phrases in the same dense text for a document's very few
top-`yakeTopK` (5) slots — it simply never won that competition.

**Attempt 2**: since the problem seemed to be "not distinctive enough,"
instruct the group to agree on one specific named entity instead (a real
product, protocol, standard, vulnerability ID). Also failed — tested on
real content mentioning "QUIC" and "MCP" constantly, still zero keyword
bridges. Root cause: `yakeDedup` drops any single-word candidate whenever a
longer co-occurring phrase (`yakeMaxN = 2` is the implementation's phrase
length ceiling) scores at least as well — and natural prose almost always
supplies one. Single-word/acronym terms are **structurally** excluded here,
regardless of how prominent or distinctive they are; no amount of corpus
scale changes this.

**Current design**: what actually survives on real data (`cyberai-kb.db`'s
genuine keyword bridges — "billion valuation", "active exploitation", "data
center") is a two-word *descriptive* phrase that is the central subject of
an early sentence in its own document — not a named entity, and not an
incidental mention. `assignKeywordGroups` (`refs_pool.go`) groups slots
(windowed to land in the same LLM batch, minimum size 3 to clear the DF
gate's floor — see its doc comment) and the prompt
(`keywordGroupInstruction` in `llmgen.go`) asks the group to agree on one
shared two-to-three-word descriptive phrase and make it the opening
sentence's subject in every member, not a passing reference.

## Usage

Requires the real ONNX runtime (same as `tools/calibrate` — run `make setup`
first if `dist/lib` is empty) and the `claude` CLI on `PATH`:

```sh
ORT_LIB_PATH=dist/lib/libonnxruntime.dylib DYLD_LIBRARY_PATH=dist/lib \
  go run ./tools/corpusgen build \
    --out ~/knomit-corpora/narrow-100 \
    --size 100 --diversity narrow --ontology code --topic architecture
```

On Linux use `libonnxruntime.so` and `LD_LIBRARY_PATH` instead.

### Flags

| flag | default | meaning |
|---|---|---|
| `--out` | _(required)_ | target directory; the repo is written to `<out>/core.db` |
| `--size` | `100` | target fact count |
| `--diversity` | `narrow` | `narrow` (one fixed `--topic`) or `broad` (every real-world topic in the ontology — see below) |
| `--ontology` | `code` | `default` (general 13-topic) or `code` (source-code) preset |
| `--topic` | _(none)_ | leaf topic to generate into; required for `--diversity narrow`, ignored for `--diversity broad` |
| `--content-source` | `real` | `real` (WebSearch-grounded, HTTP-verified) or `synthetic` (LLM-invented — deliberate use only) |
| `--shared-refs-rate` | `0.05` | fraction grouped into shared-external-ref clusters (synthetic) or shared-research-angle clusters (real) |
| `--keyword-overlap-rate` | `0.05` | fraction of facts grouped into shared-keyword clusters |
| `--seed` | `42` | governs every non-LLM-content structural choice (reproducible structure, not byte-identical prose; real mode's *subject matter* isn't reproducible at all) |
| `--llm-model` | _(CLI default)_ | model passed to `claude -p --model` |
| `--model-cache` | `~/.knomit/models` | embedding model cache dir (models must already be cached — no download step here) |
| `--embed-model` | `embeddinggemma` | embedding model id |
| `--batch-size` | `8` (`5` if `--content-source real` and not set explicitly) | facts generated per LLM call |
| `--branch` | `main` | branch facts are written to (also the branch set as HEAD by `InitRepo`) |

Example real-mode invocation:

```sh
ORT_LIB_PATH=dist/lib/libonnxruntime.dylib DYLD_LIBRARY_PATH=dist/lib \
  go run ./tools/corpusgen build \
    --out ~/knomit-corpora/real-technology-100 \
    --size 100 --diversity narrow --content-source real \
    --ontology default --topic technology
```

### `--diversity broad`: does YAKE find anything domain/entity tags don't?

Direct inspection of a real `narrow` (single-topic) corpus's kept bridges
showed keyword (YAKE) bridges were both rarer and lower-quality than
domain/entity bridges (23 candidates / 6 kept / meanQ 0.503 for keyword vs.
37 candidates / 30 kept / meanQ 1.695 for entity, same corpus), and the
single best keyword bridge's members overlapped ~50% with an already-kept
domain bridge covering the same facts. That's expected on a narrow corpus:
every fact already shares strong domain/entity tags by construction, so a
body-text-derived keyword signal has little room to add anything a tag
doesn't already state.

`--diversity broad` (`buildBroadSlots` in `diversity.go`) exists to give
keyword bridges a fair test: it spreads facts round-robin across *every*
real-world topic in the ontology (not `principles`/`meta` — see
`excludedTopics` in `ontology_walk.go`), then shuffles that assignment
across the whole corpus so batch-sized windows — the unit
`assignKeywordGroups`/`assignResearchHintGroups` actually group within — mix
topics instead of staying topic-blocked. A keyword bridge connecting a
`technology` fact to a `business` or `health` fact, where the two don't
already share a domain or entity tag, is the only kind of result that would
actually demonstrate YAKE is finding something the existing structural
signals can't.

```sh
ORT_LIB_PATH=dist/lib/libonnxruntime.dylib DYLD_LIBRARY_PATH=dist/lib \
  go run ./tools/corpusgen build \
    --out ~/knomit-corpora/broad-100 \
    --size 100 --diversity broad --ontology default
```

## Validating a generated corpus

```sh
go run ./tools/calibrate bridges --db ~/knomit-corpora/narrow-100/core.db --pool both --kind all
```

**Use `--kind all`, not the tool's own default (`both`).** `both` means
domain+entity candidates only — it silently excludes YAKE keyword
extraction entirely, which is the one thing this project is actually trying
to calibrate. `all` = domain + entity + keyword.

A healthy corpus prints a non-degenerate `Coh`/`Sep`/`Q` distribution — not
"no bridge candidates found" (the signature of an un-synced index) and not
every row at identical Q (a symptom of near-identical generated content).
Note that keyword bridges need real scale to appear at all — a 100-fact
corpus produced zero keyword candidates in testing; real keyword bridges
only showed up on a 2,238-fact real corpus.
