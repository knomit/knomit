package synthesize

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// Phase-3 acceptance: the 13 bridge-yield pairs replayed through the SHIPPED
// enumeration.
//
// The measurement they come from (summarizer doc, BRIDGE-YIELD MEASUREMENT)
// enumerated pairs sharing a canonical motif at df >= 2 while sharing zero
// stemmed entity/domain/path tokens, umbrella-aware — pairs invisible to the
// existing token detector by construction. It reported 10 on knomit-kb, 2 on
// core, 1 on agentic-engineering.
//
// This test asks whether the code that shipped finds the same population. The
// harness is READ-ONLY input: the labels file is parsed here and the facts are
// driven through enumerateMotifCandidates. Nothing is imported from
// .claude/harness/motif/, which the roadmap fences off from product code.
//
//	KNOMIT_MOTIF_ACCEPTANCE=1 go test ./internal/synthesize/ -run TestMotifAcceptance -v
const trainCanonPath = ".claude/harness/motif/data/train_canon.jsonl"

type canonRow struct {
	Key    string   `json:"key"`
	Input  string   `json:"input"`
	Output []string `json:"output"`
}

var (
	reDomain   = regexp.MustCompile(`(?m)^domain: \[(.*)\]$`)
	reEntities = regexp.MustCompile(`(?m)^entities: \[(.*)\]$`)
	reQuoted   = regexp.MustCompile(`'([^']*)'`)
)

// loadCanonCorpus reads one source corpus out of the harness labels file and
// projects it into the shape enumeration consumes.
func loadCanonCorpus(t *testing.T, corpus string) []factForLLM {
	t.Helper()
	f, err := os.Open("../../" + trainCanonPath)
	require.NoError(t, err, "harness fixture missing — this acceptance cannot run")
	defer f.Close()

	var out []factForLLM
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for sc.Scan() {
		var row canonRow
		require.NoError(t, json.Unmarshal(sc.Bytes(), &row))
		prefix := corpus + ":"
		if !strings.HasPrefix(row.Key, prefix) {
			continue
		}
		out = append(out, factForLLM{
			File:     strings.TrimPrefix(row.Key, prefix),
			Domain:   quotedList(reDomain, row.Input),
			Entities: quotedList(reEntities, row.Input),
			Motifs:   row.Output,
		})
	}
	require.NoError(t, sc.Err())
	return out
}

func quotedList(re *regexp.Regexp, input string) []string {
	m := re.FindStringSubmatch(input)
	if m == nil {
		return nil
	}
	var out []string
	for _, q := range reQuoted.FindAllStringSubmatch(m[1], -1) {
		if q[1] != "" {
			out = append(out, q[1])
		}
	}
	return out
}

// labelsFor computes the corpus's own subject-label distribution the way
// store.SubjectLabelDF does — same tokeniser, same one-set-per-fact counting.
func labelsFor(facts []factForLLM) store.SubjectLabelDF {
	d := store.SubjectLabelDF{DF: map[string]int{}, LiveFacts: len(facts)}
	for _, f := range facts {
		for tok := range fact.SubjectTokens(f.Entities, f.Domain, f.File) {
			d.DF[tok]++
		}
	}
	return d
}

// motifDFFor counts carriers per canonical motif over the corpus.
func motifDFFor(facts []factForLLM) motifDFFn {
	counts := map[string]int{}
	for _, f := range facts {
		seen := map[string]struct{}{}
		for _, m := range f.Motifs {
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			counts[m]++
		}
	}
	return func(c string) int { return counts[c] }
}

