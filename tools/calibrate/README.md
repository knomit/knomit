# calibrate

A tool for re-deriving knomit's **model-dependent cosine thresholds** when the
default embedding model changes.

## Why this exists

knomit uses six absolute cosine-similarity cutoffs across retrieval and dedup:

| threshold | role | lives on |
|---|---|---|
| `Dedup` | merge a fact into a near-duplicate (learn + synthesis prune) | `retrieval.Thresholds` |
| `ReflectNovelty` | reject a proposed methodology as too similar to an existing one | `retrieval.Thresholds` |
| `SimilarTo` | draw a `SIMILAR_TO` graph edge (on top of the top-K cap) | `retrieval.Thresholds` |
| `SearchFloor` | default recall floor for vector search | `retrieval.Thresholds` |
| `RerankHigh` / `RerankLow` | bucket a caller's `MinSimilarity` to size candidate over-fetch | `retrieval.Thresholds` |

Each value is an **absolute point on a specific model's cosine distribution**.
Different embedding models produce different distributions, so a threshold tuned
for one model is silently mis-tuned for another. EmbeddingGemma, for example,
runs markedly *cooler* than nomic (distinct same-category pairs average ~0.48 vs
~0.75) — so nomic's `0.92` dedup cutoff would almost never fire under gemma,
quietly letting duplicates accumulate.

These values are carried per-model on `embeddings.Model.Thresholds`. This tool
derives the right set for a new model.

## What it measures

For each model, it embeds the whole corpus and builds four cosine
distributions:

- **docDocSame** — distinct facts in the same category (governs `Dedup`, `ReflectNovelty`)
- **docDocAll** — all document pairs (governs `SimilarTo`)
- **queryDoc** — every `EmbedQuery(title)` against every document (governs `SearchFloor`, rerank tiers)
- **nearDup** — each fact vs. a synthetic near-duplicate of itself (the true-merge signal, used to validate the dedup gate)

It then ports each baseline threshold by **preserving its percentile**: for a
baseline value `T` on distribution `D`, find `p = CDF_baseline(T)`, then the new
model's value is the `p`-quantile of *its* `D`. This keeps each gate's
selectivity stable across the model swap without needing labeled relevance
judgments.

It reads facts directly from one or more knomit index DBs, opened **read-only and
immutable**, so it never mutates or locks a live corpus. Models are downloaded
into the cache dir on first use.

## Usage

From the repo root (the env vars point the in-process ONNX runtime at the
vendored native libs — run `make setup` first if `dist/lib` is empty):

```sh
ORT_LIB_PATH=dist/lib/libonnxruntime.dylib DYLD_LIBRARY_PATH=dist/lib \
  go run ./tools/calibrate \
    -cache ~/.knomit/models \
    ~/.knomit/repos/trunk.db ~/.knomit/repos/knomit-kb.db
```

On Linux use `libonnxruntime.so` and `LD_LIBRARY_PATH` instead.

### Flags

| flag | default | meaning |
|---|---|---|
| `-cache` | _(required)_ | model cache dir; models are downloaded here if missing |
| `-models` | `nomic-v1.5,embeddinggemma` | comma-separated model ids to measure |
| `-baseline` | `nomic-v1.5` | model whose thresholds are ported **from** |

Positional args are one or more knomit `.db` paths. Pass several to calibrate
across a combined corpus.

## Reading the output

```
=== distribution-preserving port: nomic-v1.5 -> embeddinggemma ===
threshold              nomic-v1.5     pctile embeddinggemma
dedup (learn+synth)       0.920      99.6%        0.818
...
dedup safety gap (embeddinggemma): distinct same-cat p99=0.766  true near-dup p05=0.959
```

- **pctile** is where the baseline value sits on the baseline's distribution.
- The last column is the ported value at that same percentile on the new model.
- The **dedup safety gap** sanity-checks the data-loss-critical cutoff: the dedup
  value must sit *above* `distinct same-cat p99` (don't merge distinct facts) and
  *below* `true near-dup p05` (still catch real duplicates).

## Applying the results

Copy the ported values into the model's entry in
[`internal/embeddings/model.go`](../../internal/embeddings/model.go) as its
`Thresholds`. A pure port of `SearchFloor` can come out near zero (or negative);
clamp it to a small positive floor (e.g. `0.05`) so it still drops anti-correlated
noise. Round to two decimals.

The EmbeddingGemma values currently shipped were produced this way:

| threshold | nomic | embeddinggemma |
|---|---|---|
| Dedup | 0.92 | 0.82 |
| ReflectNovelty | 0.85 | 0.69 |
| SimilarTo | 0.60 | 0.18 |
| SearchFloor | 0.40 | 0.05 _(port ~0, clamped)_ |
| RerankHigh | 0.70 | 0.43 |
| RerankLow | 0.50 | 0.10 |
