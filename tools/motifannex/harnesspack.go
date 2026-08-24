package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"

	"knomit/internal/store"
	"knomit/internal/synthesize"
)

// harnessSeed fixes every draw in this file. Same value E1 used, so a rerun of
// either is comparable with itself.
const harnessSeed = 42

// harnessArmN is the arm size E1 used. The MOTIF arms cannot reach it on real
// corpora and are reported at whatever size they have — that shortfall is the
// measurement, not a defect in the draw (Phase-4 rulings-6).
const harnessArmN = 12

type harnessPair struct {
	ID     string `json:"id"`
	Arm    string `json:"-"`
	Corpus string `json:"-"`
	Token  string `json:"-"`
	APath  string `json:"-"`
	BPath  string `json:"-"`
	ATitle string `json:"a_title"`
	ABody  string `json:"a_body"`
	BTitle string `json:"b_title"`
	BBody  string `json:"b_body"`
}

// harnesspack builds the Phase-4 blind judging pack.
//
// ARMS (Phase-4 rulings-6, option ii):
//
//   - MOTIF-NEAR / MOTIF-FAR — the SERVED sets from the shipped engine on the
//     real corpora. This is the PRIMARY evidence and it is reported at whatever
//     size it has.
//   - TOKEN — the production entity/domain axis on the same corpora, via the
//     shipped BridgeComponentReport. E1's own comparison arm.
//   - RANDOM — seeded uniform pairs. The floor control.
//   - TRAP — pairs that SHARE a canonical motif and ALSO share a subject label.
//     Drawn from real data rather than fabricated: these are what the
//     disjointness gate exists to reject, so a judge calling them MECH is
//     visible and the MECH rates have an upper control (349c0ab6).
//
// Group→pair projection: one pair per served group, chosen by a seeded draw
// over the group's member pairs, so no group can dominate an arm. That matches
// how E1's own TOKEN arm was built.
func harnesspack(ctx context.Context, scratch string, corpora []string) error {
	rng := rand.New(rand.NewSource(harnessSeed))
	var motifNear, motifFar, token, random, trap []harnessPair

	for _, corpus := range corpora {
		svc, ri, branch, closeAll, err := open(ctx, corpus, scratch)
		if err != nil {
			return err
		}
		cfg := synthesize.QualityConfigFromRepo(ri)
		rep, err := synthesize.MotifComponentReport(ctx, svc.Search(), svc.Motifs(),
			svc.Abstraction(), branch, synthesize.EffortHigh,
			ri.ClusterResolution(), ri.ClusterMinCommunitySize(), cfg)
		if err != nil {
			closeAll()
			return fmt.Errorf("%s motif report: %w", corpus, err)
		}

		served := map[string]bool{}
		for _, t := range rep.NearServed {
			served[t] = true
		}
		farServed := map[string]bool{}
		for _, t := range rep.FarServed {
			farServed[t] = true
		}
		for _, c := range rep.Candidates {
			if !served[c.Token] && !farServed[c.Token] {
				continue
			}
			p, ok := projectPair(ctx, svc, branch, corpus, c.Token, c.Members, rng)
			if !ok {
				continue
			}
			if farServed[c.Token] {
				p.Arm = "MOTIF-FAR"
				motifFar = append(motifFar, p)
			} else {
				p.Arm = "MOTIF-NEAR"
				motifNear = append(motifNear, p)
			}
		}

		// TOKEN — the shipped entity/domain report, kept bridges only.
		tk, err := synthesize.BridgeComponentReport(ctx, svc.Search(), branch,
			synthesize.BridgeKindFromString("both"), synthesize.EffortHigh,
			ri.ClusterResolution(), ri.ClusterMinCommunitySize(), cfg)
		if err != nil {
			closeAll()
			return fmt.Errorf("%s token report: %w", corpus, err)
		}
		for _, b := range tk {
			if !b.Kept {
				continue
			}
			if p, ok := projectPair(ctx, svc, branch, corpus, b.Token, b.Members, rng); ok {
				p.Arm = "TOKEN"
				token = append(token, p)
			}
		}

		// RANDOM and TRAP need the seed pool, so read it the way the report does.
		seeds, err := harnessSeeds(ctx, svc, branch)
		if err != nil {
			closeAll()
			return err
		}
		random = append(random, randomPairs(ctx, svc, branch, corpus, seeds, rng, harnessArmN)...)
		trap = append(trap, trapPairs(ctx, svc, branch, corpus, seeds, rng)...)
		closeAll()
	}

	pick := func(in []harnessPair, n int) []harnessPair {
		sort.Slice(in, func(a, b int) bool { return in[a].APath+in[a].BPath < in[b].APath+in[b].BPath })
		if len(in) <= n {
			return in
		}
		rng.Shuffle(len(in), func(a, b int) { in[a], in[b] = in[b], in[a] })
		return in[:n]
	}

	all := append([]harnessPair{}, motifNear...)
	all = append(all, motifFar...)
	all = append(all, pick(token, harnessArmN)...)
	all = append(all, pick(random, harnessArmN)...)
	all = append(all, pick(trap, 6)...)

	// Blind: shuffle, then id. The id carries no arm information.
	rng.Shuffle(len(all), func(a, b int) { all[a], all[b] = all[b], all[a] })
	var pack, key []map[string]any
	for i := range all {
		all[i].ID = fmt.Sprintf("H%03d", i+1)
		pack = append(pack, map[string]any{
			"id": all[i].ID, "a_title": all[i].ATitle, "a_body": all[i].ABody,
			"b_title": all[i].BTitle, "b_body": all[i].BBody,
		})
		key = append(key, map[string]any{
			"id": all[i].ID, "arm": all[i].Arm, "corpus": all[i].Corpus,
			"token": all[i].Token, "a": all[i].APath, "b": all[i].BPath,
		})
	}

	counts := map[string]int{}
	for _, p := range all {
		counts[p.Arm]++
	}
	fmt.Fprintf(os.Stderr, "arms=%v total=%d\n", counts, len(all))
	return emit(map[string]any{
		"seed":        harnessSeed,
		"arm_target":  harnessArmN,
		"arm_counts":  counts,
		"projection":  "one pair per served group, seeded draw over its member pairs (E1's TOKEN-arm shape)",
		"populations": "PRIMARY: real lab corpora only. The fixture supplement is a separate pack and is never pooled with this one.",
		"pack":        pack,
		"key":         key,
	})
}

