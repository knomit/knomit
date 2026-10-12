package synthesize_test

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/mcp"
	"knomit/internal/repos"
	"knomit/internal/synthesize"
)

// The full reasoning loop (internal/repos/engine_reason.js) driven over real
// scenario worlds with a SCRIPTED model: an oracle that answers every
// judgment from the question's gold (relevance and support labels, the gold
// chain's moves, the gold claim and missing premise). It makes the run
// deterministic and isolates what these tests measure: the HARNESS — that it
// offers the gold moves on its frontier, executes them, carries the support
// set through backtracking, computes the posterior, places and populates the
// answer fact and its refs, and declares gaps — scored by the same graders
// the gating tests use (GradeChain). A real model is measured elsewhere
// (E05); a low score here is a harness bug, not a model one.

const pilotDir = "testdata/scenario/pilot/"

// bowEmbedder is a deterministic bag-of-words embedder: lowercased word
// tokens (stopwords dropped, a plural "s" folded) hashed into 768 buckets,
// L2-normalised, so cosine is word overlap. Its id has no calibrated band in
// params, so the learn-time same-subject gate is off, and its Dedup floor is
// above any real cosine, so no world fact is folded into another: the world
// loads exactly as authored.
type bowEmbedder struct{}

var bowStop = map[string]bool{}

func init() {
	for _, w := range strings.Fields("a an and are as at be by for from has have how in into is it its of on or that the this to was were what when which who why with do does did can could should would will much many than then there these those about after before over under per not no yes our we you they their them his her") {
		bowStop[w] = true
	}
}

var bowWord = regexp.MustCompile(`[a-z0-9]+`)

func (bowEmbedder) vec(text string) []float32 {
	out := make([]float32, 768)
	for _, w := range bowWord.FindAllString(strings.ToLower(text), -1) {
		if bowStop[w] || len(w) < 2 {
			continue
		}
		if len(w) > 4 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = w[:len(w)-1]
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(w))
		out[h.Sum32()%768]++
	}
	var n float64
	for _, x := range out {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		out[767] = 1
		return out
	}
	n = math.Sqrt(n)
	for i := range out {
		out[i] = float32(float64(out[i]) / n)
	}
	return out
}
func (e bowEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return e.vec(text), nil
}
func (e bowEmbedder) EmbedDocument(_ context.Context, title, body string) ([]float32, error) {
	return e.vec(title + " " + body), nil
}
func (e bowEmbedder) EmbedDocuments(_ context.Context, titles, bodies []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.vec(titles[i] + " " + bodies[i])
	}
	return out, nil
}
func (e bowEmbedder) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vec(t)
	}
	return out, nil
}
func (bowEmbedder) Dim() int   { return 768 }
func (bowEmbedder) ID() string { return "bow-test" }
func (bowEmbedder) Thresholds() params.Thresholds {
	th := params.Defaults()
	th.Dedup, th.ReflectNovelty = 0.9999, 0.9998
	th.SearchFloor = 0.12
	return th
}

// --- the oracle -------------------------------------------------------------

var (
	oFact = regexp.MustCompile(`(?m)^\[(\d+)\] (\S+) \(`)
	oMenu = regexp.MustCompile(`(?m)^(\d+)\. ([a-z_]+)\((.*)\) — `)
	oDone = regexp.MustCompile(`(?m)^- ([a-z_]+)\((.*)\)$`)
)

type oracleMove struct{ move, from, arg string }

// oracle answers each judgment from q's gold, in world w.
type oracle struct {
	w *synthesize.LoadedWorld
	q *synthesize.ScenarioQuestion
	// fail lists judgment names to answer with an off-schema reply (the
	// host throws it to the script).
	fail map[string]bool
	// dupSupport repeats every support verdict in the reply, then repeats it
	// once more with the opposite verdict: one fact, three verdicts.
	dupSupport bool
	// onCall runs before a judgment is answered (a peer session acting on
	// the world while the run is inside a model call).
	onCall func(name string)

	mu      sync.Mutex
	byName  map[string]int
	prompts map[string][]string // the user briefing of every call, by judgment
}

