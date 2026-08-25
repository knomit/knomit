package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"

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
// HONEST NOTE ON WHEN IT BINDS. The eligible slice is len(pairs)*permille/1000,
// and the budgets downstream cap the offered set at maxJudgePairs. Above
// roughly sixteen clusters the percentile slice already exceeds what the
// budgets will spend, so the budgets bind and this does not. It is doing real
// work only on SMALL vocabularies — between the statistical-validity floor and
// that crossover — where it keeps a handful of clusters from offering their
// entire pair space.
//
// Left in rather than removed, and documented rather than quietly retained:
// it is the mechanism that keeps selection percentile-derived by construction
// (MN13), so a future change to the budgets cannot silently turn the pre-block
// into a rank-only cut. But a reader should not believe it is filtering a large
// corpus, because it is not.
//
// Deliberately loose regardless. It is a coarse pre-filter over what the budget
// then ranks; tightening it would start doing the judge's job, which is the one
// thing the pre-block must not do.
const judgePairPermille = 50

// maxJudgePairs is a JUDGE-SLOT BUDGET: the most cluster comparisons one review
// session will ever put in front of the judge, whatever the corpus looks like.
// It bounds what a corpus where name similarity does not discriminate can waste.
const maxJudgePairs = 6

// maxPairsMaterialized is a MEMORY BUDGET on the pair scan: the most cluster
// pairs held in memory at once. At the default it bounds the vocabulary
// actually compared to a few hundred of the corpus's most-used names, which is
// where aliasing matters — a name nothing reuses has nothing to alias with.
const maxPairsMaterialized = 200_000

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
	// Truncated counts clusters dropped from the pair scan by the memory
	// budget. Reported because a silent cap reads as full coverage.
	Truncated int
	Failure   string
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
	// Materialising every pair is O(V^2) in MEMORY, not just time: a 2,000-word
	// vocabulary is two million structs. maxPairsMaterialized is a RESOURCE
	// BUDGET (MN13) bounding that, and the truncation is SAFE because the
	// vocabulary is ordered most-frequent-first — so what is dropped is the
	// tail of rarely-used names, which the df>=2 reasoning already treats as the
	// least valuable end. Reported, never silent: a cap nothing mentions reads
	// as complete coverage.
	type scored struct {
		i, j int
		cos  float64
	}
	scanned := len(clusters)
	for scanned > 1 && scanned*(scanned-1)/2 > maxPairsMaterialized {
		scanned--
	}
	if scanned < len(clusters) {
		h.Truncated = len(clusters) - scanned
	}
	all := make([]scored, 0, scanned*(scanned-1)/2)
	for i := range scanned {
		for j := i + 1; j < scanned; j++ {
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

// ── the work item ─────────────────────────────────────────────────────────

// motifAliasStepType is the work-item step type for the alias judge.
const motifAliasStepType = "motif_alias"

// motifAliasPriority places alias items just below prune's cluster band and
// above the discover/reflect band: vocabulary resolution informs the surfaces
// and the §7 signal, so it is worth doing before discovery spends on bridges,
// but never at the expense of consolidation itself.
const motifAliasPriority = 1.2

// motifJudgeItem is one pair as the judge sees it. Field names match the
// response schema's `a`/`b` so a small model does not have to map between them.
type motifJudgeItem struct {
	A         string   `json:"a"`
	AAlso     []string `json:"a_also,omitempty"`
	ACarriers []string `json:"a_carriers"`
	B         string   `json:"b"`
	BAlso     []string `json:"b_also,omitempty"`
	BCarriers []string `json:"b_carriers"`
}

// motifAliasResponseSchema is what the judge must return.
//
// `mechanism` is required-in-practice rather than required-in-schema: the
// schema cannot express "required only when same_mechanism is true", and the
// write path refuses a merge without it anyway. Stating it in the description
// and enforcing it at the write path is the honest arrangement — a schema that
// claimed to require it unconditionally would reject every legitimate decline.
const motifAliasResponseSchema = `{
  "type": "object",
  "properties": {
    "verdicts": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "a": {"type": "string", "description": "The a name, exactly as given."},
          "b": {"type": "string", "description": "The b name, exactly as given."},
          "same_mechanism": {"type": "boolean"},
          "mechanism": {"type": "string", "description": "One sentence naming the mechanism both describe. Required when same_mechanism is true."}
        },
        "required": ["a", "b", "same_mechanism"]
      }
    }
  },
  "required": ["verdicts"]
}`

// motifAliasVerdict is one decoded verdict.
type motifAliasVerdict struct {
	A             string `json:"a"`
	B             string `json:"b"`
	SameMechanism bool   `json:"same_mechanism"`
	Mechanism     string `json:"mechanism"`
}

type motifAliasResult struct {
	Verdicts []motifAliasVerdict `json:"verdicts"`
}

// parseMotifAliasResponse decodes and probes the envelope.
//
// The probe is not redundant with the typed unmarshal (invariant 51d85fcd): a
// response carrying its content under the wrong key unmarshals to a
// zero-valued result, applies as a silent no-op, and the item advances with the
// work gone. Presence on the raw object is what makes that loud and retryable.
func parseMotifAliasResponse(raw string) (motifAliasResult, error) {
	var out motifAliasResult
	text := extractJSON(raw)
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return out, fmt.Errorf("parse motif alias response: %w", err)
	}
	if err := requireResponseKey(text, "verdicts"); err != nil {
		return out, err
	}
	return out, nil
}

// validateMotifAliasVerdicts checks the response against what was actually
// offered.
//
// Every verdict must name a pair this item asked about. A judge that invents a
// pair — or answers about one from a previous item — would otherwise merge two
// clusters nobody put in front of it, and over-merge is the failure nothing
// downstream can detect.
func validateMotifAliasVerdicts(res motifAliasResult, offered []motifJudgeItem) error {
	valid := map[string]struct{}{}
	for _, it := range offered {
		valid[motifPairKey(it.A, it.B)] = struct{}{}
	}
	seen := map[string]struct{}{}
	for _, v := range res.Verdicts {
		key := motifPairKey(v.A, v.B)
		if _, ok := valid[key]; !ok {
			return fmt.Errorf("verdict names %q/%q, which was not offered in this item", v.A, v.B)
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("verdict for %q/%q appears more than once", v.A, v.B)
		}
		seen[key] = struct{}{}
		if v.SameMechanism && strings.TrimSpace(v.Mechanism) == "" {
			return fmt.Errorf("merge of %q/%q names no shared mechanism", v.A, v.B)
		}
	}
	return nil
}

// applyMotifAliasVerdicts records the judge's decisions and rebuilds.
//
// A verdict that fails to record does not fail the item: the rest of the batch
// is still good work, and the pair will simply be offered again. Losing a whole
// batch to one bad row is the worse trade.
func applyMotifAliasVerdicts(ctx context.Context, d Deps, branch string, res motifAliasResult) error {
	for _, v := range res.Verdicts {
		var err error
		if v.SameMechanism {
			err = d.Motifs.RecordJudgeMerge(ctx, branch, v.A, v.B, v.Mechanism)
		} else {
			err = d.Motifs.RecordJudgeDecline(ctx, branch, v.A, v.B)
		}
		if err != nil {
			log.Warn().Err(err).Str("a", v.A).Str("b", v.B).
				Msg("motif alias: verdict not recorded; the pair will be offered again")
		}
	}
	// Decisions take effect here, together: the mechanical layer and the judge
	// overlay are applied in one rebuild so the result is a deterministic
	// function of (facts, decisions) rather than an accumulation of edits.
	//
	// This is the SECOND rebuild of a session that records verdicts. The first
	// runs in Plan, before selection, and is what makes these verdicts bind to
	// CURRENT membership — a session that rebuilt only here would record every
	// verdict against membership one session stale, so a decline would stop
	// binding the moment anything in the corpus moved and the pass would
	// re-offer it forever. That is the exact incrementality RecordJudgeDecline
	// exists to buy.
	return d.Motifs.RebuildAliases(ctx, branch)
}

// planMotifAliasWork selects pairs and enqueues at most one alias item per
// session.
//
// One item, not one per pair: §3.1 specifies ONE bounded prompt, and a judge
// deciding several pairs together can see that two of them are the same
// question asked twice.
//
// Degrades to "no item" rather than failing the session, and records health on
// every path — a corpus whose vocabulary could not be read must not look like
// one with nothing to resolve.
func planMotifAliasWork(ctx context.Context, d Deps, sess *store.PipelineSession, branch string) error {
	pairs, health, err := selectMotifJudgePairs(ctx, d, branch)
	recordMotifAliasHealth(sess, health)
	if err != nil || len(pairs) == 0 {
		return nil
	}
	items := make([]motifJudgeItem, 0, len(pairs))
	for _, p := range pairs {
		items = append(items, motifJudgeItem{
			A:         p.A.CanonicalID,
			AAlso:     otherMembers(p.A.Members, p.A.CanonicalID),
			ACarriers: p.ATitle,
			B:         p.B.CanonicalID,
			BAlso:     otherMembers(p.B.Members, p.B.CanonicalID),
			BCarriers: p.BTitle,
		})
	}
	payload, err := json.Marshal(items)
	if err != nil {
		return wrapf(reviewTool, err, "motif alias: marshal pairs")
	}
	return d.Pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   motifAliasStepType,
		ClusterKey: "motif-alias",
		FactsJSON:  string(payload),
		Priority:   motifAliasPriority,
	})
}

