# calibrate

A dev-only tool with two subcommands:

- **`calibrate embeddings`** re-derives knomit's **model-dependent cosine
  thresholds** when the embedding model (or its rendering template) changes.
- **`calibrate bridges`** prints the bridge-quality scorer's component report and
  suggested floors, for tuning the `discovery.quality` knobs.

Run `go run ./tools/calibrate <subcommand> --help` for the authoritative flag list.

Flags are parsed by cobra/pflag, so long flags take **two dashes** (`--cache`).
A single-dash `-cache` is read as a bundle of shorthand flags starting with `-c`
and fails with `unknown shorthand flag: 'c' in -cache`.

## `calibrate embeddings`

### Why this exists

knomit uses six absolute cosine-similarity cutoffs across retrieval and dedup.
All six are fields of `params.Thresholds`, and each model's set is an entry in
the per-model map in
[`internal/embeddings/params/thresholds.go`](../../internal/embeddings/params/thresholds.go):

| threshold | role |
|---|---|
| `Dedup` | merge a fact into a near-duplicate (learn + synthesis prune) |
| `ReflectNovelty` | reject a proposed methodology as too similar to an existing one |
| `SimilarTo` | draw a `SIMILAR_TO` graph edge (on top of the top-K cap) |
| `SearchFloor` | default recall floor for vector search |
| `RerankHigh` / `RerankLow` | bucket a caller's `MinSimilarity` to size candidate over-fetch |

Each value is an **absolute point on a specific model's cosine distribution**.
Different embedding models produce different distributions, so a threshold tuned
for one model is silently mis-tuned for another. EmbeddingGemma, for example,
runs markedly *cooler* than nomic (distinct same-category pairs average ~0.48 vs
~0.75). Under gemma, nomic's `0.92` dedup cutoff would almost never fire, so
duplicates would quietly accumulate.

`embeddings.Model.Thresholds` is filled from that map when each model
descriptor is initialized. The map is where the numbers live. This tool derives
the right set for a new model.

### What it measures

For each model, it embeds the whole corpus and builds four cosine
distributions:

- **docDocSame**: distinct facts in the same category (governs `Dedup`, `ReflectNovelty`)
- **docDocAll**: all document pairs (governs `SimilarTo`)
- **queryDoc**: every `EmbedQuery(title)` against every document (governs `SearchFloor`, rerank tiers)
- **nearDup**: each fact vs. a synthetic near-duplicate of itself (the true-merge signal, used to validate the dedup gate)

It then ports each baseline threshold by **preserving its percentile**: for a
baseline value `T` on distribution `D`, find `p = CDF_baseline(T)`, then the new
model's value is the `p`-quantile of *its* `D`. This keeps each gate's
selectivity stable across the model swap without needing labeled relevance
judgments. The baseline values are read from the baseline model's descriptor,
not re-listed in the tool.

**Only thresholds ported on the same distribution may bound one interval.**
`Dedup` and `ReflectNovelty` both come from `docDocSame`, so
`(ReflectNovelty, Dedup)` is a coherent band. `SimilarTo` comes from
`docDocAll`, so `[SimilarTo, Dedup)` describes no region of either population
(see knomit fact `decisions/mcp/learn/same-subject-band-endpoints`). This does
not make `SimilarTo` wrong. It is calibrated for exactly one job: drawing
SIMILAR_TO edges under the top-K cap.

It reads facts directly from one or more knomit index DBs, opened **read-only and
immutable**, so it never mutates or locks a live corpus. Models are downloaded
into the cache dir on first use.

### Usage

From the repo root. The env vars point the in-process ONNX runtime at the
vendored native libs under `dist/<os>-<arch>/lib`; run `make setup` first if
that directory is empty.

```sh
ORT_LIB_PATH=dist/darwin-arm64/lib/libonnxruntime.dylib \
DYLD_LIBRARY_PATH=dist/darwin-arm64/lib \
  go run ./tools/calibrate embeddings \
    --cache ~/.knomit/models \
    ~/.knomit/repos/core.db ~/.knomit/repos/knomit-kb.db
```

On Linux use `dist/linux-<arch>/lib/libonnxruntime.so` and `LD_LIBRARY_PATH`
instead.

