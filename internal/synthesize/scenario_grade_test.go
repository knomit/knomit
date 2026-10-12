package synthesize_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/synthesize"
)

// The graders, falsified both ways on the example world: a hand-written
// perfect chain scores 1.0 on every metric, and a hand-written wrong chain
// scores the exact value each metric's definition gives it.

func exampleWorld(t *testing.T) (*synthesize.LoadedWorld, *synthesize.ScenarioQuestion, *synthesize.ScenarioQuestion) {
	t.Helper()
	s, err := synthesize.ReadScenarioFile(exampleSpec)
	require.NoError(t, err)
	w, err := loadSpec(t, s, nil)
	require.NoError(t, err)
	return w, &s.Questions[0], &s.Questions[1]
}

func at(w *synthesize.LoadedWorld, ids ...string) []synthesize.FactAt {
	var out []synthesize.FactAt
	for _, id := range ids {
		out = append(out, synthesize.FactAt{Path: w.Paths[id], Commit: w.Head})
	}
	return out
}

func paths(w *synthesize.LoadedWorld, ids ...string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, w.Paths[id])
	}
	return out
}

func verdicts(w *synthesize.LoadedWorld, kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[w.Paths[kv[i]]] = kv[i+1]
	}
	return m
}

func perfectQ1(w *synthesize.LoadedWorld) synthesize.ReasonChain {
	return synthesize.ReasonChain{
		Question: "q1", ReasonRun: "reason-0000000a",
		World: synthesize.ChainWorld{Repo: w.RepoID, Branch: w.Branch, Commit: w.Head},
		Steps: []synthesize.ReasonStep{
			{Index: 0, State: "GATHER", Move: "neighbours", Arg: "question", Facts: at(w, "f03", "f01", "f04"),
				Judgment: &synthesize.StepJudgment{Kind: "relevant", Verdicts: verdicts(w, "f03", "yes", "f01", "yes", "f04", "no")}},
			{Index: 1, State: "WIDEN", Move: "refs_out", From: w.Paths["f03"], Facts: at(w, "f01", "f02", "f05"),
				Judgment: &synthesize.StepJudgment{Kind: "support", Verdicts: verdicts(w, "f01", "supports", "f02", "supports", "f05", "supports")}},
			{Index: 2, State: "WIDEN", Move: "history", From: w.Paths["f03"], Facts: at(w, "f03")},
			{Index: 3, State: "WIDEN", Move: "kind_type", Arg: "methodology", Facts: at(w, "f08")},
			{Index: 4, State: "ASSESS", Judgment: &synthesize.StepJudgment{Kind: "assess", Value: "answers"}},
		},
		Assess: "answers", Posterior: 0.8,
		Answers: []synthesize.AnswerFact{{
			Topic: "practice", Category: "storage", Type: "synthesis",
			Title:      "Velmora pears keep eleven weeks in the Hollin cellar because of its 92% humidity, not its cold",
			Body:       "The Hollin cellar holds 92% relative humidity; the Brask shed trial at the same 4 °C but 70% humidity shrivelled the same pears within four weeks, so humidity, not temperature, sets the limit.",
			Confidence: 0.85, Sources: 1,
			Domain: []string{"storage", "pears"}, Entities: []string{"Velmora pear", "Hollin cellar"},
			Refs:    paths(w, "f03", "f01", "f02", "f05", "f08"),
			Context: map[string]any{"question": "q-velmora-hollin"},
		}},
	}
}

func perfectQ2(w *synthesize.LoadedWorld) synthesize.ReasonChain {
	return synthesize.ReasonChain{
		Question: "q2", World: synthesize.ChainWorld{Commit: w.Head},
		Steps: []synthesize.ReasonStep{
			{State: "GATHER", Move: "neighbours", Arg: "question", Facts: at(w, "f09"),
				Judgment: &synthesize.StepJudgment{Kind: "relevant", Verdicts: verdicts(w, "f09", "no")}},
			{State: "WIDEN", Move: "entity", Arg: "Hollin cellar", Facts: at(w, "f01", "f02", "f03"),
				Judgment: &synthesize.StepJudgment{Kind: "relevant", Verdicts: verdicts(w, "f02", "yes", "f01", "no")}},
			{State: "ASSESS", Judgment: &synthesize.StepJudgment{Kind: "assess", Value: "not"}},
		},
		Assess: "not", Posterior: 0.9,
		Gap: &synthesize.ChainGap{
			MissingPremise: "No record says Corrigan plums were ever kept in the Hollin cellar.",
			Settlement:     "A cellar log entry for Corrigan plums in the Hollin cellar with the week losses began.",
			PartialSupport: paths(w, "f02"),
			Hypothesis: &synthesize.AnswerFact{
				Topic: "practice", Category: "storage", Type: "hypothesis",
				Title:      "Corrigan plums kept in the Hollin cellar would stay sound for several weeks",
				Body:       "Unsettled: nobody has logged Corrigan plums in the Hollin cellar. Settled by a cellar log entry recording the week losses began.",
				Confidence: 0.3, Sources: 1,
				Domain: []string{"storage", "plums"}, Entities: []string{"Corrigan plum", "Hollin cellar"},
				Refs: paths(w, "f02"),
			},
		},
	}
}

