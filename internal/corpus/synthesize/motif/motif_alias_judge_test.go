package motif

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"knomit/internal/corpus/synthesize/restatement"
	"knomit/internal/engine/store/model"

	"github.com/stretchr/testify/require"
)

// ── the over-merge guard ──────────────────────────────────────────────────

// nearMissPairs are motif pairs that a name-similarity pre-block will
// preferentially NOMINATE and that must NOT be merged. Each targets one line of
// the prompt's do-not-merge list.
//
// Over-merge is the invisible failure: two mechanisms fused into one cluster
// inflate df, pollute bridging, and nothing downstream can tell. Under-merge
// costs a wasted vocabulary slot, which §3.1 already accepts. That asymmetry is
// why the fixtures below are all negatives but one.
var nearMissPairs = []struct {
	a, b string
	why  string
}{
	{"retry-storm", "thundering-herd",
		"co-occurrence: genuinely different mechanisms that show up together — the classic false merge"},
	{"silent-fallback", "cascading-failure",
		"cause and effect"},
	{"cache-invalidation", "stale-read",
		"a mechanism and its symptom"},
	{"write-amplification", "read-amplification",
		"near-identical names, opposite mechanisms — where name similarity is most misleading"},
	{"unmonitored-expiry", "automatic-renewal",
		"a problem and its remedy: carriers discuss both, so the names read as one topic " +
			"(designer addition — the anti-remedy confusion earned its own Block B rule after " +
			"E2 measured a unanimous failure without it)"},
	{"fail-open-default", "fail-closed-default",
		"polarity: negation-blindness is the measured failure of the embedding pre-block, " +
			"so it will preferentially nominate exactly this class (designer addition)"},
}

// truePair is the positive control. Without it the suite passes for a judge
// that refuses everything, which is not the behaviour we want either — a
// vocabulary that never resolves is the axis failing quietly.
var truePair = struct{ a, b string }{"silent-fallback", "quiet-degradation"}

// ── response handling ─────────────────────────────────────────────────────

func offeredFixture() []JudgeItem {
	var out []JudgeItem
	for _, p := range nearMissPairs {
		out = append(out, JudgeItem{
			A: p.a, ACarriers: []string{"A carrier"},
			B: p.b, BCarriers: []string{"B carrier"},
		})
	}
	return out
}

// A verdict about a pair this item never offered must be rejected. Otherwise a
// judge that invents a pair — or answers one from a previous item — merges two
// clusters nobody put in front of it.
func TestMotifAliasDecode_RejectsAnUnofferedPair(t *testing.T) {
	res := AliasResult{Verdicts: []motifAliasVerdict{
		{A: "invented-one", B: "invented-two", SameMechanism: true, Mechanism: "both are made up"},
	}}
	err := ValidateAliasVerdicts(res, offeredFixture())
	require.Error(t, err)
	require.Contains(t, err.Error(), "not offered")
}

func TestMotifAliasDecode_RejectsAMergeWithNoMechanism(t *testing.T) {
	res := AliasResult{Verdicts: []motifAliasVerdict{
		{A: "retry-storm", B: "thundering-herd", SameMechanism: true, Mechanism: "  "},
	}}
	err := ValidateAliasVerdicts(res, offeredFixture())
	require.Error(t, err)
	require.Contains(t, err.Error(), "names no shared mechanism")
}

func TestMotifAliasDecode_RejectsADuplicateVerdict(t *testing.T) {
	res := AliasResult{Verdicts: []motifAliasVerdict{
		{A: "retry-storm", B: "thundering-herd", SameMechanism: false},
		{A: "thundering-herd", B: "retry-storm", SameMechanism: true, Mechanism: "changed my mind"},
	}}
	err := ValidateAliasVerdicts(res, offeredFixture())
	require.Error(t, err)
	require.Contains(t, err.Error(), "more than once")
}

// A decline needs no mechanism, and the pair may be given in either order.
func TestMotifAliasDecode_AcceptsDeclinesInEitherOrder(t *testing.T) {
	res := AliasResult{Verdicts: []motifAliasVerdict{
		{A: "thundering-herd", B: "retry-storm", SameMechanism: false},
	}}
	require.NoError(t, ValidateAliasVerdicts(res, offeredFixture()))
}

// Invariant 51d85fcd: the schema's `required` list is INERT without a probe on
// the raw object. A response carrying its content under the wrong key
// unmarshals to a zero value, applies as a silent no-op, and the item advances
// with the work gone.
func TestMotifAliasDecode_WrongEnvelopeKeyIsLoud(t *testing.T) {
	_, err := ParseAliasResponse(`{"decisions": [{"a":"x","b":"y","same_mechanism":false}]}`)
	require.Error(t, err,
		"a response under the wrong key must fail loudly, not decode to an empty result "+
			"and apply as a no-op")

	_, err = ParseAliasResponse(`{"verdicts": []}`)
	require.NoError(t, err, "an explicitly empty verdict list is a legitimate 'nothing to do'")
}

// ── health ────────────────────────────────────────────────────────────────