The `~/.knomit` above is the data root on macOS and Linux. On Windows it is
`%LOCALAPPDATA%\knomit\home` — see
[tools/desktop/README.md](../desktop/README.md). `KNOMIT_HOME` overrides it
everywhere, so `$KNOMIT_HOME/models` and `$KNOMIT_HOME/repos` are the portable
spellings when you have it set.

| flag | default | meaning |
|---|---|---|
| `--cache` | _(required)_ | model cache dir; models are downloaded here if missing |
| `--models` | `nomic-v1.5,embeddinggemma` | comma-separated model ids to measure |
| `--baseline` | `nomic-v1.5` | model whose thresholds are ported **from**; must be one of `--models` |

Positional args are one or more knomit `.db` paths. Pass several to calibrate
across a combined corpus.

### Reading the output

```
=== distribution-preserving port: nomic-v1.5 -> embeddinggemma ===
threshold              nomic-v1.5     pctile embeddinggemma
dedup (learn+synth)       0.920      99.6%        0.818
...
dedup safety gap (embeddinggemma): distinct same-cat p99=0.766  true near-dup p05=0.959
```

- **pctile** is where the baseline value sits on the baseline's distribution.
- The last column is the ported value at that same percentile on the new model.
- The **dedup safety gap** sanity-checks the cutoff whose error loses data. The
  dedup value must sit *above* `distinct same-cat p99` (don't merge distinct
  facts) and *below* `true near-dup p05` (still catch real duplicates).

### Applying the results

Copy the ported values into the model's entry in the `modelThresholds` map in
[`internal/embeddings/params/thresholds.go`](../../internal/embeddings/params/thresholds.go).
A model with no entry there fails at init, so a new model needs its entry added
before its descriptor can load. A pure port of `SearchFloor` can come out near
zero (or negative). Clamp it to a small positive floor (e.g. `0.05`) so it still
drops anti-correlated noise. Round to two decimals.

The EmbeddingGemma values currently shipped were produced this way:

| threshold | nomic | embeddinggemma |
|---|---|---|
| Dedup | 0.92 | 0.82 |
| ReflectNovelty | 0.85 | 0.69 |
| SimilarTo | 0.60 | 0.18 |
| SearchFloor | 0.40 | 0.05 _(port ~0, clamped)_ |
| RerankHigh | 0.70 | 0.43 |
| RerankLow | 0.50 | 0.10 |

## `calibrate bridges`

Opens a knomit index DB and runs the bridge-quality scorer against it. No
embedding model is loaded, so no ONNX env vars are needed. The scorer reads the
precomputed SIMILAR_TO edges, Louvain clusters, derivation paths and token
frequencies already in the index.

```sh
go run ./tools/calibrate bridges --db ~/.knomit/repos/core.db --effort high
```

For the entity/domain kinds it prints a population line, a per-bridge table
(token, kind, members, coh, sep, gap, spec, Q, kept), aggregates (candidates,
kept, mean/median Q) and `SUGGESTED-FLOORS` for `coh-floor` and `quality-floor`.

**The suggested floors are a lower bound, not the production floor.** The report
scores candidates *unreshaped*. Production applies `cohFloor` after
`reshapeCohesiveSubset`, which raises cohesion, so it effectively sees a higher
floor (knomit gotcha `synthesize/calibrate-bridges-floor-heuristic`). The
output's `POPULATION:` line says this before any number is printed.

`--kind motif` runs a **separate** report, `synthesize.MotifComponentReport`. Its
population, pool and engine all differ from the entity/domain report, so it has
its own output and is not a variant of the table above.

| flag | default | meaning |
|---|---|---|
| `--db` | _(required)_ | path to the knomit index DB |
| `--branch` | `main` | branch to query |
| `--effort` | `medium` | discovery effort (`normal` / `medium` / `high`) |
| `--kind` | `both` | `domain`, `entity`, `both`, or `motif` |
| `--resolution` | `2.0` | Louvain resolution for clustering |
| `--min-community` | `2` | minimum community size for clustering |
| `--coh-floor`, `--quality-floor`, `--w-coh`, `--w-gap`, `--w-spec`, `--max-members` | `config.Defaults().Discovery` | Q-knob overrides; unset flags use the shipped config defaults |
