package synthesize

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash/fnv"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Pair selection for the alias judge. What is asserted here is the SHAPE of
// the selection — floor, budgets, exclusion, determinism — not any particular
// cosine. There is no cosine to assert: the operating point is wherever this
// corpus's own distribution and this session's budget meet.

// motifVocabEnv builds a corpus with n distinct motifs, one carrier each,
// named so no two group mechanically.
func motifVocabEnv(t *testing.T, n int) *restatementEnv {
	t.Helper()
	env := newRestatementEnv(t, 0)
	for i := range n {
		env.writeFactWithMotifs(
			fmt.Sprintf("kb/m%d.md", i), fmt.Sprintf("Carrier %d", i), "body",
			[]string{fmt.Sprintf("mechanism-%s", numberWord(i))})
	}
	require.NoError(t, env.svc.Motifs().RebuildAliases(context.Background(), env.branch))
	return env
}

// numberWord keeps generated motif names to the 2–4 word kebab shape the field
// contract requires — "mechanism-17" would be rejected as a digit token.
func numberWord(i int) string {
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot",
		"golf", "hotel", "india", "juliet", "kilo", "lima", "mike", "november",
		"oscar", "papa", "quebec", "romeo", "sierra", "tango"}
	return words[i%len(words)]
}

// The statistical-validity floor: below it, nothing is offered at all. A
// percentile over a handful of points is not an estimate, and a hallucinated
// merge costs proportionally most in a tiny vocabulary.
func TestMotifJudgeSelection_BelowFloorOffersNothing(t *testing.T) {
	env := motifVocabEnv(t, minJudgeVocabulary-1)
	pairs, h, err := selectMotifJudgePairs(context.Background(), env.deps(), env.branch)
	require.NoError(t, err)
	require.Empty(t, pairs, "a vocabulary below the floor must offer no comparisons")
	require.True(t, h.BelowFloor)
	require.Equal(t, minJudgeVocabulary-1, h.Vocabulary)
}

func TestMotifJudgeSelection_AtFloorOffersSomething(t *testing.T) {
	env := motifVocabEnv(t, minJudgeVocabulary)
	pairs, h, err := selectMotifJudgePairs(context.Background(), env.deps(), env.branch)
	require.NoError(t, err)
	require.False(t, h.BelowFloor)
	require.NotEmpty(t, pairs,
		"at the floor the mechanism must actually engage — a floor that never lets "+
			"anything through is indistinguishable from the feature being off")
}

// The judge-slot budget is a hard cap whatever the corpus looks like: a corpus
// where every name resembles every other must not be able to spend more.
func TestMotifJudgeSelection_SlotBudgetCapsAFloodCorpus(t *testing.T) {
	env := newRestatementEnv(t, 0)
	// Names crowded into a narrow arc at DISTINCT angles. An earlier fixture put
	// every name on the identical point; that made all cosines tie, so rank
	// order became index order, every top-ranked pair shared cluster 0, and the
	// PER-CLUSTER budget bound at 3 before the slot budget was ever reached.
	// The test passed with the slot cap deleted. Distinct-but-close angles
	// spread the top of the ranking across many clusters, which is what a real
	// flood corpus looks like and what the slot budget exists for.
	env.emb.vectorFor = func(text string) []float32 { return axisVector(0.15 * unitHash(text)) }
	for i := range 40 {
		env.writeFactWithMotifs(
			fmt.Sprintf("kb/m%d.md", i), fmt.Sprintf("Carrier %d", i), "body",
			[]string{fmt.Sprintf("mechanism-%s-%s", numberWord(i), numberWord(i/20))})
	}
	require.NoError(t, env.svc.Motifs().RebuildAliases(context.Background(), env.branch))

	pairs, h, err := selectMotifJudgePairs(context.Background(), env.deps(), env.branch)
	require.NoError(t, err)
	require.LessOrEqual(t, len(pairs), maxJudgePairs,
		"the slot budget must bound a flood corpus — this is what stops a repo where "+
			"name similarity does not discriminate from spending without limit")
	require.Equal(t, len(pairs), h.Emitted)
}

// The prompt-size budget: no single cluster may dominate the offered set.
// Without it one hub-like name consumes every slot and the rest of the
// vocabulary is never examined.
func TestMotifJudgeSelection_NoClusterExceedsItsNeighbourBudget(t *testing.T) {
	env := newRestatementEnv(t, 0)
	// One name sits at the centre; all others cluster tightly around it, so
	// unbudgeted selection would pair everything with the hub.
	env.emb.vectorFor = func(text string) []float32 {
		if strings.Contains(text, "hub") {
			return axisVector(0)
		}
		return axisVector(0.01)
	}
	env.writeFactWithMotifs("kb/hub.md", "Hub", "body", []string{"hub-mechanism"})
	for i := range 20 {
		env.writeFactWithMotifs(
			fmt.Sprintf("kb/m%d.md", i), fmt.Sprintf("Carrier %d", i), "body",
			[]string{fmt.Sprintf("spoke-%s", numberWord(i))})
	}
	require.NoError(t, env.svc.Motifs().RebuildAliases(context.Background(), env.branch))

	pairs, _, err := selectMotifJudgePairs(context.Background(), env.deps(), env.branch)
	require.NoError(t, err)

	perCluster := map[string]int{}
	for _, p := range pairs {
		perCluster[p.A.ClusterKey]++
		perCluster[p.B.ClusterKey]++
	}
	for key, n := range perCluster {
		require.LessOrEqual(t, n, judgeNeighboursPerCluster,
			"cluster %s appears in %d pairs, above its neighbour budget", key, n)
	}
}

