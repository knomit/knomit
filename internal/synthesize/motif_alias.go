package synthesize

import (
	"context"
	"math"
	"sort"

	"knomit/internal/store"
)

// Alias-resolution pair selection: choosing which cluster comparisons the LLM
// judge is shown (blueprint §3.1).
//
// The embedding here is a PRE-BLOCK and nothing else. It decides what the judge
// LOOKS AT; the judge decides what merges. That separation is the design, not a
// nicety — measured, string and embedding similarity both keep adjacent-family
// false merges, which is why the judge sees carrier titles and has the final
// say (§12-E3).
//
// ── Operating points (designer ruling 2026-08-21, MN13) ───────────────────
//
// No constant below claims a fact about any corpus. Three compose, and each is
// classified where it is defined:
//
//	minJudgeVocabulary  STATISTICAL-VALIDITY FLOOR
//	judgeNeighboursPerCluster  PROMPT-SIZE BUDGET
//	judgePairPermille          SELECTION POLICY (derives a cosine per corpus)
//	maxJudgePairs              JUDGE-SLOT BUDGET
//
// The absolute cosine that ends up separating offered from unoffered pairs is
// never written down: it is wherever this corpus's own distribution and this
// session's budget happen to meet. The same code on another corpus cuts at a
// different number, which is the property MN13 exists to force.

// minJudgeVocabulary is a STATISTICAL-VALIDITY FLOOR — the third constant class
// (designer ruling 2026-08-21), alongside resource budgets and structural K's.
// It encodes nothing about any corpus; it states that a percentile computed
// over fewer than this many points is not an estimate.
//
// Below it the mechanical layer stands alone, and that is correct rather than a
// gap: a vocabulary this small has almost nothing to alias, and exact/stem
// matching already covers what it does have.
//
// The sharpest reason is about damage, not statistics. A judge shown garbage
// pairs occasionally hallucinates a merge; over-merge is the INVISIBLE failure,
// since nothing downstream can tell two mechanisms were fused. At six motifs
// one bad merge corrupts a third of the vocabulary. The floor guards over-merge
// exactly where it is proportionally largest.
//
// "Does nothing until it has evidence" is also the Phase-0 posture.
const minJudgeVocabulary = 12

// judgeNeighboursPerCluster is a PROMPT-SIZE BUDGET: how many nearest other
// clusters any one cluster may be compared against. It bounds the prompt, and
// says nothing about how similar a neighbour has to be.
const judgeNeighboursPerCluster = 3

// judgePairPermille is SELECTION POLICY: the fraction of this corpus's own
// observed pair-cosine distribution that is eligible at all, in parts per
// thousand. It derives an absolute cosine PER CORPUS and is itself no claim
// about any of them — the same shape as Phase 0's shortlistPerMille.
//
// Deliberately loose. It is a coarse pre-filter over what the budget then
// ranks; tightening it would start doing the judge's job, which is the one
// thing the pre-block must not do.
const judgePairPermille = 50

// maxJudgePairs is a JUDGE-SLOT BUDGET: the most cluster comparisons one review
// session will ever put in front of the judge, whatever the corpus looks like.
// It bounds what a corpus where name similarity does not discriminate can waste.
const maxJudgePairs = 6

// carrierTitlesPerCluster is a PROMPT-SIZE BUDGET: how many carrier titles are
// shown per cluster. Enough to expose an adjacent-family mismatch, few enough
// that a pair stays readable.
const carrierTitlesPerCluster = 4

// motifJudgePair is one comparison offered to the judge.
type motifJudgePair struct {
	A, B   store.MotifCluster
	ATitle []string
	BTitle []string
	Cos    float64
}

// motifAliasHealth reports what selection did, for the health lines. Nothing
// branches on any of it.
type motifAliasHealth struct {
	Vocabulary     int
	BelowFloor     bool
	Candidates     int
	Emitted        int
	OperatingPoint float64 // the cosine the last offered pair sits at, THIS corpus
	Failure        string
}