// pairsOf expands each candidate group into its MEMBER pairs — the unit the
// bridge-yield measurement counted.
//
// Keyed on the two paths and NOT on the token, deliberately: the token-2 tier
// merges canonical ids, so the same pair of facts can arrive under a different
// group label at a higher effort. Keying on the token would make monotonicity
// look violated by a relabelling.
func pairsOf(groups []enumeratedMotif) map[string]struct{} {
	out := map[string]struct{}{}
	for _, g := range groups {
		for i := 0; i < len(g.Members); i++ {
			for j := i + 1; j < len(g.Members); j++ {
				a, b := g.Members[i].File, g.Members[j].File
				if a > b {
					a, b = b, a
				}
				out[a+"|"+b] = struct{}{}
			}
		}
	}
	return out
}

// bridgeYieldSamples are the knomit-kb pairs the summarizer doc names in
// prose. A count can drift for many reasons; these are the specific
// cross-subsystem mechanisms the measurement pointed at, so they are asserted
// by name.
var bridgeYieldSamples = []string{
	"stale-closure-capture",
	"check-then-act-race",
	"single-source-of-truth",
	"duplicated-logic-drift",
}

func TestMotifAcceptance_BridgeYieldPairs(t *testing.T) {
	if os.Getenv("KNOMIT_MOTIF_ACCEPTANCE") != "1" {
		t.Skip("acceptance replay over the harness fixture; set KNOMIT_MOTIF_ACCEPTANCE=1")
	}
	facts := loadCanonCorpus(t, "knomit-kb")
	require.NotEmpty(t, facts)

	labels := labelsFor(facts)
	df := motifDFFor(facts)

	// No cluster assignment: every fact is its own community, so separation
	// never rejects. That matches the measurement, which asked only whether the
	// PAIR was subject-disjoint on a shared motif — it did not run Louvain.
	clusters := ClusterResult{}

	var motifed int
	for _, f := range facts {
		if len(f.Motifs) > 0 {
			motifed++
		}
	}

	high, hHigh := enumerateMotifCandidates(facts, clusters, identityResolver, df, labels, tierToken2)
	normal, hNormal := enumerateMotifCandidates(facts, clusters, identityResolver, df, labels, tierExact)

	t.Logf("corpus: %d facts, %d carrying motifs", len(facts), motifed)
	t.Logf("labels: %d distinct, live %d; operating point cut=%d umbrella=%d strict=%v",
		len(labels.DF), labels.LiveFacts, hHigh.Point.Cut, hHigh.Point.Umbrella, hHigh.Point.Strict)
	t.Logf("df band ceiling: %d; over-ceiling motifs: %d", hHigh.Ceiling, len(hHigh.OverCeilingNames))
	t.Logf("exact tier:  %d groups, %d pairs", len(normal), len(pairsOf(normal)))
	t.Logf("token2 tier: %d groups, %d pairs", len(high), len(pairsOf(high)))

	var tokens []string
	for _, g := range normal {
		tokens = append(tokens, fmt.Sprintf("%s(%d)", g.Token, len(g.Members)))
	}
	sort.Strings(tokens)
	t.Logf("exact-tier groups: %s", strings.Join(tokens, " "))

	// L1 on real data: how many of the token-2 groups are strictly contained in
	// another, and therefore never reach a slot. The reviewer measured 12 of
	// 113 at enumeration; this asserts the suppression actually removes them
	// before the budget is spent.
	kept, crossTier := suppressContained(high)
	t.Logf("contained-group suppression: %d of %d token-2 groups dropped (%d of them cross-tier)",
		len(high)-len(kept), len(high), crossTier)
	require.Positive(t, len(high)-len(kept),
		"token-2 makes contained groups by construction — a run that suppresses none "+
			"means the suppression is not reaching them")

	// TIER-AWARE, amended with the cross-tier ruling (Phase-4 rulings-3).
	//
	// The original assertion here was `pairsOf(high) == pairsOf(kept)` — "
	// suppression must not lose a PAIR". That was true while the superset
	// always survived: a contained group's members all lived on inside the
	// group that displaced it. The cross-tier rule inverts that case on
	// purpose — a token-2 family containing an exact group is DROPPED — so a
	// family's EXTRA members leave with it, and pairs only that family offered
	// are no longer offered. Equality is therefore the wrong assertion now, and
	// weakening it to "subset" alone would assert almost nothing.
	//
	// What replaces it is the property the ruling actually protects: whatever
	// else suppression removes, it never costs a pair the EXACT tier found.
	require.Subset(t, pairsOf(high), pairsOf(kept),
		"suppression only ever removes; it cannot invent a pair")
	require.Subset(t, pairsOf(kept), pairsOf(normal),
		"no pair the EXACT tier offers may be lost to suppression — that is what "+
			"the cross-tier rule exists to protect")
	// SAID PLAINLY, because a reader could take this block for a test OF the
	// cross-tier rule and it is not one: both assertions above hold under the
	// OLD rule too, and I verified that by sabotage — reverting to
	// superset-always-wins leaves this test green. They are invariants that
	// must survive the change, which is what the ruling asked be re-run. The
	// rule itself is pinned by TestRankAndCap_CrossTierTheExactGroupWins, which
	// does fail under that sabotage.
	t.Logf("pairs: %d enumerated at token-2, %d after suppression, %d at exact",
		len(pairsOf(high)), len(pairsOf(kept)), len(pairsOf(normal)))

	// The measurement's headline: ten subject-disjoint pairs on knomit-kb. The
	// shipped gate is FINER than the one that produced it (df-graded rather
	// than any-shared-token), so it should find at least as many.
	require.GreaterOrEqual(t, len(pairsOf(high)), 10,
		"the shipped enumeration must find at least the measured bridge-yield population")

	// The named samples, present as groups in their own right.
	byToken := map[string]int{}
	for _, g := range normal {
		byToken[g.Token] = len(g.Members)
	}
	for _, want := range bridgeYieldSamples {
		require.Containsf(t, byToken, want,
			"the doc names %q as a bridge-yield mechanism; enumeration must still find it", want)
		require.GreaterOrEqualf(t, byToken[want], 2, "%q must carry a real group", want)
	}

	// Monotone (MN10) on real data, not only on hand-built fixtures.
	require.Subset(t, pairsOf(high), pairsOf(normal))

	// And the two tiers must actually DIFFER, or the monotonicity assertion
	// above is comparing a set with itself.
	require.NotEqual(t, len(pairsOf(high)), len(pairsOf(normal)),
		"token-2 must admit something exact does not, or the ladder is flat on real data")
	require.Equal(t, hNormal.Ceiling, hHigh.Ceiling, "the band is a corpus property, not a tier one")
}

