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
