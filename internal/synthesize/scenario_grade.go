package synthesize

// Graders for scenario worlds (expert spike, E05 §1-§2): they score one
// ReasonChain against one question's gold, in the loaded world, with knomit's
// own validators (fact.ValidateFact with the world's ontology,
// fact.SerializeFact, fact.ClassifyRef) and the world's store for ref
// resolution. Mechanical only: the judge-graded metrics (claim entailment,
// synthesis support rate, hypothesis premise match) are not here.
//
// Every ratio is n/d with 0/0 = 1: nothing expected and nothing wrong is a
// perfect score, so a gap question is not punished for citing nothing.

import (
	"context"
	"fmt"
	"math"
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

	// RelevancePrecision / RelevanceRecall of the run's `relevant` verdicts
	// against the gold relevant labels (E05 `relevant`).
	RelevancePrecision float64
	RelevanceRecall    float64
	// SupportAccuracy: gold support labels the run reproduced exactly.
	SupportAccuracy float64
	// Misattribution: share of the run's `supports` verdicts given to a fact
	// the question forbids or the world plants as a distractor.
	Misattribution float64
	// AssessCorrect: the final ASSESS label equals the gold one.
	AssessCorrect bool

	// ChainRecall: gold support facts cited by the answer(s) over the gold
	// support set. ChainPrecision: cited world facts that are support (or
	// should-cite methodology/policy) over cited world facts.
	ChainRecall    float64
	ChainPrecision float64
	// ForbiddenCited are forbidden fact ids the answer cites.
	ForbiddenCited []string

	// AnswerValidity: checks passed over checks run on the primary answer
	// fact (or the gap's hypothesis fact); AnswerFailures names each failure.
	AnswerValidity float64
	AnswerFailures []string

	// RefCorrectness: refs that resolve in the world at the commit the chain
	// read them (else the chain's world commit, else the world head) over all
	// refs of the emitted facts; BadRefs names each failure.
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

// GradeChain scores chain against question q of world w.
func GradeChain(ctx context.Context, w *LoadedWorld, q *ScenarioQuestion, chain ReasonChain) ChainScore {
	sc := ChainScore{Question: q.ID, Posterior: chain.Posterior, GapExpected: q.Outcome == "gap", GapDeclared: chain.Gap != nil}
	gradeMoves(w, q, chain, &sc)
	gradeJudgments(w, q, chain, &sc)
	sc.AssessCorrect = chain.Assess == q.Assess

	// Facts the run emitted: the answers, or the gap's hypothesis.
	var emitted []AnswerFact
	emitted = append(emitted, chain.Answers...)
	if chain.Gap != nil && chain.Gap.Hypothesis != nil {
		emitted = append(emitted, *chain.Gap.Hypothesis)
	}
	gradeCitations(w, q, emitted, &sc)
	gradeValidity(w, q, chain, &sc)
	gradeRefs(ctx, w, chain, emitted, &sc)

	if sc.GapExpected {
		sc.Correct = sc.GapDeclared && len(chain.Answers) == 0 && sc.AssessCorrect && sc.GapHypothesisValid
	} else {
		sc.Correct = len(chain.Answers) > 0 && sc.AssessCorrect && sc.ChainRecall == 1 &&
			len(sc.ForbiddenCited) == 0 && sc.AnswerValidity == 1 && sc.RefCorrectness == 1
	}
	c := 0.0
	if sc.Correct {
		c = 1
	}
	sc.Brier = (chain.Posterior - c) * (chain.Posterior - c)
	return sc
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
	for _, st := range chain.Steps {
		if st.Move == "" {
			continue
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
	for _, st := range chain.Steps {
		if st.Judgment == nil {
			continue
		}
		var into map[string]string
		switch st.Judgment.Kind {
		case "relevant":
			into = runRel
		case "support":
			into = runSup
		default:
			continue
		}
		for p, v := range st.Judgment.Verdicts {
			id := w.IDOfPath(p)
			if id == "" {
				id = "path:" + p
			}
			into[id] = v // the last verdict on a fact stands
		}
	}
	// Relevance: precision over the run's "yes" on gold-labelled facts.
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
func citedIDs(w *LoadedWorld, facts []AnswerFact) map[string]bool {
	out := map[string]bool{}
	for _, a := range facts {
		for _, r := range a.Refs {
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

func gradeCitations(w *LoadedWorld, q *ScenarioQuestion, emitted []AnswerFact, sc *ChainScore) {
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

// toFact builds the fact.Fact knomit would validate for a.
func toFact(a AnswerFact) fact.Fact {
	f := fact.NewFact(fact.BuildFactPath("kb", a.Topic, a.Category))
	f.Title, f.Body = a.Title, a.Body
	f.Kind, f.Type = fact.Kind(a.Kind), fact.Type(a.Type)
	if f.Kind == "" {
		f.Kind = fact.DefaultKind
	}
	f.Origin = fact.Origin(a.Origin)
	f.Confidence, f.Sources = a.Confidence, a.Sources
	f.Domain, f.Entities, f.Motifs, f.Refs = a.Domain, a.Entities, a.Motifs, a.Refs
	f.Context = jsonRoundTrip(a.Context)
	return f
}

// checkFact runs knomit's own checks on a and returns the failures and the
// number of checks run.
func checkFact(w *LoadedWorld, a AnswerFact) (fails []string, n int) {
	o := w.Spec.parsed
	check := func(name string, ok bool, detail string) {
		n++
		if !ok {
			fails = append(fails, name+": "+detail)
		}
	}
	perr := o.ValidatePath(a.Topic + "/" + a.Category)
	check("ontology_path", a.Topic != "" && a.Category != "" && perr == nil, fmt.Sprint(perr))
	f := toFact(a)
	verr := fact.ValidateFact(o, a.Topic+"/"+a.Category, f)
	check("validate_fact", verr == nil, fmt.Sprint(verr))
	_, serr := fact.SerializeFact(f)
	check("serialize", serr == nil, fmt.Sprint(serr))
	check("kind_allows_type", f.Kind.AllowsType(f.Type), string(f.Kind)+"/"+string(f.Type))
	check("fields", strings.TrimSpace(a.Title) != "" && strings.TrimSpace(a.Body) != "" &&
		len(a.Domain) > 0 && len(a.Entities) > 0 && len(a.Refs) > 0,
		"title, body, domain, entities and refs must all be populated")
	return fails, n
}

func gradeValidity(w *LoadedWorld, q *ScenarioQuestion, chain ReasonChain, sc *ChainScore) {
	var fails []string
	n := 0
	check := func(name string, ok bool, detail string) {
		n++
		if !ok {
			fails = append(fails, name+": "+detail)
		}
	}
	if q.Outcome == "gap" {
		var h *AnswerFact
		if chain.Gap != nil {
			h = chain.Gap.Hypothesis
		}
		check("gap_declared", chain.Gap != nil, "the gold outcome is a gap")
		check("no_answer", len(chain.Answers) == 0, "a gap question must not emit an answer")
		check("hypothesis_present", h != nil, "a gap emits an evidence-free hypothesis fact")
		if h != nil {
			f, k := checkFact(w, *h)
			fails, n = append(fails, f...), n+k
			check("hypothesis_type", h.Type == string(fact.Hypothesis), "type is "+h.Type)
			check("settlement", strings.TrimSpace(chain.Gap.Settlement) != "", "a gap names the evidence that would settle it")
		}
		sc.AnswerValidity = ratio(n-len(fails), n)
		sc.AnswerFailures = fails
		sc.GapHypothesisValid = h != nil && len(fails) == 0
		return
	}
	g := q.Answer
	if len(chain.Answers) == 0 {
		sc.AnswerValidity = 0
		sc.AnswerFailures = []string{"no_answer: the gold outcome is " + q.Outcome}
		return
	}
	a := chain.Answers[0]
	f, k := checkFact(w, a)
	fails, n = append(fails, f...), n+k
	check("type", slices.Contains(g.Types, a.Type), fmt.Sprintf("%s not in %v", a.Type, g.Types))
	check("topic", strings.EqualFold(a.Topic, g.Topic) &&
		(g.Category == "" || strings.HasPrefix(strings.ToLower(a.Category), strings.ToLower(g.Category))),
		a.Topic+"/"+a.Category)
	if len(g.Confidence) == 2 {
		check("confidence", a.Confidence >= g.Confidence[0] && a.Confidence <= g.Confidence[1],
			fmt.Sprintf("%v outside %v", a.Confidence, g.Confidence))
	}
	cited := citedIDs(w, chain.Answers)
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
	check("entities", containsAllFold(a.Entities, g.Entities), fmt.Sprintf("want %v", g.Entities))
	check("motifs", containsAllFold(a.Motifs, g.Motifs), fmt.Sprintf("want %v", g.Motifs))
	sc.AnswerValidity = ratio(n-len(fails), n)
	sc.AnswerFailures = fails
}

func gradeRefs(ctx context.Context, w *LoadedWorld, chain ReasonChain, emitted []AnswerFact, sc *ChainScore) {
	readAt := map[string]string{}
	for _, st := range chain.Steps {
		for _, fa := range st.Facts {
			c := fact.ClassifyRef(fa.Path, w.RepoID)
			if c.Kind == fact.RefLocalFact && fa.Commit != "" {
				readAt[c.Path] = fa.Commit
			}
		}
	}
	base := chain.World.Commit
	if base == "" {
		base = w.Head
	}
	sources := map[string]bool{}
	for _, u := range w.Spec.ExternalSources {
		sources[u] = true
	}
	total, ok := 0, 0
	for _, a := range emitted {
		for _, r := range a.Refs {
			total++
			c := fact.ClassifyRef(r, w.RepoID)
			good := false
			switch c.Kind {
			case fact.RefLocalFact:
				at := readAt[c.Path]
				if at == "" {
					at = base
				}
				live, err := w.Svc.Search().FactLiveAtCommit(ctx, w.Branch, c.Path, at)
				good = err == nil && live
			case fact.RefLocalSystemFile:
				_, err := w.Svc.Facts().ReadFact(ctx, w.Branch, c.Path, &store.ReadFactOpts{AtCommit: base})
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