// TestGradeChain_PerfectScoresOne: every metric is at its best value.
// Sabotage: make moveMatches compare From by raw path instead of mapping to
// the spec id, or ratio return 0 for 0/0; this test fails.
func TestGradeChain_PerfectScoresOne(t *testing.T) {
	w, q1, q2 := exampleWorld(t)
	ctx := context.Background()

	s := synthesize.GradeChain(ctx, w, q1, perfectQ1(w))
	require.Equal(t, 1.0, s.MoveAccuracy)
	require.Equal(t, 1.0, s.MoveRecall)
	require.Equal(t, 0, s.ExtraMoves)
	require.Equal(t, 1.0, s.RelevancePrecision)
	require.Equal(t, 1.0, s.RelevanceRecall)
	require.Equal(t, 1.0, s.SupportAccuracy)
	require.Equal(t, 0.0, s.Misattribution)
	require.True(t, s.AssessCorrect)
	require.Equal(t, 1.0, s.ChainRecall)
	require.Equal(t, 1.0, s.ChainPrecision)
	require.Empty(t, s.ForbiddenCited)
	require.Equal(t, 1.0, s.AnswerValidity, "%v", s.AnswerFailures)
	require.Equal(t, 1.0, s.RefCorrectness, "%v", s.BadRefs)
	require.False(t, s.GapDeclared)
	require.True(t, s.Correct)
	require.InDelta(t, 0.04, s.Brier, 1e-9)

	g := synthesize.GradeChain(ctx, w, q2, perfectQ2(w))
	require.Equal(t, 1.0, g.MoveAccuracy)
	require.Equal(t, 1.0, g.MoveRecall)
	require.Equal(t, 1.0, g.RelevancePrecision)
	require.Equal(t, 1.0, g.RelevanceRecall)
	require.True(t, g.AssessCorrect)
	require.Equal(t, 1.0, g.ChainRecall)
	require.Equal(t, 1.0, g.ChainPrecision, "partial support is neither support nor a miss")
	require.Equal(t, 1.0, g.AnswerValidity, "%v", g.AnswerFailures)
	require.Equal(t, 1.0, g.RefCorrectness, "%v", g.BadRefs)
	require.True(t, g.GapExpected)
	require.True(t, g.GapDeclared)
	require.True(t, g.GapHypothesisValid)
	require.True(t, g.Correct)
}