// selectMotifJudgePairs picks the cluster comparisons this session may spend.
//
// Degrades to "no pairs" rather than failing a session at every step. Alias
// resolution is an addition to review; a corpus that cannot embed its
// vocabulary should still get its ordinary pass.
func selectMotifJudgePairs(ctx context.Context, d Deps, branch string) ([]motifJudgePair, motifAliasHealth, error) {
	var h motifAliasHealth

	clusters, err := d.Motifs.Clusters(ctx, branch)
	if err != nil {
		h.Failure = "vocabulary read failed"
		return nil, h, nil
	}
	h.Vocabulary = len(clusters)

	// The statistical-validity floor. Below it a percentile is not an estimate,
	// and the mechanical layer standing alone is the right answer.
	if len(clusters) < minJudgeVocabulary {
		h.BelowFloor = true
		return nil, h, nil
	}

	emb := d.RI.Embedder()
	if emb == nil {
		h.Failure = "no embedder"
		return nil, h, nil
	}

	// MN9: motif names embed through the SHORT-STRING template descriptor, the
	// measured "title-hack" rendering — never the model card's task prompts,
	// which measured worse (task-sweep, summarizer doc). This looks wrong and
	// is right, which is why it has a guard test.
	names := make([]string, len(clusters))
	for i, c := range clusters {
		names[i] = c.CanonicalID
	}
	vecs, err := emb.EmbedShortStrings(ctx, names)
	if err != nil || len(vecs) != len(clusters) {
		h.Failure = "vocabulary embedding failed"
		return nil, h, nil
	}
	for i := range vecs {
		normalizeVec(vecs[i])
	}

	answered, err := d.Motifs.AnsweredPairs(ctx, branch)
	if err != nil {
		h.Failure = "verdict history read failed"
		return nil, h, nil
	}

	// All pairs, scored. The distribution this builds is what the percentile is
	// taken over — this corpus's own, recomputed every session, never a stored
	// or configured number.
	type scored struct {
		i, j int
		cos  float64
	}
	var all []scored
	for i := range clusters {
		for j := i + 1; j < len(clusters); j++ {
			all = append(all, scored{i, j, dot(vecs[i], vecs[j])})
		}
	}
	if len(all) == 0 {
		return nil, h, nil
	}
	sort.Slice(all, func(x, y int) bool {
		if all[x].cos != all[y].cos {
			return all[x].cos > all[y].cos
		}
		// Tiebreak on cluster rank. sort.Slice is documented as NOT stable, so
		// without this the order of equal-cosine pairs is UNSPECIFIED — it
		// happens to be reproducible under the current pdqsort, which is an
		// implementation detail and not something to spend a session's judge
		// budget on. Measured honestly: deleting this changes nothing today.
		// It is here to make the order specified rather than incidental.
		if all[x].i != all[y].i {
			return all[x].i < all[y].i
		}
		return all[x].j < all[y].j
	})

	// Selection policy: the top per-mille slice of this corpus's own
	// distribution is eligible. At least one pair, so a corpus above the floor
	// is never silently excluded by integer division.
	eligible := len(all) * judgePairPermille / 1000
	if eligible < 1 {
		eligible = 1
	}
	if eligible > len(all) {
		eligible = len(all)
	}
	all = all[:eligible]
	h.Candidates = len(all)

	// Then the two budgets, applied in rank order.
	perCluster := map[int]int{}
	var out []motifJudgePair
	for _, s := range all {
		if len(out) >= maxJudgePairs {
			break
		}
		// Prompt-size budget, per cluster on both sides.
		if perCluster[s.i] >= judgeNeighboursPerCluster || perCluster[s.j] >= judgeNeighboursPerCluster {
			continue
		}
		a, b := clusters[s.i], clusters[s.j]
		// Never re-ask a question that has a binding answer. Verdicts stop
		// binding when either cluster's membership moves, so this excludes
		// settled pairs without freezing them forever.
		if _, done := answered[motifPairKey(a.ClusterKey, b.ClusterKey)]; done {
			continue
		}
		aTitles, err := d.Motifs.CarrierTitles(ctx, branch, a.ClusterKey, carrierTitlesPerCluster)
		if err != nil {
			continue
		}
		bTitles, err := d.Motifs.CarrierTitles(ctx, branch, b.ClusterKey, carrierTitlesPerCluster)
		if err != nil {
			continue
		}
		perCluster[s.i]++
		perCluster[s.j]++
		out = append(out, motifJudgePair{A: a, B: b, ATitle: aTitles, BTitle: bTitles, Cos: s.cos})
	}
	h.Emitted = len(out)
	if len(out) > 0 {
		// Not a threshold anyone chose: whatever cosine the last funded pair
		// happens to sit at in THIS repo. Reported as a corpus fingerprint —
		// the same code elsewhere prints a different number.
		h.OperatingPoint = out[len(out)-1].Cos
	}
	return out, h, nil
}

// motifPairKey mirrors the store's pair identity so the exclusion set and the
// verdict table agree on what "the same pair" means.
func motifPairKey(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "\x00" + b
}

func normalizeVec(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	// Degenerate-norm guard: a numerical floor, not a similarity threshold.
	if sum < 1e-12 {
		return
	}
	n := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= n
	}
}

func dot(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
