package synthesize

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
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
func pairsOf(groups []BridgeSeedSet) map[string]struct{} {
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
	kept := suppressContained(high)
	t.Logf("contained-group suppression: %d of %d token-2 groups dropped",
		len(high)-len(kept), len(high))
	require.Positive(t, len(high)-len(kept),
		"token-2 makes contained groups by construction — a run that suppresses none "+
			"means the suppression is not reaching them")
	require.Equal(t, pairsOf(high), pairsOf(kept),
		"suppression must not lose a PAIR — a contained group's members all "+
			"survive inside the superset that displaced it")

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
