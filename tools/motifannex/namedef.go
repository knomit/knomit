package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"

	"knomit/internal/store"
)

// namedef measures the cosine geometry of motif name+definition strings — the
// distribution the `motif_match soft` tier's operating point has to be derived
// from (carried-forward register entry 1), and with it the bridge-tier
// embedding points (entry 2).
//
// FOUR DISTRIBUTIONS, not one, because two choices were open and neither can
// be settled by reasoning:
//
//  1. RENDERING — name alone, or name+definition. No shipped code renders a
//     name+def string today; the tier was deferred before one was needed. MN9's
//     own lesson is that a measured anchor transfers only under the measured
//     string, so the rendering is measured rather than assumed, and whichever
//     ships must be the one the operating point came from.
//
//  2. CENTERING — the shipped EmbedShortStrings does NOT mean-center, and every
//     research measurement behind this campaign DID (task_sweep.py, dryrun.py,
//     replay.py, score3b.py, canonicalize.py all do `V = V - V.mean(0)`),
//     including the sweep that settled MN9's template and the E3 recall
//     figures. That has not mattered so far because the one shipped consumer,
//     the alias pre-block, takes a PERCENTILE of its own distribution each
//     session, and a percentile is invariant to centering. It matters here:
//     the soft tier needs an ABSOLUTE point, and short-string embeddings are
//     anisotropic (kb/gotchas/embeddings/short-strings/1d564e96), so a point
//     measured one way does not transfer to the other.
//
// Read-only, on a lab copy. It embeds locally and writes nothing.
func namedef(ctx context.Context, corpus, scratch string) error {
	svc, ri, branch, closeAll, err := open(ctx, corpus, scratch)
	if err != nil {
		return err
	}
	defer closeAll()

	clusters, err := svc.Motifs().Clusters(ctx, branch)
	if err != nil {
		return fmt.Errorf("clusters: %w", err)
	}

	var names, withDefs []string
	defined := 0
	for _, c := range clusters {
		def, ok, derr := svc.Motifs().Definition(ctx, branch, c.ClusterKey)
		if derr != nil {
			return fmt.Errorf("definition %s: %w", c.ClusterKey, derr)
		}
		names = append(names, c.CanonicalID)
		if ok && def != "" {
			defined++
			withDefs = append(withDefs, c.CanonicalID+": "+def)
		} else {
			// A cluster with no definition still belongs in the population —
			// dropping it would measure the definition pass's coverage instead
			// of the vocabulary's geometry. It contributes its bare name, which
			// is what a real query would have to match against too.
			withDefs = append(withDefs, c.CanonicalID)
		}
	}
	if len(names) < 2 {
		return fmt.Errorf("%s: %d clusters — no pair distribution to measure", corpus, len(names))
	}

	emb := ri.Embedder()
	if emb == nil {
		return fmt.Errorf("no embedder")
	}

	out := map[string]any{
		"corpus":            corpus,
		"branch":            branch,
		"clusters":          len(clusters),
		"with_definition":   defined,
		"rendering_namedef": "<canonical_id>: <definition>  (bare id when undefined)",
		"template_note": "both renderings go through the SHIPPED EmbedShortStrings, i.e. the MN9 " +
			"ShortStringTemplate; nothing here re-implements the rendering",
	}

	for label, texts := range map[string][]string{"name": names, "namedef": withDefs} {
		vecs, eerr := emb.EmbedShortStrings(ctx, texts)
		if eerr != nil {
			return fmt.Errorf("embed %s: %w", label, eerr)
		}
		out[label+"_uncentered"] = cosineLadder(vecs, false)
		out[label+"_centered"] = cosineLadder(vecs, true)
	}

	// THE ONLY GROUND TRUTH THIS CORPUS HAS. A cluster carrying two or more
	// distinct spellings is a pair of strings a human or the alias judge
	// declared to mean the same mechanism — the only labelled POSITIVES
	// available anywhere (the judge's merges are the same handful; after a
	// merge the two ids become one cluster, so the spellings are where the
	// label survives).
	//
	// Reported with its n, which is tiny. It cannot place an operating point
	// and is not offered as one: it is a sanity check on whether the geometry
	// puts known-equivalent strings above the bulk at all.
	pos, perr := knownEquivalentPairs(ctx, svc, branch, emb)
	if perr != nil {
		return perr
	}
	out["known_equivalent_pairs"] = pos

	fmt.Fprintf(os.Stderr, "%s: %d clusters, %d with definitions\n", corpus, len(clusters), defined)
	return emit(out)
}

