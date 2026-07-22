package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"github.com/spf13/cobra"
	"knomit/internal/embeddings"
	"knomit/internal/fact"
)

func newEmbeddingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "embeddings [flags] <db> [<db>...]",
		Short: "Measure cosine-similarity geometry and port model-dependent thresholds",
		Long: `embeddings reads facts from one or more knomit index DBs (opened read-only and
immutable), computes cosine-similarity distributions for each embedding model,
and prints distribution statistics and percentile-preserving threshold ports.

The thresholds are absolute points on a cosine distribution and that distribution
is model-specific. When the default model changes, each threshold is ported by
PRESERVING THE PERCENTILE it occupied on the baseline model's distribution.`,
		Example: `  ORT_LIB_PATH=dist/darwin-arm64/lib/libonnxruntime.dylib \
  DYLD_LIBRARY_PATH=dist/darwin-arm64/lib \
    go run ./tools/calibrate embeddings --cache ~/.knomit/models \
      ~/.knomit/repos/core.db ~/.knomit/repos/knomit-kb.db`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cache, _ := cmd.Flags().GetString("cache")
			modelsCSV, _ := cmd.Flags().GetString("models")
			baseline, _ := cmd.Flags().GetString("baseline")
			return runEmbeddings(cache, modelsCSV, baseline, args)
		},
	}

	f := cmd.Flags()
	f.String("cache", "", "model cache dir (required)")
	f.String("models", "nomic-v1.5,embeddinggemma", "comma-separated model ids to measure")
	f.String("baseline", "nomic-v1.5", "model id to port thresholds FROM")
	_ = cmd.MarkFlagRequired("cache")

	return cmd
}

func runEmbeddings(cache, modelsCSV, baseline string, dbPaths []string) error {
	models := strings.Split(modelsCSV, ",")

	docs, err := loadDocs(dbPaths)
	if err != nil {
		return fmt.Errorf("load corpus: %w", err)
	}
	fmt.Printf("corpus: %d facts from %d db(s)\n\n", len(docs), len(dbPaths))

	measured := map[string]dists{}
	for _, id := range models {
		d, err := measure(strings.TrimSpace(id), cache, docs)
		if err != nil {
			return fmt.Errorf("measure %s: %w", id, err)
		}
		measured[strings.TrimSpace(id)] = d
	}

	for _, id := range models {
		id = strings.TrimSpace(id)
		d := measured[id]
		fmt.Printf("=== %s distributions ===\n", id)
		fmt.Println(" ", summary("docDocSame", d.docDocSame))
		fmt.Println(" ", summary("docDocAll", d.docDocAll))
		fmt.Println(" ", summary("queryDoc", d.queryDoc))
		fmt.Println(" ", summary("nearDup", d.nearDup))
		fmt.Println()
	}

	base, ok := measured[baseline]
	if !ok {
		return fmt.Errorf("baseline %q not among --models", baseline)
	}
	// The baseline values are the thresholds ALREADY configured for the baseline
	// model — read straight from its descriptor (single source of truth) rather
	// than re-listing literals here, so re-tuning a model never leaves calibrate
	// porting from stale numbers. nomic's descriptor == retrieval.Defaults().
	baseModel, err := embeddings.Lookup(baseline)
	if err != nil {
		return fmt.Errorf("baseline descriptor %q: %w", baseline, err)
	}
	bt := baseModel.Thresholds
	// The six model-dependent thresholds and the distribution each governs.
	thresholds := []baselineThreshold{
		{"dedup (learn+synth)", bt.Dedup, func(d dists) []float64 { return d.docDocSame }},
		{"reflect novelty", bt.ReflectNovelty, func(d dists) []float64 { return d.docDocSame }},
		{"SIMILAR_TO (knn)", bt.SimilarTo, func(d dists) []float64 { return d.docDocAll }},
		{"search recall floor", bt.SearchFloor, func(d dists) []float64 { return d.queryDoc }},
		{"rerank high tier", bt.RerankHigh, func(d dists) []float64 { return d.queryDoc }},
		{"rerank low tier", bt.RerankLow, func(d dists) []float64 { return d.queryDoc }},
	}
	for _, id := range models {
		id = strings.TrimSpace(id)
		if id == baseline {
			continue
		}
		target := measured[id]
		fmt.Printf("=== distribution-preserving port: %s -> %s ===\n", baseline, id)
		fmt.Printf("%-22s %8s %10s %12s\n", "threshold", baseline, "pctile", id)
		for _, t := range thresholds {
			pct := cdf(t.distOf(base), t.value)
			fmt.Printf("%-22s %8.3f %9.1f%% %12.3f\n", t.name, t.value, pct*100, quantile(t.distOf(target), pct))
		}
		fmt.Printf("\ndedup safety gap (%s): distinct same-cat p99=%.3f  true near-dup p05=%.3f\n\n",
			id, quantile(target.docDocSame, .99), quantile(target.nearDup, .05))
	}
	return nil
}

type doc struct {
	path, title, body, category, ftype string
}

// baselineThreshold names a knomit threshold, its current (baseline-model)
// value, and which measured distribution governs it.
type baselineThreshold struct {
	name   string
	value  float64
	distOf func(dists) []float64
}