// A pair with a binding verdict is never re-offered. This is what makes the
// pass incremental: without it a stable corpus re-asks the same questions every
// session until its budget is gone.
func TestMotifJudgeSelection_AnsweredPairsAreNotReOffered(t *testing.T) {
	ctx := context.Background()
	env := motifVocabEnv(t, minJudgeVocabulary+4)

	first, _, err := selectMotifJudgePairs(ctx, env.deps(), env.branch)
	require.NoError(t, err)
	require.NotEmpty(t, first)

	// Answer every offered pair — decline, so nothing merges and the vocabulary
	// is unchanged apart from the verdicts themselves.
	for _, p := range first {
		require.NoError(t, env.svc.Motifs().RecordJudgeDecline(ctx, env.branch,
			p.A.CanonicalID, p.B.CanonicalID))
	}

	second, _, err := selectMotifJudgePairs(ctx, env.deps(), env.branch)
	require.NoError(t, err)
	for _, p := range second {
		for _, done := range first {
			require.False(t,
				p.A.ClusterKey == done.A.ClusterKey && p.B.ClusterKey == done.B.ClusterKey,
				"pair %s/%s was already answered and must not be re-offered",
				p.A.CanonicalID, p.B.CanonicalID)
		}
	}
}

// Selection must be deterministic: the same corpus offers the same pairs on a
// rerun, so a session's judge budget is not spent on the luck of an iteration
// order.
//
// Honest note on what this does and does not catch. Deleting the cosine
// tiebreak in selectMotifJudgePairs does NOT move this test — measured, not
// assumed: Go's sort is reproducible for a fixed input even on equal elements,
// so the tiebreak is insurance against unspecified behaviour rather than
// something currently load-bearing. What this test WOULD catch is the failure
// that actually threatens determinism here: a map iterated in ranking or
// selection. It is a regression guard, and it is worth saying so rather than
// listing it as sabotage-verified when it is not.
func TestMotifJudgeSelection_IsDeterministic(t *testing.T) {
	ctx := context.Background()
	env := motifVocabEnv(t, minJudgeVocabulary+6)

	var runs [][]string
	for range 3 {
		pairs, _, err := selectMotifJudgePairs(ctx, env.deps(), env.branch)
		require.NoError(t, err)
		var keys []string
		for _, p := range pairs {
			keys = append(keys, p.A.ClusterKey+"|"+p.B.ClusterKey)
		}
		runs = append(runs, keys)
	}
	require.Equal(t, runs[0], runs[1])
	require.Equal(t, runs[1], runs[2])
}

// Every offered pair carries carrier titles for BOTH sides. The titles are the
// over-merge guard: string similarity alone keeps adjacent-family false merges
// (§12-E3), and a pair shown without them asks the judge to decide on names.
func TestMotifJudgeSelection_EveryPairCarriesTitlesForBothSides(t *testing.T) {
	env := motifVocabEnv(t, minJudgeVocabulary+2)
	pairs, _, err := selectMotifJudgePairs(context.Background(), env.deps(), env.branch)
	require.NoError(t, err)
	require.NotEmpty(t, pairs)
	for _, p := range pairs {
		require.NotEmpty(t, p.ATitle, "pair %s has no carrier titles", p.A.CanonicalID)
		require.NotEmpty(t, p.BTitle, "pair %s has no carrier titles", p.B.CanonicalID)
	}
}

// Degradation, not failure: alias resolution is an ADDITION to review, so a
// corpus that cannot embed its vocabulary still gets its ordinary pass.
func TestMotifJudgeSelection_NoEmbedderDegradesQuietly(t *testing.T) {
	env := newRestatementEnvWithoutEmbedder(t, 0)
	for i := range minJudgeVocabulary + 2 {
		env.writeFactWithMotifs(
			fmt.Sprintf("kb/m%d.md", i), fmt.Sprintf("Carrier %d", i), "body",
			[]string{fmt.Sprintf("mechanism-%s", numberWord(i))})
	}
	require.NoError(t, env.svc.Motifs().RebuildAliases(context.Background(), env.branch))

	pairs, h, err := selectMotifJudgePairs(context.Background(), env.deps(), env.branch)
	require.NoError(t, err, "a missing embedder must not fail the session")
	require.Empty(t, pairs)
	require.NotEmpty(t, h.Failure,
		"...but it must SAY so — a silent skip and a clean corpus must not look alike")
}