// TestGradeChain_WrongChainScoresBelow: a chain that follows the distractor,
// cites a hypothesis and a retracted fact, and emits a mistyped answer gets
// the exact value each definition gives, on every metric.
func TestGradeChain_WrongChainScoresBelow(t *testing.T) {
	w, q1, q2 := exampleWorld(t)
	ctx := context.Background()

	wrong := synthesize.ReasonChain{
		Question: "q1", World: synthesize.ChainWorld{Commit: w.Head},
		Steps: []synthesize.ReasonStep{
			{Move: "neighbours", Arg: "question", Facts: at(w, "f03", "f01", "f04"),
				Judgment: &synthesize.StepJudgment{Kind: "relevant", Verdicts: verdicts(w, "f03", "yes", "f04", "yes", "f01", "no")}},
			{Move: "entity", Arg: "Orrin packhouse", Facts: at(w, "f04")},
			{Move: "motif", Arg: "one-condition-sets-limit", Facts: at(w, "f01", "f03", "f05"),
				Judgment: &synthesize.StepJudgment{Kind: "support", Verdicts: verdicts(w, "f01", "supports", "f04", "supports", "f02", "unrelated")}},
		},
		Assess: "partly", Posterior: 0.9,
		Answers: []synthesize.AnswerFact{{
			Topic: "practice", Category: "storage", Type: "observation",
			Title: "Velmora pears keep eleven weeks because they are graded by stem length", Body: "Grading explains storage.",
			Confidence: 0.97, Sources: 1, Domain: []string{"storage"}, Entities: []string{"Velmora pear"},
			Refs: append(paths(w, "f04", "f01", "f07", "f10"), "https://elsewhere.invalid/x"),
		}},
	}
	s := synthesize.GradeChain(ctx, w, q1, wrong)
	require.InDelta(t, 1.0/3, s.MoveAccuracy, 1e-9, "1 of 3 run moves is on the gold path")
	require.Equal(t, 0.5, s.MoveRecall, "1 of 2 required gold steps")
	require.Equal(t, 2, s.ExtraMoves)
	require.Equal(t, 0.5, s.RelevancePrecision, "yes on f03 (right) and f04 (distractor)")
	require.Equal(t, 0.5, s.RelevanceRecall, "f01 was judged irrelevant")
	require.InDelta(t, 1.0/3, s.SupportAccuracy, 1e-9, "f01 right, f02 wrong, f05 never judged")
	require.Equal(t, 0.5, s.Misattribution, "half the supports verdicts went to the distractor")
	require.False(t, s.AssessCorrect)
	require.Equal(t, 0.25, s.ChainRecall, "only f01 of {f03,f01,f02,f05}")
	require.Equal(t, 0.25, s.ChainPrecision, "1 support among 4 cited world facts")
	require.Equal(t, []string{"f04", "f07", "f10"}, s.ForbiddenCited)
	require.InDelta(t, 7.0/13, s.AnswerValidity, 1e-9, "%v", s.AnswerFailures)
	require.Len(t, s.AnswerFailures, 6)
	require.InDelta(t, 0.6, s.RefCorrectness, 1e-9, "retracted f10 and an unlisted URL do not resolve")
	require.Equal(t, []string{w.Paths["f10"], "https://elsewhere.invalid/x"}, s.BadRefs)
	require.False(t, s.Correct)
	require.InDelta(t, 0.81, s.Brier, 1e-9)

	// q2 answered instead of a gap: the gap metrics catch it.
	answered := synthesize.ReasonChain{
		Question: "q2", World: synthesize.ChainWorld{Commit: w.Head},
		Steps: []synthesize.ReasonStep{{Move: "entity", Arg: "Velmora pear", Facts: at(w, "f01")}},
		Assess: "answers", Posterior: 0.7,
		Answers: []synthesize.AnswerFact{{
			Topic: "practice", Category: "storage", Type: "synthesis",
			Title: "Corrigan plums keep eleven weeks in the Hollin cellar", Body: "Like the pears.",
			Confidence: 0.7, Sources: 1, Domain: []string{"storage"}, Entities: []string{"Corrigan plum"},
			Refs: paths(w, "f01"),
		}},
	}
	g := synthesize.GradeChain(ctx, w, q2, answered)
	require.Equal(t, 0.0, g.MoveAccuracy)
	require.Equal(t, 0.0, g.MoveRecall)
	require.Equal(t, 0.0, g.RelevanceRecall)
	require.False(t, g.AssessCorrect)
	require.Equal(t, 0.0, g.ChainPrecision, "f01 is not support for q2")
	require.Equal(t, []string{"f01"}, g.ForbiddenCited)
	require.Equal(t, 0.0, g.AnswerValidity, "%v", g.AnswerFailures)
	require.False(t, g.GapDeclared)
	require.False(t, g.GapHypothesisValid)
	require.False(t, g.Correct)

	// Aggregates.
	q1gap := perfectQ1(w)
	q1gap.Answers, q1gap.Gap = nil, perfectQ2(w).Gap
	perfect1 := synthesize.GradeChain(ctx, w, q1, perfectQ1(w))
	perfect2 := synthesize.GradeChain(ctx, w, q2, perfectQ2(w))
	falseGap := synthesize.GradeChain(ctx, w, q1, q1gap)
	require.Equal(t, 0.0, falseGap.AnswerValidity)
	recall, fgr := synthesize.GapRates([]synthesize.ChainScore{perfect1, falseGap, perfect2, g})
	require.Equal(t, 0.5, recall)
	require.Equal(t, 0.5, fgr)
	require.InDelta(t, 0.55, synthesize.CalibrationECE([]synthesize.ChainScore{perfect1, s}, 10), 1e-9)
}