func (o *oracle) id(path string) string { return o.w.IDOfPath(path) }

func (o *oracle) goldLabels() (rel, sup map[string]string) {
	rel, sup = map[string]string{}, map[string]string{}
	for _, g := range o.q.Chain {
		for id, v := range g.Relevant {
			rel[id] = v
		}
		for id, v := range g.Support {
			sup[id] = v
		}
	}
	return rel, sup
}

func (o *oracle) inSet(id string, sets ...[]string) bool {
	for _, s := range sets {
		for _, x := range s {
			if x == id {
				return true
			}
		}
	}
	return false
}

func (o *oracle) relevant(id string) string {
	rel, _ := o.goldLabels()
	if v, ok := rel[id]; ok {
		return v
	}
	var should, partial []string
	if o.q.Answer != nil {
		should = o.q.Answer.ShouldCite
	}
	if o.q.Gap != nil {
		partial = o.q.Gap.PartialSupport
	}
	if o.inSet(id, o.q.Support, should, partial) {
		return "yes"
	}
	return "no"
}

func (o *oracle) support(id string) string {
	_, sup := o.goldLabels()
	if o.inSet(id, o.q.Support) {
		return "supports"
	}
	if v, ok := sup[id]; ok {
		return v
	}
	return "unrelated"
}

// matched replays the grader's greedy match of taken moves onto gold steps.
func (o *oracle) matched(taken []oracleMove) map[string]bool {
	m := map[string]bool{}
	for _, t := range taken {
		for _, g := range o.q.Chain {
			if m[g.ID] || !allIn(g.Requires, m) {
				continue
			}
			if o.moveMatches(t, g.Moves) {
				m[g.ID] = true
				break
			}
		}
	}
	return m
}

func allIn(ids []string, m map[string]bool) bool {
	for _, id := range ids {
		if !m[id] {
			return false
		}
	}
	return true
}

func (o *oracle) moveMatches(t oracleMove, alts []synthesize.GoldMove) bool {
	for _, a := range alts {
		if a.Move != t.move {
			continue
		}
		switch {
		case a.From != "":
			if t.from != "" && o.id(t.from) == a.From {
				return true
			}
		case a.Move == "ontology":
			if strings.Trim(strings.ToLower(t.arg), "/") == strings.Trim(strings.ToLower(a.Arg), "/") {
				return true
			}
		default:
			if strings.EqualFold(strings.TrimSpace(t.arg), strings.TrimSpace(a.Arg)) {
				return true
			}
		}
	}
	return false
}

func parseMove(name, inner string) oracleMove {
	if strings.HasPrefix(inner, "kb/") || strings.HasPrefix(inner, "kb://") {
		return oracleMove{move: name, from: inner}
	}
	return oracleMove{move: name, arg: inner}
}

// choose picks the menu entry that matches the next eligible gold step, or
// stops (0) when none on the menu does.
func (o *oracle) choose(user string) int {
	var taken []oracleMove
	for _, m := range oDone.FindAllStringSubmatch(user, -1) {
		taken = append(taken, parseMove(m[1], m[2]))
	}
	done := o.matched(taken)
	for _, m := range oMenu.FindAllStringSubmatch(user, -1) {
		mv := parseMove(m[2], m[3])
		for _, g := range o.q.Chain {
			if done[g.ID] || !allIn(g.Requires, done) {
				continue
			}
			if o.moveMatches(mv, g.Moves) {
				var n int
				fmt.Sscan(m[1], &n)
				return n
			}
		}
	}
	return 0
}

