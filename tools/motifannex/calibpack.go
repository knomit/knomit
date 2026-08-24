package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
)

// calibpack builds the T8 labelling pack: a stratified sample of motif pairs,
// blinded, for a judge to label same-mechanism / different-mechanism.
//
// It emits TWO files' worth of data on stdout — the pack (what the judge sees)
// and the key (what it means) — so the judge can be handed the pack alone.
//
// THE TRANSFORM AND RENDERING ARE FIXED HERE, and that is deliberate: labels
// attach to strings, so labelling a string the shipped tier will not use wastes
// the pass. Per phase4-rulings-4:
//
//   - rendering: `<canonical_id>: <definition>` (name+def), the calibrated
//     tier's string. The alias pre-block keeps name-only and its percentile,
//     untouched.
//   - transform: mean-CENTERED, by the corpus's own vocabulary mean — the same
//     per-corpus mean production will store as derived state, so a cosine here
//     is the number production would compute for that pair in that corpus.
//
// Centering per corpus rather than over a pooled population is the faithful
// choice even though the point being derived is per-MODEL: each pair is scored
// the way the corpus carrying it would score it, and the pooled label set then
// says whether one threshold serves them all. Pooling first would measure a
// population no deployment ever sees.
func calibpack(ctx context.Context, scratch string, corpora []string) error {
	type scored struct {
		A, B   string // the rendered name+def strings
		AID    string
		BID    string
		Corpus string
		Cos    float64
	}
	var all []scored
	seen := map[string]bool{}

	for _, corpus := range corpora {
		svc, ri, branch, closeAll, err := open(ctx, corpus, scratch)
		if err != nil {
			return err
		}
		clusters, err := svc.Motifs().Clusters(ctx, branch)
		if err != nil {
			closeAll()
			return fmt.Errorf("%s clusters: %w", corpus, err)
		}
		var ids, texts []string
		for _, c := range clusters {
			def, ok, derr := svc.Motifs().Definition(ctx, branch, c.ClusterKey)
			if derr != nil {
				closeAll()
				return derr
			}
			t := c.CanonicalID
			if ok && def != "" {
				t = c.CanonicalID + ": " + def
			}
			ids = append(ids, c.CanonicalID)
			texts = append(texts, t)
		}
		if len(texts) < 2 {
			closeAll()
			continue
		}
		emb := ri.Embedder()
		vecs, err := emb.EmbedShortStrings(ctx, texts)
		if err != nil {
			closeAll()
			return err
		}
		mean := meanOf(vecs)
		for i := range ids {
			for j := i + 1; j < len(ids); j++ {
				// Dedupe across corpora on the id pair: merged was seeded from
				// agentic-engineering, so most of ag's vocabulary appears twice
				// and a pooled sample would double-weight it.
				k := ids[i] + "\x00" + ids[j]
				if ids[j] < ids[i] {
					k = ids[j] + "\x00" + ids[i]
				}
				if seen[k] {
					continue
				}
				seen[k] = true
				all = append(all, scored{
					A: texts[i], B: texts[j], AID: ids[i], BID: ids[j],
					Corpus: corpus, Cos: round4(pairCos(vecs[i], vecs[j], mean)),
				})
			}
		}
		closeAll()
	}
	if len(all) == 0 {
		return fmt.Errorf("no pairs")
	}

	// STRATA. The decision region gets the sample; the extremes get controls.
	// Without the controls a pass cannot tell "the threshold is here" from
	// "the judge says yes to everything" — 349c0ab6's floor-and-ceiling rule.
	type stratum struct {
		name     string
		lo, hi   float64
		want     int
		isBand   bool
		selected []scored
	}
	strata := []*stratum{
		{name: "control-low", lo: -1, hi: 0.05, want: 30},
		{name: "band-lower", lo: 0.05, hi: 0.15, want: 60, isBand: true},
		{name: "band-core", lo: 0.15, hi: 0.25, want: 80, isBand: true},
		{name: "band-upper", lo: 0.25, hi: 0.40, want: 60, isBand: true},
		{name: "control-high", lo: 0.40, hi: 2, want: 30},
	}

	// Deterministic selection: sort each stratum and take an even spread across
	// it, so the sample covers the stratum rather than clustering at one end,
	// and a rerun draws the same pack.
	byStratum := map[string][]scored{}
	for _, s := range all {
		for _, st := range strata {
			if s.Cos >= st.lo && s.Cos < st.hi {
				byStratum[st.name] = append(byStratum[st.name], s)
				break
			}
		}
	}
	for _, st := range strata {
		pool := byStratum[st.name]
		sort.Slice(pool, func(a, b int) bool {
			if pool[a].Cos != pool[b].Cos {
				return pool[a].Cos < pool[b].Cos
			}
			return pool[a].AID+pool[a].BID < pool[b].AID+pool[b].BID
		})
		if len(pool) <= st.want {
			st.selected = pool
			continue
		}
		step := float64(len(pool)) / float64(st.want)
		for i := 0; i < st.want; i++ {
			st.selected = append(st.selected, pool[int(math.Floor(float64(i)*step))])
		}
	}

	// Blind ids, and a stable shuffle by hash of the id pair so the pack's
	// order carries no information about stratum or cosine.
	type packItem struct {
		ID string `json:"id"`
		A  string `json:"a"`
		B  string `json:"b"`
	}
	type keyItem struct {
		ID      string  `json:"id"`
		Stratum string  `json:"stratum"`
		Cos     float64 `json:"centered_cos"`
		AID     string  `json:"a_id"`
		BID     string  `json:"b_id"`
		Corpus  string  `json:"corpus"`
	}
	var flat []scored
	counts := map[string]int{}
	for _, st := range strata {
		flat = append(flat, st.selected...)
		counts[st.name] = len(st.selected)
		for range st.selected {
			_ = st
		}
	}
	stratumOf := map[string]string{}
	for _, st := range strata {
		for _, s := range st.selected {
			stratumOf[s.AID+"\x00"+s.BID] = st.name
		}
	}
	sort.Slice(flat, func(a, b int) bool {
		return fnv(flat[a].AID+flat[a].BID) < fnv(flat[b].AID+flat[b].BID)
	})

	var pack []packItem
	var key []keyItem
	for i, s := range flat {
		id := fmt.Sprintf("P%03d", i+1)
		pack = append(pack, packItem{ID: id, A: s.A, B: s.B})
		key = append(key, keyItem{
			ID: id, Stratum: stratumOf[s.AID+"\x00"+s.BID], Cos: s.Cos,
			AID: s.AID, BID: s.BID, Corpus: s.Corpus,
		})
	}

	fmt.Fprintf(os.Stderr, "pairs=%d selected=%d strata=%v\n", len(all), len(flat), counts)
	return emit(map[string]any{
		"total_pairs":     len(all),
		"selected":        len(flat),
		"stratum_counts":  counts,
		"rendering":       "<canonical_id>: <definition>",
		"transform":       "mean-centered by the corpus's own vocabulary mean",
		"pack":            pack,
		"key":             key,
		"preregistration": "phase4-rulings-4 / T8 preamble: fewer than 20 positives inside the band strata means the soft tier is measured and explicitly re-deferred, never given a number",
	})
}

// fnv is a small stable hash, used only to shuffle the pack deterministically.
func fnv(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