// loadDocs reads facts (latest version per path) from knomit index DBs,
// opened read-only and immutable.
func loadDocs(dbPaths []string) ([]doc, error) {
	seen := map[string]bool{}
	var docs []doc
	for _, p := range dbPaths {
		db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro&immutable=1", p))
		if err != nil {
			return nil, err
		}
		rows, err := db.Query(`
			SELECT f.path, f.title, CAST(o.data AS TEXT)
			FROM branch_facts bf
			JOIN facts f ON f.id = bf.fact_id
			JOIN objects o ON o.hash = f.blob_hash`)
		if err != nil {
			db.Close()
			return nil, err
		}
		for rows.Next() {
			var fp, title, data string
			if err := rows.Scan(&fp, &title, &data); err != nil {
				rows.Close()
				db.Close()
				return nil, err
			}
			if seen[fp] {
				continue
			}
			body := fact.ExtractBody([]byte(data))
			if strings.TrimSpace(body) == "" {
				continue
			}
			seen[fp] = true
			docs = append(docs, doc{fp, title, body, path.Dir(fp), ""})
		}
		rows.Close()
		db.Close()
	}
	return docs, nil
}

// nearDup builds a realistic near-duplicate: same title, body restated shorter
// by dropping every 3rd sentence (a fact that says ~the same thing with fewer
// details — the canonical dedup-merge case).
func nearDup(body string) string {
	sents := strings.Split(body, ". ")
	if len(sents) < 3 {
		if len(body) > 40 {
			return body[:len(body)*3/4]
		}
		return body
	}
	kept := make([]string, 0, len(sents))
	for i, s := range sents {
		if i%3 == 2 {
			continue
		}
		kept = append(kept, s)
	}
	return strings.Join(kept, ". ")
}

// dists holds the cosine distributions measured for one model (all sorted asc).
type dists struct {
	docDocSame []float64 // distinct facts, same category (dedup / novelty background)
	docDocAll  []float64 // all doc pairs (SIMILAR_TO / knn)
	queryDoc   []float64 // EmbedQuery(title_i) vs doc_j, all pairs (recall / rerank)
	nearDup    []float64 // doc_i vs near-duplicate(doc_i) (true-merge signal)
}

func measure(id, cacheDir string, docs []doc) (dists, error) {
	m, err := embeddings.Lookup(id)
	if err != nil {
		return dists{}, err
	}
	fmt.Fprintf(os.Stderr, "[%s] loading (dim=%d)...\n", id, m.Dim)
	// A calibration run is a foreground CLI sweep with no caller to cancel it,
	// so Background is the honest ctx for both the model fetch and the embeds.
	ctx := context.Background()
	e, err := embeddings.NewEmbedder(ctx, m, cacheDir)
	if err != nil {
		return dists{}, err
	}
	defer e.Close()

	n := len(docs)
	docVecs := make([][]float32, n)
	qVecs := make([][]float32, n)
	for i, d := range docs {
		if docVecs[i], err = e.EmbedDocument(ctx, d.title, d.body); err != nil {
			return dists{}, err
		}
		if qVecs[i], err = e.EmbedQuery(ctx, d.title); err != nil {
			return dists{}, err
		}
		if i%100 == 0 {
			fmt.Fprintf(os.Stderr, "[%s] embedded %d/%d\n", id, i, n)
		}
	}

	var out dists
	for i := range n {
		for j := i + 1; j < n; j++ {
			c := cosine(docVecs[i], docVecs[j])
			out.docDocAll = append(out.docDocAll, c)
			if docs[i].category == docs[j].category {
				out.docDocSame = append(out.docDocSame, c)
			}
		}
	}
	for i := range n {
		for j := range n {
			out.queryDoc = append(out.queryDoc, cosine(qVecs[i], docVecs[j]))
		}
	}
	for i := 0; i < n; i += 7 { // every 7th fact keeps inference count modest
		nd, err := e.EmbedDocument(ctx, docs[i].title, nearDup(docs[i].body))
		if err != nil {
			return dists{}, err
		}
		out.nearDup = append(out.nearDup, cosine(docVecs[i], nd))
	}

	sort.Float64s(out.docDocSame)
	sort.Float64s(out.docDocAll)
	sort.Float64s(out.queryDoc)
	sort.Float64s(out.nearDup)
	return out, nil
}

func cosine(a, b []float32) float64 {
	var d float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
	}
	return d // embedder L2-normalizes, so dot == cosine
}

func quantile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := p * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// cdf returns the fraction of sorted samples <= v.
func cdf(sorted []float64, v float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	lo, hi := 0, len(sorted)
	for lo < hi {
		mid := (lo + hi) / 2
		if sorted[mid] <= v {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return float64(lo) / float64(len(sorted))
}

func summary(name string, s []float64) string {
	if len(s) == 0 {
		return name + ": (empty)"
	}
	var mean float64
	for _, v := range s {
		mean += v
	}
	mean /= float64(len(s))
	return fmt.Sprintf("%-12s n=%-8d min=%.3f p50=%.3f p90=%.3f p95=%.3f p99=%.3f max=%.3f mean=%.3f",
		name, len(s), s[0], quantile(s, .5), quantile(s, .9), quantile(s, .95), quantile(s, .99), s[len(s)-1], mean)
}
