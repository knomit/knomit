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

- **`--content-source synthetic`** (default): the LLM invents plausible
  content. Fast, no network dependency beyond the CLI itself, but the
  information in the corpus isn't real — it's a fictional system the model
  makes up (e.g. `meridian-gateway`, `event-bus`, entirely invented).
- **`--content-source real`**: the LLM must use its `WebSearch` tool and
  ground every fact in genuine search results, citing a real URL. Every
  cited URL gets an actual HTTP check (`verify.go`) before being trusted
  into the corpus — a fact with no verifiable citation is dropped and
  regenerated (corpusgen overprovisions ~30% extra slots to absorb this).
  Confirmed empirically that `claude -p --allowedTools WebSearch` performs
  genuine searches headlessly (verified against a query answerable only by
  a live search, not training-data recall) — but `WebFetch` is **not**
  authorized non-interactively, so the model works from search-result
  snippets only, not full page reads. Real mode is meaningfully slower
  (genuine searches per batch) and, unlike synthetic mode, `--seed` no
  longer makes runs reproducible even structurally-adjacent — which real
  subjects get covered varies with what's actually findable each run.

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
| `--diversity` | `narrow` | diversity profile — only `narrow` is implemented so far |
| `--ontology` | `code` | `default` (general 13-topic) or `code` (source-code) preset |
| `--topic` | _(none)_ | leaf topic to generate into; required for `--diversity narrow` |
| `--content-source` | `synthetic` | `synthetic` (LLM-invented) or `real` (WebSearch-grounded, HTTP-verified) |
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

## Validating a generated corpus

```sh
go run ./tools/calibrate bridges --db ~/knomit-corpora/narrow-100/core.db --pool both --kind both
```

A healthy corpus prints a non-degenerate `Coh`/`Sep`/`Q` distribution — not
"no bridge candidates found" (the signature of an un-synced index) and not
every row at identical Q (a symptom of near-identical generated content).