// MN9: motif names embed through the short-string descriptor, never the
// document or query template. Asserted by counting which embedder entry point
// selection used.
func TestMotifJudgeSelection_MN9_NamesEmbedThroughTheShortStringTemplate(t *testing.T) {
	env := motifVocabEnv(t, minJudgeVocabulary+2)
	before := env.emb.documentCalls.Load()
	shortBefore := env.emb.shortStringCalls.Load()

	_, _, err := selectMotifJudgePairs(context.Background(), env.deps(), env.branch)
	require.NoError(t, err)

	require.Greater(t, env.emb.shortStringCalls.Load(), shortBefore,
		"motif names must embed through EmbedShortStrings (the title-hack descriptor)")
	require.Equal(t, before, env.emb.documentCalls.Load(),
		"a motif name is not a document — the task-prompt rendering measured WORSE, "+
			"which is exactly the mistake this guard exists to catch")
}

// ── MN13 ──────────────────────────────────────────────────────────────────

// motifBudgetLiterals are the numeric literals the alias-selection path is
// allowed to contain, each with the reason it is not a claim about a corpus.
//
// Same gate as phase 0's, on this phase's file. An absolute cosine is what this
// kind of code grows every time it is written, and it looks reasonable on every
// occasion.
var motifBudgetLiterals = map[string]string{
	"12":    "minJudgeVocabulary: statistical-validity floor — a percentile over fewer points is not an estimate",
	"3":     "judgeNeighboursPerCluster: prompt-size budget",
	"50":    "judgePairPermille: selection policy, per-mille of THIS corpus's own distribution",
	"6":     "maxJudgePairs: judge-slot budget",
	"4":     "carrierTitlesPerCluster: prompt-size budget",
	"1e-12": "degenerate-norm guard in cosine; a numerical floor, not a similarity threshold",
	"1000":  "per-mille denominator",
	"0":     "loop and slice bounds",
	"1":     "loop and slice bounds",
	"2":     "loop and slice bounds",
}

// TestMN13_NoCorpusPropertyConstantsInAliasSelection fails if a numeric literal
// appears in the selection path without being declared above.
func TestMN13_NoCorpusPropertyConstantsInAliasSelection(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "motif_alias.go", nil, 0)
	require.NoError(t, err)

	seen := 0
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || (lit.Kind != token.FLOAT && lit.Kind != token.INT) {
			return true
		}
		_, allowed := motifBudgetLiterals[lit.Value]
		require.Truef(t, allowed,
			"numeric literal %s at %s is not a declared budget. An absolute cosine, or "+
				"any number claiming a fact about a corpus, is forbidden (MN13): derive it "+
				"from the repo's own data or do not have it.",
			lit.Value, fset.Position(lit.Pos()))
		seen++
		return true
	})
	require.Greater(t, seen, 4, "the audit must actually be reading literals")
}

// The operating point is REPORTED, never chosen — and it is a corpus
// fingerprint, so two different corpora must produce two different numbers from
// the same code. That is the property MN13 exists to force, asserted rather
// than asserted-about-in-a-comment.
func TestMN13_OperatingPointIsPerCorpus(t *testing.T) {
	ctx := context.Background()

	// Angles come from a hash of the name, not its length: an earlier fixture
	// used len(text)%7 and collapsed distinct names onto identical vectors, so
	// BOTH corpora reported an operating point of exactly 1.0 and the test
	// compared two constants. Distinct names must land at distinct angles or
	// this asserts nothing about distributions.
	arc := func(width float64) func(string) []float32 {
		return func(text string) []float32 { return axisVector(width * unitHash(text)) }
	}

	tight := motifVocabEnv(t, minJudgeVocabulary+6)
	// Names crowded into a narrow arc: this corpus's pairs are all similar.
	tight.emb.vectorFor = arc(0.2)
	_, hTight, err := selectMotifJudgePairs(ctx, tight.deps(), tight.branch)
	require.NoError(t, err)

	spread := motifVocabEnv(t, minJudgeVocabulary+6)
	// Names spread around a half-circle: this corpus's pairs are dissimilar.
	spread.emb.vectorFor = arc(math.Pi)
	_, hSpread, err := selectMotifJudgePairs(ctx, spread.deps(), spread.branch)
	require.NoError(t, err)

	require.NotEqual(t, hTight.OperatingPoint, hSpread.OperatingPoint,
		"the same code must cut at a DIFFERENT absolute cosine on a different corpus — "+
			"if these matched, the operating point would be a constant in disguise")
	require.Greater(t, hTight.OperatingPoint, hSpread.OperatingPoint,
		"the crowded corpus should cut higher: its own distribution is tighter")
}

// unitHash maps text to a stable value in [0,1). Used to place motif names at
// distinct, deterministic angles: a fixture that derives position from a
// property several names share (length, a small modulus) puts them on top of
// each other and silently turns a distribution test into a constant test.
func unitHash(text string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	return float64(h.Sum64()%1_000_000) / 1_000_000
}
