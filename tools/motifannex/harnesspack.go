package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"

	"knomit/internal/fact"
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
//
//   - TOKEN — the production entity/domain axis on the same corpora, via the
//     shipped BridgeComponentReport. E1's own comparison arm.
//
//   - RANDOM — seeded uniform pairs. The floor control.
//
//   - SAME-SUBJECT — pairs that share a canonical motif AND at least one raw
//     subject label. Drawn from real data rather than fabricated.
//
//     NAMED FOR WHAT IT IS, not for what it was first assumed to be. It was
//     called TRAP, on the belief that these are pairs the disjointness gate
//     rejects. They are not: the gate is df-GRADED (rider 1), so a shared label
//     blocks only when it is SPECIFIC — below a per-corpus percentile, not
//     umbrella, not universal. Measured on the served near pair, the four
//     shared labels were df 284 (universal, = LiveFacts), 66 (umbrella, > 56),
//     26 and 28 (both > the cut of 5). Every one deliberately permitted.
//
//     So a MECH verdict in this arm is NOT evidence of a gate failure, and
//     reporting it as one would indict the rider for working. What the arm
//     still does — its actual job — is bound the MECH rate from above: a judge
//     that calls same-subject pairs MECH is visible, and without that the other
//     arms' rates cannot be read (349c0ab6).
//
// Group→pair projection: one pair per served group, chosen by a seeded draw
// over the group's member pairs, so no group can dominate an arm. That matches
// how E1's own TOKEN arm was built.
func harnesspack(ctx context.Context, scratch string, corpora []string) error {
	rng := rand.New(rand.NewSource(harnessSeed))
	var motifNear, motifFar, token, random, trap []harnessPair

	// DEDUPE ACROSS CORPORA. merged was seeded from agentic-engineering, so the
	// same facts and the same bridges exist in both and the naive draw emits
	// each twice — three pairs of the first pack were exact repeats, which the
	// blind judge noticed before I did. A repeated pair is not extra evidence;
	// it is one pair counted twice, and in an arm of three that is most of the
	// arm.
	// Deduping happens ONCE, at the end, in ARM PRIORITY order — not inline as
	// each corpus is read.
	//
	// Inline deduping silently cost the primary evidence: merged is read before
	// agentic-engineering, so a TOKEN pair from merged claimed a pair that was
	// also ag's only MOTIF-NEAR bridge, and the scarce arm lost its single
	// member to a control. Priority order means the arm that can least afford
	// to lose a pair keeps it.
	//
	// A pair claimed by two arms is also worth KNOWING about rather than
	// quietly resolving — the motif and entity/domain axes agreeing on the same
	// two facts is a finding — so collisions are reported.

	for _, corpus := range corpora {
		svc, ri, branch, closeAll, err := open(ctx, corpus, scratch)
		if err != nil {
			return err
		}
		cfg := synthesize.QualityConfigFromRepo(ri)
		rep, err := synthesize.MotifComponentReport(ctx, svc.Search(), svc.Motifs(),
			svc.Abstraction(), branch, fact.ID12(ri.ID()), synthesize.EffortHigh,
			ri.ClusterResolution(), ri.ClusterMinCommunitySize(), cfg)
		if err != nil {
			closeAll()
			return fmt.Errorf("%s motif report: %w", corpus, err)
		}

		// CROSS-CHECK: the motif arms must total exactly what the report says
		// was served. Not circular — the arms are built from per-row Served
		// while this compares against NearServed/FarServed, which the builder
		// produced from the ranked-and-capped output. When Served was
		// unbound, hardcoding it true admitted the cross-tier-suppressed
		// family as a third MOTIF-FAR pair and nothing noticed (M-8).
		wantMotif := len(rep.NearServed) + len(rep.FarServed)
		gotMotif := 0

		// Arm membership comes from the ROW, never from a token lookup
		// (review finding L-4). A token-2 family is keyed by one of the
		// canonical ids it folded, so two candidates can share a Token; if the
		// disjointness trim drops one of an edged pair from the family, the
		// two land in DIFFERENT lanes and a token lookup labels both by
		// whichever map it checked first — putting a near-lane group under the
		// far arm's verdict. Served and Lane are per-candidate facts and cannot
		// be ambiguous.
		for _, c := range rep.Candidates {
			if !c.Served {
				continue
			}
			p, ok := projectPair(ctx, svc, branch, corpus, c.Token, c.Members, rng)
			if !ok {
				continue
			}
			p.Arm = motifArmOf(c)
			gotMotif++
			if p.Arm == "MOTIF-FAR" {
				motifFar = append(motifFar, p)
			} else {
				motifNear = append(motifNear, p)
			}
		}
		if gotMotif != wantMotif {
			closeAll()
			return fmt.Errorf("%s: %d candidates report Served but the builder served %d — "+
				"the arms and the served set disagree, so one of them is not measuring "+
				"what it says", corpus, gotMotif, wantMotif)
		}

		// TOKEN — the shipped entity/domain report, kept bridges only.
		tk, err := synthesize.BridgeComponentReport(ctx, svc.Search(), branch,
			fact.ID12(ri.ID()), synthesize.BridgeKindFromString("both"), synthesize.EffortHigh,
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

		// RANDOM and SAME-SUBJECT are drawn from the SAME post-AcceptSeed pool
		// the measured arms came from (review finding M-3). harnessSeeds used
		// to run a raw branch Search — 284 facts against the pool's 221 on
		// agentic-engineering, 440 against 313 on knomit-kb — so the controls
		// could contain pragmatic and discovered facts the MOTIF and TOKEN
		// arms structurally cannot. A floor measured on a different population
		// than the thing it floors flatters every rate against it.
		seeds, err := harnessSeeds(ctx, svc, branch, rep.SeedPaths)
		if err != nil {
			closeAll()
			return err
		}
		// fresh() applies to EVERY arm, not just the two it was written for.
		// The first fix deduped MOTIF and TOKEN and left RANDOM and TRAP alone,
		// which still let one duplicate through — a guard applied to some of
		// the things it protects is a guard with a hole in it.
		random = append(random, randomPairs(ctx, svc, branch, corpus, seeds, rng, harnessArmN)...)
		trap = append(trap, trapPairs(ctx, svc, branch, corpus, seeds, rng)...)
		closeAll()
	}

	// Priority order: the scarce primary arms first, then the comparison arm,
	// then the controls.
	claimedBy := map[string]string{}
	droppedTo := map[string]int{} // "TOKEN->MOTIF-FAR" etc.
	dedupe := func(in []harnessPair) []harnessPair {
		var out []harnessPair
		for _, p := range in {
			k := p.APath + "\x00" + p.BPath
			if first, dup := claimedBy[k]; dup {
				droppedTo[p.Arm+"->"+first]++
				continue
			}
			claimedBy[k] = p.Arm
			out = append(out, p)
		}
		return out
	}
	motifNear = dedupe(motifNear)
	motifFar = dedupe(motifFar)
	token = dedupe(token)
	trap = dedupe(trap)
	random = dedupe(random)

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
	uniq := map[string]bool{}
	for _, p := range all {
		counts[p.Arm]++
		uniq[p.APath+"\x00"+p.BPath] = true
	}
	if len(uniq) != len(all) {
		return fmt.Errorf("pack contains %d duplicate pair(s): a repeated pair is one pair "+
			"counted twice, and in a three-pair arm that is most of the arm", len(all)-len(uniq))
	}
	fmt.Fprintf(os.Stderr, "arms=%v total=%d\n", counts, len(all))
	return emit(map[string]any{
		"seed":       harnessSeed,
		"arm_target": harnessArmN,
		"arm_counts": counts,
		// Every drop, by WHICH arm lost the pair and WHICH kept it. The first
		// version reported a single "token pairs also claimed by a motif arm"
		// number that was really "token pairs dropped for any reason" —
		// including token-vs-token duplicates between merged and the corpus it
		// was seeded from, which is most of them. A count that does not mean
		// its label is worse than no count.
		"deduped_by_arm_pair": droppedTo,
		"projection":          "one pair per served group, seeded draw over its member pairs (E1's TOKEN-arm shape)",
		"populations":         "PRIMARY: real lab corpora only. The fixture supplement is a separate pack and is never pooled with this one.",
		"pack":                pack,
		"key":                 key,
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

// harnessSeeds narrows a branch scan to the report's own seed pool, so the
// controls are drawn from the population the measured arms came from.
//
// Filtering a full scan rather than re-deriving the pool: the paths come from
// MotifComponentReport, which built them with production's projection and
// filter, and SearchResult carries the motifs and labels SAME-SUBJECT needs.
// Re-deriving would be a second implementation of AcceptSeed in a measurement
// tool, which is the drift this phase spent its instrument work avoiding.
func harnessSeeds(ctx context.Context, svc *store.Service, branch string,
	pool []string) ([]store.SearchResult, error) {
	all, err := svc.Search().Search(ctx, branch, store.SearchOptions{Limit: 100000})
	if err != nil {
		return nil, err
	}
	keep := make(map[string]struct{}, len(pool))
	for _, p := range pool {
		keep[p] = struct{}{}
	}
	out := make([]store.SearchResult, 0, len(pool))
	for _, r := range all {
		if _, ok := keep[r.Path]; ok {
			out = append(out, r)
		}
	}
	return out, nil
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

// trapPairs builds the SAME-SUBJECT ceiling control: two facts sharing a
// canonical motif and at least one raw subject label. See the arm's note above
// for why raw overlap is NOT the same as "what the gate rejects".
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
				p.Arm = "SAME-SUBJECT"
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

// motifArmOf names a served candidate's arm from the candidate itself.
//
// Keyed on Lane, which the scorer assigned to THIS row, rather than on whether
// some served-token list contains its Token. See the call site for why the
// token is ambiguous.
func motifArmOf(c synthesize.ScoredMotifBridge) string {
	if c.Lane == string(synthesize.LaneFar) {
		return "MOTIF-FAR"
	}
	return "MOTIF-NEAR"
}