func (o *oracle) reply(name, user string) string {
	o.mu.Lock()
	o.byName[name]++
	if o.prompts == nil {
		o.prompts = map[string][]string{}
	}
	o.prompts[name] = append(o.prompts[name], user)
	o.mu.Unlock()
	if o.onCall != nil {
		o.onCall(name)
	}
	if o.fail[name] {
		return `{"reasoning": "r", "verdict": "maybe"}`
	}
	var facts []string
	for _, m := range oFact.FindAllStringSubmatch(user, -1) {
		facts = append(facts, m[2])
	}
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	switch name {
	case "relevant":
		var labels []any
		for i, p := range facts {
			labels = append(labels, map[string]any{"fact": i + 1, "relevant": o.relevant(o.id(p))})
		}
		return js(map[string]any{"reasoning": "gold", "labels": labels})
	case "support":
		var vs []any
		for i, p := range facts {
			v := map[string]any{"fact": i + 1, "verdict": o.support(o.id(p))}
			vs = append(vs, v)
			if o.dupSupport {
				other := "contradicts"
				if v["verdict"] == "contradicts" {
					other = "supports"
				}
				vs = append(vs, v, map[string]any{"fact": i + 1, "verdict": other})
			}
		}
		return js(map[string]any{"reasoning": "gold", "verdicts": vs})
	case "choose_move", "backtrack_choice":
		return js(map[string]any{"reasoning": "gold", "choice": o.choose(user)})
	case "synthesize":
		claim := ""
		if o.q.Answer != nil {
			claim = strings.Join(strings.Fields(o.q.Answer.Claim), " ")
		}
		var carriers []int
		for i, p := range facts {
			if o.inSet(o.id(p), o.q.Support) {
				carriers = append(carriers, i+1)
			}
		}
		return js(map[string]any{"reasoning": "gold", "claim": claim, "consequence": "", "misreading": "", "carriers": carriers})
	case "assess":
		have := map[string]bool{}
		for _, p := range facts {
			have[o.id(p)] = true
		}
		verdict := o.q.Assess
		if o.q.Outcome != "gap" {
			for _, id := range o.q.Support {
				if !have[id] {
					verdict = "partly"
				}
			}
		}
		missing := ""
		if o.q.Gap != nil {
			missing = strings.Join(strings.Fields(o.q.Gap.MissingPremise), " ")
		}
		return js(map[string]any{"reasoning": "gold", "assess": verdict, "missing": missing})
	case "hypothesize":
		claim, settle := "unknown", "unknown"
		if o.q.Gap != nil {
			claim = strings.Join(strings.Fields(o.q.Gap.MissingPremise), " ")
			settle = strings.Join(strings.Fields(o.q.Gap.Settlement), " ")
		}
		return js(map[string]any{"reasoning": "gold", "claim": claim, "settlement": settle, "observable": settle})
	case "seek":
		s := o.q.Text
		if o.q.Gap != nil {
			s = o.q.Gap.Settlement
		}
		return js(map[string]any{"reasoning": "gold", "search": strings.Join(strings.Fields(s), " ")})
	}
	return `{}`
}