// projectPair turns a served group into one judged pair.
func projectPair(ctx context.Context, svc *store.Service, branch, corpus, token string,
	members []string, rng *rand.Rand) (harnessPair, bool) {
	if len(members) < 2 {
		return harnessPair{}, false
	}
	sorted := append([]string(nil), members...)
	sort.Strings(sorted)
	i := rng.Intn(len(sorted))
	j := rng.Intn(len(sorted) - 1)
	if j >= i {
		j++
	}
	a, b := sorted[i], sorted[j]
	if b < a {
		a, b = b, a
	}
	at, ab, ok1 := factText(ctx, svc, branch, a)
	bt, bb, ok2 := factText(ctx, svc, branch, b)
	if !ok1 || !ok2 {
		return harnessPair{}, false
	}
	return harnessPair{Corpus: corpus, Token: token, APath: a, BPath: b,
		ATitle: at, ABody: ab, BTitle: bt, BBody: bb}, true
}

func factText(ctx context.Context, svc *store.Service, branch, path string) (title, body string, ok bool) {
	f, err := svc.FactQuery().GetByPath(ctx, branch, path)
	if err != nil || f == nil {
		return "", "", false
	}
	b := strings.TrimSpace(f.Body)
	if len(b) > 400 {
		b = b[:400]
	}
	return f.Title, strings.ReplaceAll(b, "\n", " "), true
}

// harnessSeeds reads the corpus's bridging population — the same projection and
// filter the motif report uses, so RANDOM and TRAP are drawn from the same pool
// the other arms came from rather than from a different corpus view.
func harnessSeeds(ctx context.Context, svc *store.Service, branch string) ([]store.SearchResult, error) {
	return svc.Search().Search(ctx, branch, store.SearchOptions{Limit: 100000})
}

func randomPairs(ctx context.Context, svc *store.Service, branch, corpus string,
	seeds []store.SearchResult, rng *rand.Rand, n int) []harnessPair {
	if len(seeds) < 2 {
		return nil
	}
	var out []harnessPair
	for range n * 3 {
		i, j := rng.Intn(len(seeds)), rng.Intn(len(seeds))
		if i == j {
			continue
		}
		p, ok := projectPair(ctx, svc, branch, corpus, "",
			[]string{seeds[i].Path, seeds[j].Path}, rng)
		if !ok {
			continue
		}
		p.Arm = "RANDOM"
		out = append(out, p)
		if len(out) >= n {
			break
		}
	}
	return out
}

// trapPairs are the ceiling control: two facts that share a canonical motif AND
// share a subject label, i.e. exactly what subject-disjointness rejects.
//
// Drawn from real data rather than fabricated. A judge who calls these
// MECH is calling "same subject, same mechanism" a bridge, and the MECH rate
// on the real arms cannot be read without knowing that.
func trapPairs(ctx context.Context, svc *store.Service, branch, corpus string,
	seeds []store.SearchResult, rng *rand.Rand) []harnessPair {
	byMotif := map[string][]store.SearchResult{}
	for _, s := range seeds {
		for _, m := range s.Motifs {
			byMotif[m] = append(byMotif[m], s)
		}
	}
	var out []harnessPair
	keys := make([]string, 0, len(byMotif))
	for k := range byMotif {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fs := byMotif[k]
		for i := range fs {
			for j := i + 1; j < len(fs); j++ {
				if !sharesSubjectLabel(fs[i], fs[j]) {
					continue
				}
				p, ok := projectPair(ctx, svc, branch, corpus, k,
					[]string{fs[i].Path, fs[j].Path}, rng)
				if !ok {
					continue
				}
				p.Arm = "TRAP"
				out = append(out, p)
			}
		}
	}
	return out
}

func sharesSubjectLabel(a, b store.SearchResult) bool {
	set := map[string]struct{}{}
	for _, e := range append(append([]string{}, a.Entities...), a.Domain...) {
		set[strings.ToLower(e)] = struct{}{}
	}
	for _, e := range append(append([]string{}, b.Entities...), b.Domain...) {
		if _, ok := set[strings.ToLower(e)]; ok {
			return true
		}
	}
	return false
}