// otherMembers is the cluster's spellings apart from the one being displayed —
// what "already resolves to this name" means in the prompt.
func otherMembers(members []string, canonical string) []string {
	var out []string
	for _, m := range members {
		if m != canonical {
			out = append(out, m)
		}
	}
	return out
}

// recordMotifAliasHealth APPENDS this phase's descriptors to the session's
// health lines.
//
// Appends rather than assigns, unlike recordRestatementHealth: by Phase 2 the
// field has two independent producers, and the last writer to ASSIGN silently
// deletes the other's lines. Health is the only channel through which either
// mechanism reports "I ran and found nothing" — losing it makes a broken
// subsystem indistinguishable from a clean corpus, which is the failure the
// Phase-0 descriptors exist to prevent in the first place.
//
// TestMotifAliasHealth_CoexistsWithRestatementLines is the guard; it fails if
// either producer starts clobbering the other.
func recordMotifAliasHealth(sess *store.PipelineSession, h motifAliasHealth) {
	if sess == nil {
		return
	}
	sess.Health = append(sess.Health, motifAliasHealthLines(h)...)
}

func motifAliasHealthLines(h motifAliasHealth) []string {
	if h.Failure != "" {
		return []string{fmt.Sprintf("motif aliases: %s (vocabulary %d)", h.Failure, h.Vocabulary)}
	}
	if h.BelowFloor {
		return []string{fmt.Sprintf(
			"motif aliases: vocabulary %d below the %d-cluster validity floor; mechanical layer only",
			h.Vocabulary, minJudgeVocabulary)}
	}
	lines := []string{
		fmt.Sprintf("motif aliases: vocabulary %d, %d candidates, %d offered",
			h.Vocabulary, h.Candidates, h.Emitted),
	}
	// The operating point is meaningless when nothing was offered — there is no
	// "last funded pair" to read it off. Printing 0.000 invited it to be read as
	// a corpus whose names are all dissimilar, which is a claim about the
	// corpus rather than about the session.
	if h.Emitted > 0 {
		lines = append(lines, fmt.Sprintf(
			"motif aliases: operating point name-cos %.3f (this corpus, this session)",
			h.OperatingPoint))
	}
	if h.Truncated > 0 {
		lines = append(lines, fmt.Sprintf(
			"motif aliases: %d least-used clusters dropped from the pair scan (memory budget)",
			h.Truncated))
	}
	return lines
}