func (o *oracle) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			ResponseFormat struct {
				JSONSchema struct {
					Name string `json:"name"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		user := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				user = m.Content
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant",
				"content": o.reply(req.ResponseFormat.JSONSchema.Name, user)}, "finish_reason": "stop"}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- running and grading ----------------------------------------------------

func loadPilot(t *testing.T, path string) *synthesize.LoadedWorld {
	t.Helper()
	s, err := synthesize.ReadScenarioFile(path)
	require.NoError(t, err)
	w, err := loadSpec(t, s, bowEmbedder{})
	require.NoError(t, err)
	return w
}

func questionByID(t *testing.T, w *synthesize.LoadedWorld, id string) *synthesize.ScenarioQuestion {
	t.Helper()
	for i := range w.Spec.Questions {
		if w.Spec.Questions[i].ID == id {
			return &w.Spec.Questions[i]
		}
	}
	t.Fatalf("no question %s in %s", id, w.Spec.ID)
	return nil
}

type engineRun struct {
	res   repos.ReasonResult
	chain synthesize.ReasonChain
	score synthesize.ChainScore
	o     *oracle
}

func runOracle(t *testing.T, w *synthesize.LoadedWorld, q *synthesize.ScenarioQuestion, o *oracle, opts repos.ReasonOptions) engineRun {
	t.Helper()
	if o == nil {
		o = &oracle{}
	}
	o.w, o.q, o.byName = w, q, map[string]int{}
	srv := o.server(t)
	eng := repos.NewEngine(repos.EngineConfig{
		Model: config.EngineConfig{ModelURL: srv.URL + "/v1", Model: "oracle"},
		Tools: mcp.NewScriptTools(bowEmbedder{}), Home: t.TempDir(),
	})
	if opts.Budget == 0 {
		opts.Budget = time.Minute
	}
	res, err := eng.RunWith(context.Background(), repos.NewBindingOfRepo(w.RI, w.Branch), q.Text, opts)
	require.NoError(t, err)
	b, err := json.Marshal(res)
	require.NoError(t, err)
	var chain synthesize.ReasonChain
	require.NoError(t, json.Unmarshal(b, &chain), "ReasonResult decodes as the graders' ReasonChain")
	sc, err := synthesize.GradeChain(context.Background(), w, q, chain)
	require.NoError(t, err)
	return engineRun{res: res, chain: chain, score: sc, o: o}
}

func (r engineRun) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "outcome=%s posterior=%v error=%q\n", r.res.Outcome, r.res.Posterior, r.res.Error)
	for _, s := range r.res.Chain {
		fmt.Fprintf(&b, "  %d %s %s(%s%s) %s %s %v\n", s.Step, s.State, s.Move, s.From, s.Arg, s.Judgment, s.Verdict, s.Support)
	}
	fmt.Fprintf(&b, "gaps=%+v\nscore=%+v\n", r.res.Gaps, r.score)
	if ps := r.o.prompts["backtrack_choice"]; len(ps) > 0 {
		fmt.Fprintf(&b, "last backtrack_choice briefing:\n%s\n", ps[len(ps)-1])
	}
	return b.String()
}

// minScore is what an answerable question's chain must reach with the gold
// oracle: every mechanical metric the grader computes, as a floor.
type minScore struct {
	moveAccuracy, moveRecall, relRecall, supportAcc, chainRecall, chainPrecision, validity, refs float64
}

func requireAtLeast(t *testing.T, r engineRun, m minScore) {
	t.Helper()
	sc := r.score
	require.Equal(t, "answer", r.res.Outcome, r.String())
	require.True(t, sc.OutcomeCorrect, r.String())
	require.GreaterOrEqual(t, sc.MoveAccuracy, m.moveAccuracy, "move accuracy\n%s", r)
	require.GreaterOrEqual(t, sc.MoveRecall, m.moveRecall, "move recall\n%s", r)
	require.GreaterOrEqual(t, sc.RelevanceRecall, m.relRecall, "relevance recall\n%s", r)
	require.GreaterOrEqual(t, sc.SupportAccuracy, m.supportAcc, "support accuracy\n%s", r)
	require.GreaterOrEqual(t, sc.ChainRecall, m.chainRecall, "chain recall\n%s", r)
	require.GreaterOrEqual(t, sc.ChainPrecision, m.chainPrecision, "chain precision\n%s", r)
	require.GreaterOrEqual(t, sc.AnswerValidity, m.validity, "answer validity %v\n%s", sc.AnswerFailures, r)
	require.GreaterOrEqual(t, sc.RefCorrectness, m.refs, "ref correctness %v\n%s", sc.BadRefs, r)
	require.Empty(t, sc.ForbiddenCited, r.String())
	require.Zero(t, sc.Misattribution, r.String())
}

func requireGap(t *testing.T, r engineRun) {
	t.Helper()
	sc := r.score
	require.Equal(t, "gap", r.res.Outcome, r.String())
	require.True(t, sc.GapDeclared && sc.OutcomeCorrect, r.String())
	require.True(t, sc.GapHypothesisValid, "the gap's hypothesis passes every check: %v\n%s", sc.AnswerFailures, r)
	require.Empty(t, sc.ForbiddenCited, r.String())
	require.Equal(t, 1.0, sc.RefCorrectness, r.String())
	require.True(t, sc.Correct, r.String())
	require.NotEmpty(t, r.res.Gaps[0].Missing, "the gap names the missing premise")
}

// TestEngineOracle_Example: the reference world. q1 is answered through the
// gold chain (the synthesis f03, its members via refs_out, the methodology
// f08) and the answer cites every gold support and the methodology; q2 (a
// fruit nobody stored there) ends on a gap with a valid hypothesis.
func TestEngineOracle_Example(t *testing.T) {
	w := loadPilot(t, exampleSpec)
	r := runOracle(t, w, questionByID(t, w, "q1"), nil, repos.ReasonOptions{})
	t.Log(r)
	requireAtLeast(t, r, minScore{moveAccuracy: 1, moveRecall: 1, relRecall: 1, supportAcc: 1, chainRecall: 1, chainPrecision: 1, validity: 1, refs: 1})
	require.True(t, r.score.Correct, r.String())

	g := runOracle(t, w, questionByID(t, w, "q2"), nil, repos.ReasonOptions{})
	t.Log(g)
	requireGap(t, g)
}

// TestEngineOracle_BacktrackOnRetractedPremise (F13, tile margin): the
// highest-ranked line is the synthesis c10, which rests on c01 — retracted
// at HEAD. refs_out(c10) shows the dead premise; the harness confirms it
// (supersession(c10)), pops the line — c10 and c02, reached only through
// it, are dropped — and the model's backtrack_choice opens the energy line
// (refs_in(c05) -> c06, motif -> c07). The answer cites exactly the gold
// support and never c10 or c01. Sabotage: make checkPremises return null
// (red: the answer rests on c10 and cites it, a forbidden fact).
func TestEngineOracle_BacktrackOnRetractedPremise(t *testing.T) {
	w := loadPilot(t, pilotDir+"f13-gsm-tile-margin/spec.yaml")
	r := runOracle(t, w, questionByID(t, w, "q1"), nil, repos.ReasonOptions{})
	t.Log(r)
	var back *repos.ReasonStep
	for i, s := range r.res.Chain {
		if s.State == "BACKTRACK" && s.Move == "supersession" {
			back = &r.res.Chain[i]
		}
	}
	require.NotNil(t, back, "a BACKTRACK step ran\n%s", r)
	require.Equal(t, w.Paths["c10"], back.From)
	require.Equal(t, 1, r.o.byName["backtrack_choice"], "the model chose where to resume")
	requireAtLeast(t, r, minScore{moveAccuracy: 1, moveRecall: 1, relRecall: 1, supportAcc: 0.85, chainRecall: 1, chainPrecision: 1, validity: 1, refs: 1})
	require.True(t, r.score.Correct, r.String())
	require.InDelta(t, 0.75, r.res.Posterior, 1e-9, "the weakest of c03 0.85, c05 0.75, c06 0.8, c07 0.8")

	// and the gap sibling: both lines die (the retracted price, a
	// hypothesis with no mass), so the run declares a gap
	g := runOracle(t, w, questionByID(t, w, "q2"), nil, repos.ReasonOptions{})
	t.Log(g)
	requireGap(t, g)
}

// TestEngineOracle_MultiHop (F2, seed mites): q1 is three hops down from a
// market synthesis, through a premise that was revised after it was cited
// (superseded: read at HEAD, history, never error); q2 is two hops to a
// reference.
func TestEngineOracle_MultiHop(t *testing.T) {
	w := loadPilot(t, pilotDir+"f02-gsm-seed-mites/spec.yaml")
	r := runOracle(t, w, questionByID(t, w, "q1"), nil, repos.ReasonOptions{})
	t.Log(r)
	// precision 2/3: the gold chain labels m12 and m21 `supports` on the way
	// down but leaves them out of the support set, so citing what the gold
	// itself called support counts against precision
	requireAtLeast(t, r, minScore{moveAccuracy: 1, moveRecall: 1, relRecall: 1, supportAcc: 1, chainRecall: 1, chainPrecision: 0.66, validity: 1, refs: 1})
	require.True(t, r.score.Correct, r.String())

	r2 := runOracle(t, w, questionByID(t, w, "q2"), nil, repos.ReasonOptions{})
	t.Log(r2)
	// validity 12/13: the answer is filed beside the synthesis m15
	// (markets/firms); the gold files it under science/chemistry
	requireAtLeast(t, r2, minScore{moveAccuracy: 1, moveRecall: 1, relRecall: 1, supportAcc: 1, chainRecall: 1, chainPrecision: 1, validity: 0.92, refs: 1})
}

// TestEngineOracle_Gap (F11, sedge germination): q1's premise is never
// stated; every near miss is another lot, year or condition. The run ends on
// a gap with a valid evidence-free hypothesis that cites no forbidden fact.
// q2 (its answerable sibling) is answered directly.
func TestEngineOracle_Gap(t *testing.T) {
	w := loadPilot(t, pilotDir+"f11-gsm-sedge-germination/spec.yaml")
	g := runOracle(t, w, questionByID(t, w, "q1"), nil, repos.ReasonOptions{})
	t.Log(g)
	requireGap(t, g)

	r := runOracle(t, w, questionByID(t, w, "q2"), nil, repos.ReasonOptions{})
	t.Log(r)
	requireAtLeast(t, r, minScore{moveAccuracy: 1, moveRecall: 1, relRecall: 1, supportAcc: 1, chainRecall: 1, chainPrecision: 1, validity: 1, refs: 1})
	require.True(t, r.score.Correct, r.String())
}

// A model that gives one fact several verdicts in one reply (fix-later 3) —
// here each verdict twice, then its opposite — gets ONE verdict per fact,
// its first: the posterior, the support labels and the answer's sources are
// those of the clean run. Sabotage: drop the `verdicts[p]` guard in
// judgeSupport (red: the last verdict wins, supports become contradicts and
// the posterior falls).
func TestEngineOracle_DuplicateVerdictsDeduped(t *testing.T) {
	w := loadPilot(t, pilotDir+"f13-gsm-tile-margin/spec.yaml")
	q := questionByID(t, w, "q1")
	clean := runOracle(t, w, q, nil, repos.ReasonOptions{})
	dup := runOracle(t, w, q, &oracle{dupSupport: true}, repos.ReasonOptions{})
	require.Equal(t, clean.res.Posterior, dup.res.Posterior, dup.String())
	require.Len(t, dup.res.AnswerFacts, 1, dup.String())
	f := readAnswer(t, w, dup.chain)
	fc := readAnswer(t, w, clean.chain)
	require.Equal(t, fc.Sources, f.Sources, "sources count each support once")
	require.Equal(t, 4, f.Sources, "c03, c05, c06, c07")
	require.Equal(t, clean.score, scoreWithoutQuestion(dup.score, clean.score.Question))
}

func scoreWithoutQuestion(s synthesize.ChainScore, q string) synthesize.ChainScore {
	s.Question = q
	return s
}

// A model that throws mid-ASSESS — every support judgment comes back off
// schema, so the host refuses it — does not end the run as an error
// (fix-later 6): each failure is recorded, read as `unrelated`, and the run
// reaches a gap with its hypothesis written. Sabotage: drop the try/catch in
// judge() (red: outcome error).
func TestEngineOracle_ModelThrowMidAssessYieldsGap(t *testing.T) {
	w := loadPilot(t, exampleSpec)
	r := runOracle(t, w, questionByID(t, w, "q1"), &oracle{fail: map[string]bool{"support": true}}, repos.ReasonOptions{})
	t.Log(r)
	require.Equal(t, "gap", r.res.Outcome, r.String())
	require.Empty(t, r.res.Error)
	failed := 0
	for _, s := range r.res.Chain {
		if s.Judgment == "support" && strings.HasPrefix(s.Verdict, "failed: ") {
			failed++
		}
	}
	require.Positive(t, failed, "the failed judgments are in the chain")
	require.NotEmpty(t, r.res.Gaps)
	require.Len(t, r.res.AnswerFacts, 1, "the gap's hypothesis is written")
	f := readAnswer(t, w, r.chain)
	require.Equal(t, "hypothesis", string(f.Type))
}

// The move budget bounds exploration, distinct from the wall clock: with one
// move (the gather) the loop widens no further and still closes with an
// answer or a gap.
func TestEngineOracle_MoveBudget(t *testing.T) {
	w := loadPilot(t, pilotDir+"f13-gsm-tile-margin/spec.yaml")
	r := runOracle(t, w, questionByID(t, w, "q1"), nil, repos.ReasonOptions{MaxMoves: 1})
	t.Log(r)
	moves := 0
	for _, s := range r.res.Chain {
		if synthesize.ScenarioMoves[s.Move] && s.State != "BACKTRACK" {
			moves++
		}
	}
	require.Equal(t, 1, moves, r.String())
	require.Contains(t, []string{"answer", "gap"}, r.res.Outcome)
	require.Zero(t, r.o.byName["choose_move"], "no widening was offered")
}

func readAnswer(t *testing.T, w *synthesize.LoadedWorld, chain synthesize.ReasonChain) fact.Fact {
	t.Helper()
	require.NotEmpty(t, chain.AnswerFacts)
	res, err := w.Svc.Facts().ReadFact(context.Background(), chain.ExperimentBranch, chain.AnswerFacts[0], nil)
	require.NoError(t, err)
	f, err := fact.ParseFact(chain.AnswerFacts[0], res.Content)
	require.NoError(t, err)
	return f
}

// Freshness (E02 §7): EMIT re-explains every support at HEAD before it
// writes. Here a peer session retracts the support f02 while the run is
// inside its DECIDE judgment; EMIT finds it gone, drops it, re-decides, and
// the answer it writes does not rest on the retracted fact. Sabotage: make
// EMIT skip the freshness loop (red: the write cites f02, which the refs gate
// refuses on the experiment forked after the retraction, and the run ends on
// a gap).
func TestEngineOracle_FreshnessBeforeEmit(t *testing.T) {
	w := loadPilot(t, exampleSpec)
	retracted := false
	o := &oracle{onCall: func(name string) {
		if name != "assess" || retracted {
			return
		}
		retracted = true
		text, isErr, err := mcp.NewScriptTools(bowEmbedder{}).Call(w.Ctx(context.Background()), "retract",
			map[string]any{"file": w.Paths["f02"], "moment_name": "peer retracts f02"})
		require.NoError(t, err)
		require.False(t, isErr, text)
	}}
	r := runOracle(t, w, questionByID(t, w, "q1"), o, repos.ReasonOptions{})
	t.Log(r)
	require.True(t, retracted)
	fresh := false
	for _, s := range r.res.Chain {
		if s.State == "EMIT" && s.Move == "freshness" {
			fresh = true
			require.Contains(t, s.Facts, w.Paths["f02"])
		}
	}
	require.True(t, fresh, "the freshness check recorded the moved support\n%s", r)
	require.Equal(t, "answer", r.res.Outcome, r.String())
	f := readAnswer(t, w, r.chain)
	for _, ref := range f.Refs {
		require.False(t, strings.HasSuffix(ref, w.Paths["f02"]), "the answer cites the retracted f02: %v", f.Refs)
	}
}