// cosineLadder returns the percentile ladder of all pairwise cosines, with the
// vectors optionally mean-centered first.
//
// Centering happens BEFORE normalisation, matching what the research scripts
// did (`V = V - V.mean(0)` then row-normalise). Doing it the other way measures
// a third thing that neither production nor the research used.
func cosineLadder(in [][]float32, center bool) map[string]any {
	if len(in) < 2 {
		return map[string]any{"pairs": 0}
	}
	dim := len(in[0])
	v := make([][]float64, len(in))
	for i, row := range in {
		v[i] = make([]float64, dim)
		for j, f := range row {
			v[i][j] = float64(f)
		}
	}
	if center {
		mean := make([]float64, dim)
		for _, row := range v {
			for j, x := range row {
				mean[j] += x
			}
		}
		for j := range mean {
			mean[j] /= float64(len(v))
		}
		for _, row := range v {
			for j := range row {
				row[j] -= mean[j]
			}
		}
	}
	for _, row := range v {
		var n float64
		for _, x := range row {
			n += x * x
		}
		n = math.Sqrt(n)
		if n < 1e-9 {
			continue
		}
		for j := range row {
			row[j] /= n
		}
	}

	var cos []float64
	for i := range v {
		for j := i + 1; j < len(v); j++ {
			var d float64
			for k := range v[i] {
				d += v[i][k] * v[j][k]
			}
			cos = append(cos, d)
		}
	}
	sort.Float64s(cos)

	q := func(p float64) float64 {
		if len(cos) == 0 {
			return 0
		}
		idx := int(p * float64(len(cos)-1))
		return cos[idx]
	}
	var sum float64
	for _, c := range cos {
		sum += c
	}
	return map[string]any{
		"pairs": len(cos),
		"mean":  round4(sum / float64(len(cos))),
		"min":   round4(cos[0]),
		"p50":   round4(q(0.50)),
		"p90":   round4(q(0.90)),
		"p95":   round4(q(0.95)),
		"p99":   round4(q(0.99)),
		"p999":  round4(q(0.999)),
		"max":   round4(cos[len(cos)-1]),
	}
}

func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

// knownEquivalentPairs returns the cosine of every within-cluster spelling pair
// — strings the vocabulary already says mean the same mechanism — under both
// centerings, so they can be read against the bulk ladder above.
func knownEquivalentPairs(ctx context.Context, svc *store.Service, branch string, emb store.BatchEmbedder) ([]map[string]any, error) {
	// AliasTable is spelling -> canonical id, which is exactly the grouping
	// needed: two spellings sharing a canonical id are two strings the
	// vocabulary says name one mechanism.
	table, err := svc.Motifs().AliasTable(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("alias table: %w", err)
	}
	byCluster := map[string][]string{}
	for spelling, canon := range table {
		byCluster[canon] = append(byCluster[canon], spelling)
	}

	// Embed the WHOLE vocabulary once, and take the centering mean over all of
	// it. Centering a pair by its own two-vector mean makes the two vectors
	// exact opposites and every cosine -1 — which is what the first version of
	// this function did, and it produced a confident -1.000 for every positive.
	// The mean has to be the POPULATION's, because that is the anisotropy being
	// removed; a mean over the thing being measured measures nothing.
	all := make([]string, 0, len(table))
	for spelling := range table {
		all = append(all, spelling)
	}
	sort.Strings(all)
	if len(all) < 2 {
		return nil, nil
	}
	vecs, err := emb.EmbedShortStrings(ctx, all)
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, sp := range all {
		idx[sp] = i
	}
	mean := meanOf(vecs)

	canons := make([]string, 0, len(byCluster))
	for c, ms := range byCluster {
		if len(ms) >= 2 {
			canons = append(canons, c)
		}
	}
	sort.Strings(canons)

	var out []map[string]any
	for _, c := range canons {
		ms := byCluster[c]
		sort.Strings(ms)
		for i := range ms {
			for j := i + 1; j < len(ms); j++ {
				a, b := vecs[idx[ms[i]]], vecs[idx[ms[j]]]
				out = append(out, map[string]any{
					"a":          ms[i],
					"b":          ms[j],
					"uncentered": round4(pairCos(a, b, nil)),
					"centered":   round4(pairCos(a, b, mean)),
				})
			}
		}
	}
	return out, nil
}

func meanOf(v [][]float32) []float64 {
	if len(v) == 0 {
		return nil
	}
	m := make([]float64, len(v[0]))
	for _, row := range v {
		for j, x := range row {
			m[j] += float64(x)
		}
	}
	for j := range m {
		m[j] /= float64(len(v))
	}
	return m
}

func pairCos(a, b []float32, mean []float64) float64 {
	x := make([]float64, len(a))
	y := make([]float64, len(b))
	for i := range a {
		x[i], y[i] = float64(a[i]), float64(b[i])
		if mean != nil {
			x[i] -= mean[i]
			y[i] -= mean[i]
		}
	}
	var d, nx, ny float64
	for i := range x {
		d += x[i] * y[i]
		nx += x[i] * x[i]
		ny += y[i] * y[i]
	}
	if nx < 1e-18 || ny < 1e-18 {
		return 0
	}
	return d / (math.Sqrt(nx) * math.Sqrt(ny))
}
