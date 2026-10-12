package synthesize

// Graders for scenario worlds (expert spike, E05 §1-§2): they score one
// engine run (ReasonChain, the decoded repos.ReasonResult) against one
// question's gold, in the loaded world, with knomit's own validators
// (fact.ValidateFact with the world's ontology, fact.SerializeFact,
// fact.ClassifyRef) and the world's store. The answer facts are read from the
// store at the paths the host wrote. Mechanical only: the judge-graded
// metrics (claim entailment, synthesis support rate, hypothesis premise
// match) are not here.
//
// Every ratio is n/d with 0/0 = 1: nothing expected and nothing wrong is a
// perfect score, so a gap question is not punished for citing nothing.

import (
	"context"
	"fmt"
	"math"
	"path"
	"slices"
	"sort"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// ChainScore is one chain's grade.
type ChainScore struct {
	Question string

	// MoveAccuracy: run moves that match a gold step (in the gold's partial
	// order) over all run moves (E05 "move accuracy").
	MoveAccuracy float64
	// MoveRecall: required (non-optional) gold steps matched over required
	// gold steps.
	MoveRecall float64
	// ExtraMoves: run moves that matched no gold step (E05 "regret").
	ExtraMoves int

	// RelevancePrecision / RelevanceRecall of the run's `relevant` labels
	// against the gold relevant labels (E05 `relevant`).
	RelevancePrecision float64
	RelevanceRecall    float64
	// SupportAccuracy: gold support labels the run reproduced exactly.
	SupportAccuracy float64
	// Misattribution: share of the run's `supports` labels given to a fact
	// the question forbids or the world plants as a distractor.
	Misattribution float64
	// OutcomeCorrect: the run's outcome matches the gold one (an `answer` run
	// matches gold answer and partly; `gap` matches gap).
	OutcomeCorrect bool

	// ChainRecall: gold support facts cited by the answer fact(s) over the
	// gold support set. ChainPrecision: cited world facts that are support
	// (should-cite and partial-support facts are neutral) over cited world
	// facts.
	ChainRecall    float64
	ChainPrecision float64
	// ForbiddenCited are forbidden fact ids the answer cites.
	ForbiddenCited []string

	// AnswerValidity: checks passed over checks run on the primary answer
	// fact (or, for a gap, the hypothesis fact); AnswerFailures names each.
	AnswerValidity float64
	AnswerFailures []string

	// RefCorrectness: refs of the emitted facts that resolve in the world at
	// the head the run started from (a kb:// ref carries no commit) over all
	// refs; BadRefs names each failure.
	RefCorrectness float64
	BadRefs        []string

	// GapExpected is the gold outcome gap; GapDeclared is the run declaring
	// one; GapHypothesisValid is its hypothesis fact passing every check.
	GapExpected        bool
	GapDeclared        bool
	GapHypothesisValid bool

	// Correct is the chain-level verdict the calibration placeholder uses.
	Correct   bool
	Posterior float64
	// Brier is (Posterior - Correct)^2.
	Brier float64
}

// emittedFact is an answer fact read back from the store.
type emittedFact struct {
	path  string
	topic string // the path's topic/category walk, for ValidateFact
	f     fact.Fact
}

// readEmitted reads the facts the run wrote (AnswerFacts, on the experiment
// branch the run opened, else the world branch) and the existing fact it
// returned instead of writing (ExistingAnswer, on the world branch).
func readEmitted(ctx context.Context, w *LoadedWorld, chain ReasonChain) ([]emittedFact, error) {
	read := func(branch, p string) (emittedFact, error) {
		res, err := w.Svc.Facts().ReadFact(ctx, branch, p, nil)
		if err != nil {
			return emittedFact{}, fmt.Errorf("read answer fact %s on %s: %w", p, branch, err)
		}
		f, err := fact.ParseFact(p, res.Content)
		if err != nil {
			return emittedFact{}, fmt.Errorf("parse answer fact %s: %w", p, err)
		}
		rel := strings.TrimPrefix(strings.ToLower(p), "kb/")
		return emittedFact{path: p, topic: path.Dir(rel), f: f}, nil
	}
	branch := chain.ExperimentBranch
	if branch == "" {
		branch = w.Branch
	}
	var out []emittedFact
	for _, p := range chain.AnswerFacts {
		e, err := read(branch, p)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if chain.ExistingAnswer != "" {
		e, err := read(w.Branch, chain.ExistingAnswer)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// GradeChain scores chain against question q of world w.
func GradeChain(ctx context.Context, w *LoadedWorld, q *ScenarioQuestion, chain ReasonChain) (ChainScore, error) {
	sc := ChainScore{Question: q.ID, Posterior: chain.Posterior, GapExpected: q.Outcome == "gap",
		GapDeclared: chain.Outcome == "gap" || len(chain.Gaps) > 0}
	emitted, err := readEmitted(ctx, w, chain)
	if err != nil {
		return sc, err
	}
	var answers, hyps []emittedFact
	for _, e := range emitted {
		if e.f.Type == fact.Hypothesis {
			hyps = append(hyps, e)
		} else {
			answers = append(answers, e)
		}
	}
	gradeMoves(w, q, chain, &sc)
	gradeJudgments(w, q, chain, &sc)
	switch q.Outcome {
	case "gap":
		sc.OutcomeCorrect = chain.Outcome == "gap"
	default:
		sc.OutcomeCorrect = chain.Outcome == "answer"
	}
	gradeCitations(w, q, emitted, &sc)
	gradeValidity(w, q, chain, answers, hyps, &sc)
	gradeRefs(ctx, w, emitted, &sc)

	if sc.GapExpected {
		sc.Correct = sc.GapDeclared && sc.OutcomeCorrect && len(answers) == 0 && sc.GapHypothesisValid
	} else {
		sc.Correct = len(answers) > 0 && sc.OutcomeCorrect && sc.ChainRecall == 1 &&
			len(sc.ForbiddenCited) == 0 && sc.AnswerValidity == 1 && sc.RefCorrectness == 1
	}
	c := 0.0
	if sc.Correct {
		c = 1
	}
	sc.Brier = (chain.Posterior - c) * (chain.Posterior - c)
	return sc, nil
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 1
	}
	return float64(n) / float64(d)
}

// gradeMoves matches run moves to gold steps greedily in run order: a run
// move matches the first unmatched gold step whose requirements are all
// matched and one of whose alternatives it equals.
func gradeMoves(w *LoadedWorld, q *ScenarioQuestion, chain ReasonChain, sc *ChainScore) {
	matched := map[string]bool{}
	runMoves, hits := 0, 0
	for _, st := range chain.Chain {
		if st.Move == "" || !ScenarioMoves[st.Move] {
			continue // a judgment-only step (e.g. the engine's "judge")
		}
		runMoves++
		for _, g := range q.Chain {
			if matched[g.ID] || !allMatched(g.Requires, matched) {
				continue
			}
			if moveMatches(w, st, g.Moves) {
				matched[g.ID] = true
				hits++
				break
			}
		}
	}
	required, reqHit := 0, 0
	for _, g := range q.Chain {
		if g.Optional {
			continue
		}
		required++
		if matched[g.ID] {
			reqHit++
		}
	}
	sc.MoveAccuracy = ratio(hits, runMoves)
	if runMoves == 0 {
		sc.MoveAccuracy = 0
	}
	sc.MoveRecall = ratio(reqHit, required)
	sc.ExtraMoves = runMoves - hits
}

func allMatched(ids []string, m map[string]bool) bool {
	for _, id := range ids {
		if !m[id] {
			return false
		}
	}
	return true
}

func moveMatches(w *LoadedWorld, st ReasonStep, alts []GoldMove) bool {
	for _, a := range alts {
		if a.Move != st.Move {
			continue
		}
		switch {
		case a.From != "":
			if st.From != "" && w.IDOfPath(st.From) == a.From {
				return true
			}
		case a.Move == "ontology":
			if strings.Trim(strings.ToLower(st.Arg), "/") == strings.Trim(strings.ToLower(a.Arg), "/") {
				return true
			}
		default:
			if strings.EqualFold(strings.TrimSpace(st.Arg), strings.TrimSpace(a.Arg)) {
				return true
			}
		}
	}
	return false
}

func gradeJudgments(w *LoadedWorld, q *ScenarioQuestion, chain ReasonChain, sc *ChainScore) {
	goldRel, goldSup := map[string]string{}, map[string]string{}
	for _, g := range q.Chain {
		for id, v := range g.Relevant {
			goldRel[id] = v
		}
		for id, v := range g.Support {
			goldSup[id] = v
		}
	}
	runRel, runSup := map[string]string{}, map[string]string{}
	for _, st := range chain.Chain {
		var into map[string]string
		switch st.Judgment {
		case "relevant":
			into = runRel
		case "support":
			into = runSup
		default:
			continue
		}
		for p, v := range st.Support {
			id := w.IDOfPath(p)
			if id == "" {
				id = "path:" + p
			}
			into[id] = v // the last label on a fact stands
		}
	}
	tp, runYes, goldYes := 0, 0, 0
	for id, v := range goldRel {
		if v == "yes" {
			goldYes++
		}
		if runRel[id] == "yes" {
			runYes++
			if v == "yes" {
				tp++
			}
		}
	}
	sc.RelevancePrecision = ratio(tp, runYes)
	sc.RelevanceRecall = ratio(tp, goldYes)

	ok := 0
	for id, v := range goldSup {
		if runSup[id] == v {
			ok++
		}
	}
	sc.SupportAccuracy = ratio(ok, len(goldSup))

	forbidden := map[string]bool{}
	for _, id := range q.Forbidden {
		forbidden[id] = true
	}
	for _, f := range w.Spec.Facts {
		if f.Role == "distractor" {
			forbidden[f.ID] = true
		}
	}
	supports, mis := 0, 0
	for id, v := range runSup {
		if v != "supports" {
			continue
		}
		supports++
		if forbidden[id] {
			mis++
		}
	}
	sc.Misattribution = 0
	if supports > 0 {
		sc.Misattribution = float64(mis) / float64(supports)
	}
}

// citedIDs maps every local ref of the emitted facts to a world fact id.
func citedIDs(w *LoadedWorld, facts []emittedFact) map[string]bool {
	out := map[string]bool{}
	for _, e := range facts {
		for _, r := range e.f.Refs {
			c := fact.ClassifyRef(r, w.RepoID)
			if c.Kind != fact.RefLocalFact {
				continue
			}
			if id := w.IDOfPath(c.Path); id != "" {
				out[id] = true
			}
		}
	}
	return out
}

func gradeCitations(w *LoadedWorld, q *ScenarioQuestion, emitted []emittedFact, sc *ChainScore) {
	cited := citedIDs(w, emitted)
	support := map[string]bool{}
	for _, id := range q.Support {
		support[id] = true
	}
	neutral := map[string]bool{}
	if q.Answer != nil {
		for _, id := range q.Answer.ShouldCite {
			neutral[id] = true
		}
	}
	if q.Gap != nil {
		for _, id := range q.Gap.PartialSupport {
			neutral[id] = true
		}
	}
	hit := 0
	for id := range support {
		if cited[id] {
			hit++
		}
	}
	sc.ChainRecall = ratio(hit, len(support))
	good, counted := 0, 0
	for id := range cited {
		if neutral[id] && !support[id] {
			continue
		}
		counted++
		if support[id] {
			good++
		}
	}
	sc.ChainPrecision = ratio(good, counted)
	forbidden := append([]string{}, q.Forbidden...)
	if q.Answer != nil {
		forbidden = append(forbidden, q.Answer.MustNotCite...)
	}
	seen := map[string]bool{}
	for _, id := range forbidden {
		if cited[id] && !seen[id] {
			seen[id] = true
			sc.ForbiddenCited = append(sc.ForbiddenCited, id)
		}
	}
	sort.Strings(sc.ForbiddenCited)
}

// checkFact runs knomit's own checks on an emitted fact and returns the
// failures and the number of checks run.
func checkFact(w *LoadedWorld, e emittedFact) (fails []string, n int) {
	o := w.Spec.parsed
	check := func(name string, ok bool, detail string) {
		n++
		if !ok {
			fails = append(fails, name+": "+detail)
		}
	}
	perr := o.ValidatePath(e.topic)
	check("ontology_path", perr == nil, fmt.Sprint(perr))
	verr := fact.ValidateFact(o, e.topic, e.f)
	check("validate_fact", verr == nil, fmt.Sprint(verr))
	_, serr := fact.SerializeFact(e.f)
	check("serialize", serr == nil, fmt.Sprint(serr))
	kind := e.f.Kind
	if kind == "" {
		kind = fact.DefaultKind
	}
	check("kind_allows_type", kind.AllowsType(e.f.Type), string(kind)+"/"+string(e.f.Type))
	check("fields", strings.TrimSpace(e.f.Title) != "" && strings.TrimSpace(e.f.Body) != "" &&
		len(e.f.Domain) > 0 && len(e.f.Entities) > 0 && len(e.f.Refs) > 0,
		"title, body, domain, entities and refs must all be populated")
	return fails, n
}

// inCategory reports whether the topic walk got (topic/category...) sits at
// the gold topic and category: the same category, or a descendant of it. A
// category that merely starts with the same letters ("storage-legacy" for
// "storage") is a different category.
func inCategory(got, topic, category string) bool {
	want := strings.ToLower(strings.Trim(topic+"/"+category, "/"))
	got = strings.ToLower(strings.Trim(got, "/"))
	return got == want || strings.HasPrefix(got, want+"/")
}

func gradeValidity(w *LoadedWorld, q *ScenarioQuestion, chain ReasonChain, answers, hyps []emittedFact, sc *ChainScore) {
	var fails []string
	n := 0
	check := func(name string, ok bool, detail string) {
		n++
		if !ok {
			fails = append(fails, name+": "+detail)
		}
	}
	if q.Outcome == "gap" {
		check("gap_declared", sc.GapDeclared, "the gold outcome is a gap")
		check("no_answer", len(answers) == 0, "a gap question must not emit an answer")
		check("hypothesis_present", len(hyps) > 0, "a gap emits an evidence-free hypothesis fact")
		if len(hyps) > 0 {
			f, k := checkFact(w, hyps[0])
			fails, n = append(fails, f...), n+k
			missing := false
			for _, g := range chain.Gaps {
				if strings.TrimSpace(g.Missing) != "" {
					missing = true
				}
			}
			check("missing_premise", missing, "a gap names the missing premise")
		}
		sc.AnswerValidity = ratio(n-len(fails), n)
		sc.AnswerFailures = fails
		sc.GapHypothesisValid = len(hyps) > 0 && len(fails) == 0
		return
	}
	g := q.Answer
	if len(answers) == 0 {
		sc.AnswerValidity = 0
		sc.AnswerFailures = []string{"no_answer: the gold outcome is " + q.Outcome}
		return
	}
	a := answers[0]
	f, k := checkFact(w, a)
	fails, n = append(fails, f...), n+k
	check("type", slices.Contains(g.Types, string(a.f.Type)), fmt.Sprintf("%s not in %v", a.f.Type, g.Types))
	check("topic", inCategory(a.topic, g.Topic, g.Category), a.topic)
	if len(g.Confidence) == 2 {
		check("confidence", a.f.Confidence >= g.Confidence[0] && a.f.Confidence <= g.Confidence[1],
			fmt.Sprintf("%v outside %v", a.f.Confidence, g.Confidence))
	}
	cited := citedIDs(w, answers)
	for _, set := range []struct {
		name string
		ids  []string
		want bool
	}{{"must_cite", g.MustCite, true}, {"should_cite", g.ShouldCite, true}, {"must_not_cite", g.MustNotCite, false}} {
		var bad []string
		for _, id := range set.ids {
			if cited[id] != set.want {
				bad = append(bad, id)
			}
		}
		sort.Strings(bad)
		check(set.name, len(bad) == 0, strings.Join(bad, ","))
	}
	check("entities", containsAllFold(a.f.Entities, g.Entities), fmt.Sprintf("want %v", g.Entities))
	check("motifs", containsAllFold(a.f.Motifs, g.Motifs), fmt.Sprintf("want %v", g.Motifs))
	sc.AnswerValidity = ratio(n-len(fails), n)
	sc.AnswerFailures = fails
}

// gradeRefs judges every ref of the emitted facts against the world head the
// run started from (LoadedWorld.Head). A kb:// ref carries no commit, so
// there is no other commit to judge it at.
func gradeRefs(ctx context.Context, w *LoadedWorld, emitted []emittedFact, sc *ChainScore) {
	sources := map[string]bool{}
	for _, u := range w.Spec.ExternalSources {
		sources[u] = true
	}
	total, ok := 0, 0
	for _, e := range emitted {
		for _, r := range e.f.Refs {
			total++
			c := fact.ClassifyRef(r, w.RepoID)
			good := false
			switch c.Kind {
			case fact.RefLocalFact:
				live, err := w.Svc.Search().FactLiveAtCommit(ctx, w.Branch, c.Path, w.Head)
				good = err == nil && live
			case fact.RefLocalSystemFile:
				_, err := w.Svc.Facts().ReadFact(ctx, w.Branch, c.Path, &store.ReadFactOpts{AtCommit: w.Head})
				good = err == nil
			case fact.RefExternalURL:
				good = sources[r]
			}
			if good {
				ok++
			} else {
				sc.BadRefs = append(sc.BadRefs, r)
			}
		}
	}
	sc.RefCorrectness = ratio(ok, total)
}

func containsAllFold(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if strings.EqualFold(h, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// GapRates aggregates gap detection over many chains (E05 §2): the share of
// gap questions answered with a gap, and the share of answerable questions
// answered with a gap. Both directions, so neither "always answer" nor
// "always gap" scores. 0/0 = 1 for the recall, 0 for the false-gap rate.
func GapRates(scores []ChainScore) (gapRecall, falseGapRate float64) {
	gq, gd, aq, fg := 0, 0, 0, 0
	for _, s := range scores {
		if s.GapExpected {
			gq++
			if s.GapDeclared {
				gd++
			}
		} else {
			aq++
			if s.GapDeclared {
				fg++
			}
		}
	}
	gapRecall = ratio(gd, gq)
	if aq > 0 {
		falseGapRate = float64(fg) / float64(aq)
	}
	return gapRecall, falseGapRate
}

// CalibrationECE is the trust-calibration placeholder (E05 §2): expected
// calibration error of the chains' posteriors against their Correct verdict,
// over `bins` equal-width bins. The bar is set relative to the Opus arm
// (ruled 2026-10-12), never from a constant here.
func CalibrationECE(scores []ChainScore, bins int) float64 {
	if len(scores) == 0 || bins <= 0 {
		return 0
	}
	type acc struct{ n, correct, conf float64 }
	b := make([]acc, bins)
	for _, s := range scores {
		p := math.Min(math.Max(s.Posterior, 0), 1)
		i := int(p * float64(bins))
		if i == bins {
			i--
		}
		b[i].n++
		b[i].conf += p
		if s.Correct {
			b[i].correct++
		}
	}
	ece := 0.0
	for _, x := range b {
		if x.n == 0 {
			continue
		}
		ece += x.n / float64(len(scores)) * math.Abs(x.correct/x.n-x.conf/x.n)
	}
	return ece
}