// ── Phase-4 measurement: the token-2 tier's noise shape (register entry 6) ──

// TestMotifMeasurement_Token2FoldStems reports WHAT the token-2 tier folds on.
//
// Register entry 6 asks two things: whether a stopword tightening is warranted,
// and — regardless — that "Phase-4 readers of tier numbers must know what
// produced them". The Phase-3 review named the suspected culprit as connective
// merges (`cost-of-delay`/`wolf-of-delay` on `of`+`delay`). This measures the
// actual distribution rather than assuming that shape.
//
// It runs over the FIXTURE corpus because that is the only population with
// enough recurring vocabulary for the tier to fold anything at scale: the real
// corpora carry 0-3 folds each. Read-only replay of the harness labels file,
// per the Phase-2 exception; nothing is imported from .claude/harness.
//
//	KNOMIT_MOTIF_ACCEPTANCE=1 go test ./internal/synthesize/ -run TestMotifMeasurement -v
func TestMotifMeasurement_Token2FoldStems(t *testing.T) {
	if os.Getenv("KNOMIT_MOTIF_ACCEPTANCE") != "1" {
		t.Skip("measurement replay over the harness fixture; set KNOMIT_MOTIF_ACCEPTANCE=1")
	}
	facts := loadCanonCorpus(t, "knomit-kb")
	require.NotEmpty(t, facts)

	// The canonical ids this corpus's bridging population actually carries.
	idSet := map[string]struct{}{}
	for _, f := range facts {
		for _, m := range f.Motifs {
			idSet[m] = struct{}{}
		}
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	stems := make([]map[string]struct{}, len(ids))
	for i, id := range ids {
		stems[i] = motifStems(id) // the SHIPPED predicate, not a copy
	}

	type kv struct {
		stems string
		n     int
	}

	// A fold is a pair of canonical ids the tier would join. Counted by the
	// stem-set that did it, so the shape of the noise is visible rather than
	// inferred.
	byStems := map[string]int{}
	folds := 0
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			sh := sharedMotifStems(stems[i], stems[j])
			if len(sh) < token2SharedStems {
				continue
			}
			folds++
			byStems[strings.Join(sh, "+")]++
		}
	}

	top := make([]kv, 0, len(byStems))
	for s, n := range byStems {
		top = append(top, kv{s, n})
	}
	sort.Slice(top, func(a, b int) bool {
		if top[a].n != top[b].n {
			return top[a].n > top[b].n
		}
		return top[a].stems < top[b].stems
	})

	// Two competing explanations for the noise, counted rather than assumed.
	// CONNECTIVES are what the Phase-3 review suspected (`of`+`delay`). Stem
	// PARTICIPATION tests the other hypothesis: that a few very common
	// mechanism words carry the folds regardless of part of speech.
	connectives := map[string]bool{
		"of": true, "to": true, "by": true, "in": true, "on": true, "for": true,
		"and": true, "or": true, "the": true, "a": true, "an": true, "at": true,
		"as": true, "with": true, "from": true, "into": true, "over": true,
		"under": true, "is": true, "are": true, "be": true, "it": true,
		"its": true, "that": true, "this": true, "not": true, "no": true,
		"than": true, "rather": true, "once": true, "when": true, "before": true,
		"after": true, "per": true, "via": true,
	}
	connFolds := 0
	participation := map[string]int{}
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			sh := sharedMotifStems(stems[i], stems[j])
			if len(sh) < token2SharedStems {
				continue
			}
			hasConn := false
			for _, st := range sh {
				participation[st]++
				if connectives[st] {
					hasConn = true
				}
			}
			if hasConn {
				connFolds++
			}
		}
	}
	partTop := make([]kv, 0, len(participation))
	for s, n := range participation {
		partTop = append(partTop, kv{s, n})
	}
	sort.Slice(partTop, func(a, b int) bool {
		if partTop[a].n != partTop[b].n {
			return partTop[a].n > partTop[b].n
		}
		return partTop[a].stems < partTop[b].stems
	})

	t.Logf("vocabulary: %d canonical ids; %d token-2 folds", len(ids), folds)
	t.Logf("folds involving a CONNECTIVE stem: %d of %d (%.1f%%)",
		connFolds, folds, 100*float64(connFolds)/float64(folds))
	t.Logf("top participating stems (folds each stem appears in):")
	for i, k := range partTop {
		if i >= 12 {
			break
		}
		t.Logf("  %4d  %s%s", k.n, k.stems, map[bool]string{true: "   [connective]"}[connectives[k.stems]])
	}
	require.Positive(t, folds, "precondition: the fixture must actually fold something")
	for i, k := range top {
		if i >= 25 {
			t.Logf("... and %d more distinct stem-sets", len(top)-i)
			break
		}
		t.Logf("  %4d x  %s", k.n, k.stems)
	}
}