// Both health recorders must APPEND. If either starts assigning, whichever runs
// last silently deletes the other's lines — and health is the only channel
// through which either mechanism reports "I ran and found nothing", so a broken
// subsystem then looks exactly like a clean corpus.
//
// TestHealthRecorders_NeverDestroyExistingLines below is the real guard. THIS
// test only proves the two compose in the order Plan happens to call them, and
// that is worth stating: when the restatement recorder still assigned, this test
// passed anyway, because the test called it first — the safe order. A test that
// guards an arrangement rather than a property survives its own subject
// regressing.
func TestMotifAliasHealth_CoexistsWithRestatementLines(t *testing.T) {
	env := motifVocabEnv(t, minJudgeVocabulary+4)
	sess, err := env.Svc.Pipeline().CreatePipelineSession(context.Background(), "review", env.Branch, "")
	require.NoError(t, err)

	// The production recorder, reached the way Plan reaches it: PlanShortlist
	// records health on every path through its deferred recorder.
	require.NoError(t, restatement.PlanShortlist(context.Background(), env.Deps(), sess, env.Branch, nil))
	before := len(sess.Health)
	require.NotZero(t, before, "the restatement lines must be there to be clobbered")

	require.NoError(t, PlanAliasWork(context.Background(), env.Deps(), sess, env.Branch))

	require.Greater(t, len(sess.Health), before,
		"the alias lines must be ADDED, not substituted for the shortlist's")
	joined := strings.Join(sess.Health, "\n")
	require.Contains(t, joined, "standing restatement pairs", "the restatement lines must survive")
	require.Contains(t, joined, "motif aliases", "the alias lines must be present")
}

// A corpus below the floor must SAY so. "Found nothing" and "did not run" have
// to stay distinguishable, which is the whole reason these descriptors exist.
func TestMotifAliasHealth_BelowFloorIsReported(t *testing.T) {
	env := motifVocabEnv(t, minJudgeVocabulary-1)
	sess, err := env.Svc.Pipeline().CreatePipelineSession(context.Background(), "review", env.Branch, "")
	require.NoError(t, err)

	require.NoError(t, PlanAliasWork(context.Background(), env.Deps(), sess, env.Branch))
	require.Contains(t, strings.Join(sess.Health, "\n"), "validity floor")
}

// ── the work item ─────────────────────────────────────────────────────────

// The payload must carry carrier titles for both sides. The prompt tells the
// judge to read them; an item that shipped without them would be asking it to
// decide on names, which is the pre-block's job and not the judge's.
func TestMotifAliasWorkItem_PayloadCarriesTitlesForBothSides(t *testing.T) {
	ctx := context.Background()
	env := motifVocabEnv(t, minJudgeVocabulary+4)
	sess, err := env.Svc.Pipeline().CreatePipelineSession(ctx, "review", env.Branch, "")
	require.NoError(t, err)
	require.NoError(t, PlanAliasWork(ctx, env.Deps(), sess, env.Branch))

	item, err := env.Svc.Pipeline().NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, item, "an above-floor corpus must enqueue an alias item")
	require.Equal(t, AliasStepType, item.StepType)

	var offered []JudgeItem
	require.NoError(t, json.Unmarshal([]byte(item.FactsJSON), &offered))
	require.NotEmpty(t, offered)
	for _, o := range offered {
		require.NotEmpty(t, o.ACarriers, "pair %s/%s shipped without carriers for a", o.A, o.B)
		require.NotEmpty(t, o.BCarriers, "pair %s/%s shipped without carriers for b", o.A, o.B)
	}
}

// ONE item per session, not one per pair: §3.1 specifies one bounded prompt,
// and a judge seeing several pairs together can notice that two of them are the
// same question asked twice.
func TestMotifAliasWorkItem_OneItemPerSession(t *testing.T) {
	ctx := context.Background()
	env := motifVocabEnv(t, minJudgeVocabulary+6)
	sess, err := env.Svc.Pipeline().CreatePipelineSession(ctx, "review", env.Branch, "")
	require.NoError(t, err)
	require.NoError(t, PlanAliasWork(ctx, env.Deps(), sess, env.Branch))

	n := 0
	for {
		item, err := env.Svc.Pipeline().NextPipelineWorkItem(ctx, sess.ID)
		require.NoError(t, err)
		if item == nil {
			break
		}
		if item.StepType == AliasStepType {
			n++
		}
		_, err = env.Svc.Pipeline().AnswerPipelineWorkItem(ctx, item.ID, "{}")
		require.NoError(t, err)
	}
	require.Equal(t, 1, n)
}

// TestHealthRecorders_NeverDestroyExistingLines is the property test the
// co-existence test above is not.
//
// It seeds sess.Health with a line neither recorder produces, then calls the
// motif-alias recorder (the restatement recorder's own subcase lives in the
// restatement package, beside the unexported recorder it calls), and requires the seed to survive. That holds regardless of call order,
// of how many producers exist, and of which one runs first — which is what
// makes it a guard on the RULE ("a health recorder appends") rather than on
// today's arrangement of callers.
//
// The distinction is not academic. This file's first version tested only the
// arrangement, and it passed with the restatement recorder still assigning.
func TestHealthRecorders_NeverDestroyExistingLines(t *testing.T) {
	const seed = "seeded by another producer"

	for name, record := range map[string]func(*model.PipelineSession){
		"motif-alias": func(s *model.PipelineSession) {
			recordMotifAliasHealth(s, motifAliasHealth{Vocabulary: 20, Emitted: 2})
		},
	} {
		t.Run(name, func(t *testing.T) {
			sess := &model.PipelineSession{Health: []string{seed}}
			record(sess)
			require.Contains(t, sess.Health, seed,
				"%s destroyed a line it did not write — health recorders must append, "+
					"or the last one to run silences every other subsystem's only report channel",
				name)
			require.Greater(t, len(sess.Health), 1, "%s recorded nothing at all", name)
		})
	}
}
