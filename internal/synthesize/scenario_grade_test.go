package synthesize_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/mcp"
	"knomit/internal/synthesize"
)

// The graders, falsified both ways on the example world: a hand-written
// perfect run scores 1.0 on every metric, and a hand-written wrong run scores
// the exact value each metric's definition gives it. A run is the engine's
// result shape (ReasonChain mirrors repos.ReasonResult); its answer facts are
// written through knomit_learn, as the host writes them, and read back by
// path.

func exampleWorld(t *testing.T) (*synthesize.LoadedWorld, *synthesize.ScenarioQuestion, *synthesize.ScenarioQuestion) {
	t.Helper()
	s, err := synthesize.ReadScenarioFile(exampleSpec)
	require.NoError(t, err)
	w, err := loadSpec(t, s, nil)
	require.NoError(t, err)
	return w, &s.Questions[0], &s.Questions[1]
}

// writeAnswer writes one answer fact through knomit_learn on the world branch
// and returns the path the server minted.
func writeAnswer(t *testing.T, w *synthesize.LoadedWorld, f map[string]any) string {
	t.Helper()
	text, isErr, err := mcp.NewScriptTools(nil).Call(w.Ctx(context.Background()), "learn",
		map[string]any{"moment_name": "engine answer", "facts": []any{f}})
	require.NoError(t, err)
	require.False(t, isErr, text)
	var res struct {
		Commits []struct {
			File string `json:"file"`
		} `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &res))
	require.Len(t, res.Commits, 1)
	return res.Commits[0].File
}

func paths(w *synthesize.LoadedWorld, ids ...string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, w.Paths[id])
	}
	return out
}

func labels(w *synthesize.LoadedWorld, kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[w.Paths[kv[i]]] = kv[i+1]
	}
	return m
}

func q1Answer(w *synthesize.LoadedWorld, category string) map[string]any {
	return map[string]any{
		"topic": "practice", "category": category, "type": "synthesis",
		"title":      "Velmora pears keep eleven weeks in the Hollin cellar because of its 92% humidity, not its cold",
		"body":       "The Hollin cellar holds 92% relative humidity; the Brask shed trial at the same 4 °C but 70% humidity shrivelled the same pears within four weeks, so humidity, not temperature, sets the limit.",
		"confidence": 0.85, "sources": 1,
		"domain": []string{"storage", "pears"}, "entities": []string{"Velmora pear", "Hollin cellar"},
		"refs": paths(w, "f03", "f01", "f02", "f05", "f08"),
	}
}

func perfectQ1(w *synthesize.LoadedWorld, answer string) synthesize.ReasonChain {
	return synthesize.ReasonChain{
		Run: "reason-0000000a", Outcome: "answer", Question: "Why do Velmora pears keep?",
		Repo: w.RepoID, Branch: w.Branch, AnswerFacts: []string{answer}, Posterior: 0.8,
		Chain: []synthesize.ReasonStep{
			{Step: 0, State: "GATHER", Move: "neighbours", Arg: "question", Facts: paths(w, "f03", "f01", "f04"),
				Judgment: "relevant", Support: labels(w, "f03", "yes", "f01", "yes", "f04", "no")},
			{Step: 1, State: "WIDEN", Move: "refs_out", From: w.Paths["f03"], Facts: paths(w, "f01", "f02", "f05"),
				Judgment: "support", Support: labels(w, "f01", "supports", "f02", "supports", "f05", "supports")},
			{Step: 2, State: "WIDEN", Move: "history", From: w.Paths["f03"], Facts: paths(w, "f03")},
			{Step: 3, State: "WIDEN", Move: "kind_type", Arg: "methodology", Facts: paths(w, "f08")},
			{Step: 4, State: "ASSESS", Move: "judge", Arg: "5 facts", Judgment: "assess", Verdict: "answers"},
		},
	}
}

func q2Hypothesis(w *synthesize.LoadedWorld) map[string]any {
	return map[string]any{
		"topic": "practice", "category": "storage", "type": "hypothesis",
		"title":      "Corrigan plums kept in the Hollin cellar would stay sound for several weeks",
		"body":       "Unsettled: nobody has logged Corrigan plums in the Hollin cellar. Settled by a cellar log entry recording the week losses began.",
		"confidence": 0.3, "sources": 1,
		"domain": []string{"storage", "plums"}, "entities": []string{"Corrigan plum", "Hollin cellar"},
		"refs": paths(w, "f02"),
	}
}

func perfectQ2(w *synthesize.LoadedWorld, hypothesis string) synthesize.ReasonChain {
	return synthesize.ReasonChain{
		Run: "reason-0000000b", Outcome: "gap", Branch: w.Branch, AnswerFacts: []string{hypothesis}, Posterior: 0.9,
		Chain: []synthesize.ReasonStep{
			{State: "GATHER", Move: "neighbours", Arg: "question", Facts: paths(w, "f09"),
				Judgment: "relevant", Support: labels(w, "f09", "no")},
			{State: "WIDEN", Move: "entity", Arg: "Hollin cellar", Facts: paths(w, "f01", "f02", "f03"),
				Judgment: "relevant", Support: labels(w, "f02", "yes", "f01", "no")},
		},
		Gaps: []synthesize.ChainGap{{
			Question: "How long do Corrigan plums keep?", Posterior: 0.2,
			Missing:     "No record says Corrigan plums were ever kept in the Hollin cellar.",
			BestPartial: "The cellar's conditions are logged (4 °C, 92% humidity).",
		}},
	}
}

func grade(t *testing.T, w *synthesize.LoadedWorld, q *synthesize.ScenarioQuestion, c synthesize.ReasonChain) synthesize.ChainScore {
	t.Helper()
	s, err := synthesize.GradeChain(context.Background(), w, q, c)
	require.NoError(t, err)
	return s
}

// TestGradeChain_DecodesTheEngineResult: a ReasonResult as the engine emits
// it decodes into ReasonChain with nothing lost. Sabotage: rename a tag.
func TestGradeChain_DecodesTheEngineResult(t *testing.T) {
	raw := `{"reason_run":"reason-1a2b3c4d","outcome":"gap","question":"q","repo":"r","branch":"agent/x",
"experiment":"e","experiment_branch":"exp/e","answer_facts":["kb/a/b/1.md"],"existing_answer":"kb/a/b/0.md",
"posterior":0.4,"chain":[{"step":3,"state":"ASSESS","move":"judge","from":"kb/a/b/2.md","arg":"2 facts",
"facts":["kb/a/b/2.md"],"judgment":"support","verdict":"1 supports","support":{"kb/a/b/2.md":"supports"},"posterior":0.5}],
"gaps":[{"question":"q","missing":"m","best_partial":"p","posterior":0.1,"tried":["neighbours"],"contradictions":["c"],"reason":"budget"}],
"unwritten_facts":[{"x":1}],"model_calls":7,"elapsed_ms":1200,"error":"","next":"commit"}`
	var c synthesize.ReasonChain
	require.NoError(t, json.Unmarshal([]byte(raw), &c))
	back, err := json.Marshal(c)
	require.NoError(t, err)
	var a, b map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &a))
	require.NoError(t, json.Unmarshal(back, &b))
	delete(a, "error") // omitempty and empty
	require.Equal(t, a, b)
	require.Equal(t, 0.5, *c.Chain[0].Posterior)
	require.Equal(t, "supports", c.Chain[0].Support["kb/a/b/2.md"])
	require.Equal(t, "m", c.Gaps[0].Missing)
}

// TestGradeChain_PerfectScoresOne: every metric is at its best value.
// Sabotage: make moveMatches compare From by raw path instead of mapping to
// the spec id, or ratio return 0 for 0/0; this test fails.
func TestGradeChain_PerfectScoresOne(t *testing.T) {
	w, q1, q2 := exampleWorld(t)

	s := grade(t, w, q1, perfectQ1(w, writeAnswer(t, w, q1Answer(w, "storage"))))
	require.Equal(t, 1.0, s.MoveAccuracy)
	require.Equal(t, 1.0, s.MoveRecall)
	require.Equal(t, 0, s.ExtraMoves)
	require.Equal(t, 1.0, s.RelevancePrecision)
	require.Equal(t, 1.0, s.RelevanceRecall)
	require.Equal(t, 1.0, s.SupportAccuracy)
	require.Equal(t, 0.0, s.Misattribution)
	require.True(t, s.OutcomeCorrect)
	require.Equal(t, 1.0, s.ChainRecall)
	require.Equal(t, 1.0, s.ChainPrecision)
	require.Empty(t, s.ForbiddenCited)
	require.Equal(t, 1.0, s.AnswerValidity, "%v", s.AnswerFailures)
	require.Equal(t, 1.0, s.RefCorrectness, "%v", s.BadRefs)
	require.False(t, s.GapDeclared)
	require.True(t, s.Correct)
	require.InDelta(t, 0.04, s.Brier, 1e-9)

	// A descendant of the gold category is the same place.
	sub := grade(t, w, q1, perfectQ1(w, writeAnswer(t, w, q1Answer(w, "storage/cold"))))
	require.Equal(t, 1.0, sub.AnswerValidity, "%v", sub.AnswerFailures)

	g := grade(t, w, q2, perfectQ2(w, writeAnswer(t, w, q2Hypothesis(w))))
	require.Equal(t, 1.0, g.MoveAccuracy)
	require.Equal(t, 1.0, g.MoveRecall)
	require.Equal(t, 1.0, g.RelevancePrecision)
	require.Equal(t, 1.0, g.RelevanceRecall)
	require.True(t, g.OutcomeCorrect)
	require.Equal(t, 1.0, g.ChainRecall)
	require.Equal(t, 1.0, g.ChainPrecision, "partial support is neither support nor a miss")
	require.Equal(t, 1.0, g.AnswerValidity, "%v", g.AnswerFailures)
	require.Equal(t, 1.0, g.RefCorrectness, "%v", g.BadRefs)
	require.True(t, g.GapExpected)
	require.True(t, g.GapDeclared)
	require.True(t, g.GapHypothesisValid)
	require.True(t, g.Correct)
	require.InDelta(t, 0.01, g.Brier, 1e-9)
}

// TestGradeChain_CategoryPrefixIsNotACategory: an answer filed under
// practice/storage-legacy is NOT in the gold practice/storage, although the
// string starts the same. Sabotage: compare with a raw HasPrefix.
func TestGradeChain_CategoryPrefixIsNotACategory(t *testing.T) {
	w, q1, _ := exampleWorld(t)
	s := grade(t, w, q1, perfectQ1(w, writeAnswer(t, w, q1Answer(w, "storage-legacy"))))
	require.InDelta(t, 12.0/13, s.AnswerValidity, 1e-9, "%v", s.AnswerFailures)
	require.Equal(t, []string{"topic: practice/storage-legacy"}, s.AnswerFailures)
	require.False(t, s.Correct)
}

// TestGradeChain_WrongChainScoresBelow: a run that follows the distractor,
// cites a hypothesis and an unlisted source, and emits a mistyped answer gets
// the exact value each definition gives, on every metric.
func TestGradeChain_WrongChainScoresBelow(t *testing.T) {
	w, q1, q2 := exampleWorld(t)

	answer := writeAnswer(t, w, map[string]any{
		"topic": "practice", "category": "storage", "type": "observation",
		"title": "Velmora pears keep eleven weeks because they are graded by stem length", "body": "Grading explains storage.",
		"confidence": 0.97, "sources": 1, "domain": []string{"storage"}, "entities": []string{"Velmora pear"},
		"refs": append(paths(w, "f04", "f01", "f07"), "https://elsewhere.invalid/x"),
	})
	wrong := synthesize.ReasonChain{
		Outcome: "answer", Branch: w.Branch, AnswerFacts: []string{answer}, Posterior: 0.9,
		Chain: []synthesize.ReasonStep{
			{Move: "neighbours", Arg: "question", Facts: paths(w, "f03", "f01", "f04"),
				Judgment: "relevant", Support: labels(w, "f03", "yes", "f04", "yes", "f01", "no")},
			{Move: "entity", Arg: "Orrin packhouse", Facts: paths(w, "f04")},
			{Move: "motif", Arg: "one-condition-sets-limit", Facts: paths(w, "f01", "f03", "f05"),
				Judgment: "support", Support: labels(w, "f01", "supports", "f04", "supports", "f02", "unrelated")},
		},
	}
	s := grade(t, w, q1, wrong)
	require.InDelta(t, 1.0/3, s.MoveAccuracy, 1e-9, "1 of 3 run moves is on the gold path")
	require.Equal(t, 0.5, s.MoveRecall, "1 of 2 required gold steps")
	require.Equal(t, 2, s.ExtraMoves)
	require.Equal(t, 0.5, s.RelevancePrecision, "yes on f03 (right) and f04 (distractor)")
	require.Equal(t, 0.5, s.RelevanceRecall, "f01 was judged irrelevant")
	require.InDelta(t, 1.0/3, s.SupportAccuracy, 1e-9, "f01 right, f02 wrong, f05 never judged")
	require.Equal(t, 0.5, s.Misattribution, "half the supports labels went to the distractor")
	require.True(t, s.OutcomeCorrect, "an answer was the right outcome; its content is what is wrong")
	require.Equal(t, 0.25, s.ChainRecall, "only f01 of {f03,f01,f02,f05}")
	require.InDelta(t, 1.0/3, s.ChainPrecision, 1e-9, "1 support among 3 cited world facts")
	require.Equal(t, []string{"f04", "f07"}, s.ForbiddenCited)
	require.InDelta(t, 7.0/13, s.AnswerValidity, 1e-9, "%v", s.AnswerFailures)
	require.Len(t, s.AnswerFailures, 6)
	require.Equal(t, 0.75, s.RefCorrectness, "the unlisted URL does not resolve in the world")
	require.Equal(t, []string{"https://elsewhere.invalid/x"}, s.BadRefs)
	require.False(t, s.Correct)
	require.InDelta(t, 0.81, s.Brier, 1e-9)

	// A ref to a fact that did not exist at the head the run started from
	// does not resolve, even though it exists now.
	late := writeAnswer(t, w, map[string]any{
		"topic": "practice", "category": "grading", "type": "observation",
		"title": "Orrin packhouse grades plums by colour", "body": "Colour sorting at Orrin.",
		"confidence": 0.6, "sources": 1, "domain": []string{"grading"}, "entities": []string{"Orrin packhouse"},
		"refs": []string{answer},
	})
	l := grade(t, w, q1, synthesize.ReasonChain{Outcome: "answer", Branch: w.Branch, AnswerFacts: []string{late}})
	require.Equal(t, 0.0, l.RefCorrectness)

	// q2 answered instead of a gap: the gap metrics catch it.
	ans2 := writeAnswer(t, w, map[string]any{
		"topic": "practice", "category": "storage", "type": "synthesis",
		"title": "Corrigan plums keep eleven weeks in the Hollin cellar", "body": "Like the pears.",
		"confidence": 0.7, "sources": 1, "domain": []string{"storage"}, "entities": []string{"Corrigan plum"},
		"refs": paths(w, "f01"),
	})
	answered := synthesize.ReasonChain{
		Outcome: "answer", Branch: w.Branch, AnswerFacts: []string{ans2}, Posterior: 0.7,
		Chain: []synthesize.ReasonStep{{Move: "entity", Arg: "Velmora pear", Facts: paths(w, "f01")}},
	}
	g := grade(t, w, q2, answered)
	require.Equal(t, 0.0, g.MoveAccuracy)
	require.Equal(t, 0.0, g.MoveRecall)
	require.Equal(t, 0.0, g.RelevanceRecall)
	require.False(t, g.OutcomeCorrect)
	require.Equal(t, 0.0, g.ChainPrecision, "f01 is not support for q2")
	require.Equal(t, []string{"f01"}, g.ForbiddenCited)
	require.Equal(t, 0.0, g.AnswerValidity, "%v", g.AnswerFailures)
	require.False(t, g.GapDeclared)
	require.False(t, g.GapHypothesisValid)
	require.False(t, g.Correct)

	// Aggregates.
	hyp := writeAnswer(t, w, q2Hypothesis(w))
	perfect1 := grade(t, w, q1, perfectQ1(w, writeAnswer(t, w, q1Answer(w, "storage"))))
	perfect2 := grade(t, w, q2, perfectQ2(w, hyp))
	q1gap := perfectQ2(w, hyp)
	falseGap := grade(t, w, q1, q1gap)
	require.Equal(t, 0.0, falseGap.AnswerValidity)
	require.True(t, falseGap.GapDeclared)
	recall, fgr := synthesize.GapRates([]synthesize.ChainScore{perfect1, falseGap, perfect2, g})
	require.Equal(t, 0.5, recall)
	require.Equal(t, 0.5, fgr)
	require.InDelta(t, 0.55, synthesize.CalibrationECE([]synthesize.ChainScore{perfect1, s}, 10), 1e-9)
}