// TestMotifMeasurement_WriteSupplementaryHarnessPack emits the H-track's
// SUPPLEMENTARY arm: motif pairs from the harness fixture corpus.
//
// Why a supplement rather than more of the primary arm (Phase-4 rulings-6):
// the real corpora serve one to three motif pairs each, so the primary MOTIF
// arms cannot reach E1's size. This answers the different question — "when the
// axis HAS population, are the pairs it produces good?" — and is never pooled
// with the primary evidence.
//
// TWO CAVEATS, both structural and both stated in the emitted file so they
// travel with the numbers:
//
//  1. LANELESS. The fixture is a labels file with no vectors, so there is no
//     SIMILAR_TO graph, laneOf assigns everything FAR by construction and the
//     cohesion floor never engages. These are ENUMERATED candidates, not
//     SERVED ones — no scoring, no budget, no lane.
//
//  2. Its facts are titles and bodies from the harness's own extraction, not
//     the corpus text a production judge would see.
//
//     KNOMIT_MOTIF_ACCEPTANCE=1 go test ./internal/synthesize/ -run TestMotifMeasurement_Write -v
func TestMotifMeasurement_WriteSupplementaryHarnessPack(t *testing.T) {
	if os.Getenv("KNOMIT_MOTIF_ACCEPTANCE") != "1" {
		t.Skip("fixture replay; set KNOMIT_MOTIF_ACCEPTANCE=1")
	}
	outDir := os.Getenv("KNOMIT_HARNESS_OUT")
	if outDir == "" {
		t.Skip("set KNOMIT_HARNESS_OUT to the directory the pack should be written to")
	}
	facts := loadCanonCorpus(t, "knomit-kb")
	require.NotEmpty(t, facts)

	cands, _ := enumerateMotifCandidates(facts, ClusterResult{}, identityResolver,
		motifDFFor(facts), labelsFor(facts), tierToken2)
	kept, _ := suppressContained(cands)
	require.NotEmpty(t, kept)

	byPath := map[string]factForLLM{}
	for _, f := range facts {
		byPath[f.File] = f
	}

	// One pair per group, deterministic: the first two members in path order.
	// No rng here — the fixture has no scoring to break ties with, so an
	// arbitrary-but-fixed rule is more honest than a seeded draw that looks
	// like sampling.
	type item struct {
		ID     string `json:"id"`
		ATitle string `json:"a_title"`
		ABody  string `json:"a_body"`
		BTitle string `json:"b_title"`
		BBody  string `json:"b_body"`
	}
	type keyRow struct {
		ID    string `json:"id"`
		Arm   string `json:"arm"`
		Token string `json:"token"`
		A     string `json:"a"`
		B     string `json:"b"`
	}
	var pack []item
	var key []keyRow
	for _, g := range kept {
		if len(g.Members) < 2 {
			continue
		}
		ms := append([]factForLLM(nil), g.Members...)
		sort.Slice(ms, func(i, j int) bool { return ms[i].File < ms[j].File })
		a, b := ms[0], ms[1]
		id := fmt.Sprintf("S%03d", len(pack)+1)
		clip := func(s string) string {
			s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
			if len(s) > 400 {
				s = s[:400]
			}
			return s
		}
		pack = append(pack, item{ID: id, ATitle: a.Title, ABody: clip(a.Body),
			BTitle: b.Title, BBody: clip(b.Body)})
		key = append(key, keyRow{ID: id, Arm: "MOTIF-FIXTURE", Token: g.Token, A: a.File, B: b.File})
		if len(pack) >= 12 {
			break
		}
	}
	require.NotEmpty(t, pack)

	payload := map[string]any{
		"arm": "MOTIF-FIXTURE (SUPPLEMENTARY — never pooled with the primary arms)",
		"n":   len(pack),
		"caveat": "ENUMERATED candidates, not served: the fixture carries no vectors, so there is no " +
			"SIMILAR_TO graph, every group is assigned FAR by construction, and neither the cohesion " +
			"floor nor the per-lane budget ever runs. Text is the harness's own extraction.",
		"pack": pack, "key": key,
	}
	blob, err := json.MarshalIndent(payload, "", " ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "supplementary.json"), blob, 0o644))
	t.Logf("wrote %d supplementary pairs from %d kept groups", len(pack), len(kept))
}
