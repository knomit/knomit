package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// knomit_reason end to end: the real tool set (NewScriptTools), the embedded
// reason.js, a real repo, and a fake OpenAI-compatible model that returns a
// fixed judgment. What lands is checked on the branches themselves.

// reasonOntologyYAML declares the run key as a context key on architecture,
// so the answer fact can carry it; learn_dedup is off so the seeds and the
// answer are never folded into each other by the embedder below.
const reasonOntologyYAML = `id: r
name: R
topics:
  architecture:
    description: structure
    attributes:
      learn_dedup: off
    context:
      reason_run: {type: string, pattern: "^reason-[0-9a-f]{8}$"}
  decisions:
    description: choices
    attributes:
      learn_dedup: off
`

// reasonEmbedder: "match-target" text embeds to e0, "weak-target" to
// cosine 0.8 with it, everything else to e2 (unrelated to both).
type reasonEmbedder struct{}

func (reasonEmbedder) vec(text string) []float32 {
	out := make([]float32, 768)
	switch {
	case strings.Contains(text, "match-target"):
		out[0] = 1
	case strings.Contains(text, "weak-target"):
		out[0], out[1] = 0.8, 0.6
	default:
		out[2] = 1
	}
	return out
}
func (e reasonEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return e.vec(text), nil
}
func (e reasonEmbedder) EmbedDocument(_ context.Context, title, body string) ([]float32, error) {
	return e.vec(title + " " + body), nil
}
func (e reasonEmbedder) EmbedDocuments(_ context.Context, titles, bodies []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.vec(titles[i] + " " + bodies[i])
	}
	return out, nil
}
func (e reasonEmbedder) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vec(t)
	}
	return out, nil
}
func (reasonEmbedder) Dim() int                      { return 768 }
func (reasonEmbedder) ID() string                    { return "reason-test" }
func (reasonEmbedder) Thresholds() params.Thresholds { return params.Defaults() }

type reasonFixture struct {
	ri       *repos.RepoInstance
	svc      *store.Service
	ctx      context.Context
	a, b, c  string
	model    *httptest.Server
	modelMu  sync.Mutex
	requests int
	delay    time.Duration
	eng      *repos.Engine
}

const reasonJudgment = `{"claim": "Alpha holds only while beta is configured.", ` +
	`"verdicts": [{"fact": 1, "verdict": "supports"}, {"fact": 2, "verdict": "supports"}], "missing": ""}`

func newReasonFixture(t *testing.T, ontologyYAML string) *reasonFixture {
	t.Helper()
	ont, err := fact.ParseOntology([]byte(ontologyYAML))
	require.NoError(t, err)
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	emb := reasonEmbedder{}
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "test", UID: nextTestRepoUID(), AgentBranch: "agent/test", Svc: svc,
		Ontology: ont, OntologyRoot: "kb", Embedder: emb,
	})
	f := &reasonFixture{ri: ri, svc: svc, ctx: repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, "agent/test"))}

	learn := func(item map[string]any) string {
		res, err := LearnHandler(emb)(f.ctx, learnItems(item))
		require.NoError(t, err)
		require.False(t, res.IsError, resultText(t, res))
		var out struct {
			Commits []struct{ File string } `json:"commits"`
		}
		require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))
		return out.Commits[0].File
	}
	f.a = learn(map[string]any{"topic": "architecture", "category": "engine/alpha", "title": "Alpha match-target holds",
		"body": "Alpha match-target holds while beta is configured.", "confidence": 0.9, "sources": 1,
		"entities": []any{"alpha"}, "domain": []any{"engine"}})
	f.b = learn(map[string]any{"topic": "architecture", "category": "engine/beta", "title": "Beta weak-target configures alpha",
		"body": "Beta weak-target is the configuration alpha needs.", "confidence": 0.8, "sources": 1,
		"entities": []any{"beta"}, "domain": []any{"engine"}})
	// C cites A and matches nothing in the question: it is reached only by
	// refs_in(A), never by the gather.
	f.c = learn(map[string]any{"topic": "decisions", "category": "engine/gamma", "title": "Gamma rests on alpha",
		"body": "Gamma was chosen because alpha holds.", "confidence": 0.7, "sources": 1,
		"entities": []any{"gamma"}, "refs": []any{f.a}})

	f.model = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.modelMu.Lock()
		f.requests++
		d := f.delay
		f.modelMu.Unlock()
		if d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reasonJudgment}, "finish_reason": "stop"}},
		})
	}))
	t.Cleanup(f.model.Close)
	f.eng = repos.NewEngine(repos.EngineConfig{
		Model: config.EngineConfig{ModelURL: f.model.URL + "/v1", Model: "lfm-test"},
		Tools: NewScriptTools(emb), Home: t.TempDir(),
	})
	return f
}

func (f *reasonFixture) call(t *testing.T, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Name = "knomit_reason"
	req.Params.Arguments = args
	res, err := ReasonHandler(func() *repos.Engine { return f.eng })(f.ctx, req)
	require.NoError(t, err)
	return res
}

type reasonLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *reasonLogBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *reasonLogBuf) linesOf(t *testing.T, run string) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(s.b.String(), "\n") {
		if !strings.Contains(l, run) {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &m), l)
		out = append(out, m)
	}
	return out
}

// The whole loop: gather finds A and B, widen explains them and reads C as a
// citer of A, the fake model's judgment says both support, and EMIT writes
// ONE synthesis into a new experiment — refs to A and B, the run key in its
// context and its trace trailer — leaving the agent branch untouched. A
// rollback (the asker's decline) removes it. Every log line of the run
// carries reason_run. Sabotage: make experimentOpen keep the world binding
// (red: the answer lands on agent/test).
func TestReason_EndToEnd_AnswerInExperimentThenRollback(t *testing.T) {
	buf := &reasonLogBuf{}
	prev := log.Logger
	log.Logger = zerolog.New(buf).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = prev })

	f := newReasonFixture(t, reasonOntologyYAML)
	agentBefore := headOf(t, f.ri, "agent/test")

	res := f.call(t, map[string]any{"question": "Does alpha match-target hold?", "budget": 60})
	require.False(t, res.IsError, resultText(t, res))
	var out repos.ReasonResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))

	require.Equal(t, "answer", out.Outcome, resultText(t, res))
	require.Regexp(t, `^reason-[0-9a-f]{8}$`, out.Run)
	require.Equal(t, 1, out.ModelCalls)
	require.Len(t, out.AnswerFacts, 1)
	require.True(t, strings.HasPrefix(out.Experiment, "reason-does-alpha-match-target-hold-"), out.Experiment)
	require.Equal(t, "exp/"+out.Experiment, out.ExperimentBranch)
	require.Equal(t, 0.63, out.Posterior, "(0.9 + 0.8) / (0.9 + 0.8 + 1)")
	require.Contains(t, out.Next, `{action: "rollback", name: "`+out.Experiment+`"}`)

	// the chain: gather, widen (refs_in of A surfaces C), the judgment, the write
	var moves []string
	var sawC bool
	var assess *repos.ReasonStep
	for i, s := range out.Chain {
		moves = append(moves, s.Move)
		if s.Move == "refs_in" && s.From == f.a {
			sawC = len(s.Facts) == 1 && s.Facts[0] == f.c
		}
		if s.Judgment == "support" {
			assess = &out.Chain[i]
		}
	}
	require.Equal(t, "neighbours", out.Chain[0].Move)
	require.Equal(t, "question", out.Chain[0].Arg)
	require.True(t, sawC, "refs_in(A) surfaced C: %v", out.Chain)
	require.NotNil(t, assess)
	require.Equal(t, map[string]string{f.a: "supports", f.b: "supports"}, assess.Support)
	require.Equal(t, "learn", moves[len(moves)-1])

	// what landed, on the branches themselves
	path := out.AnswerFacts[0]
	require.True(t, strings.HasPrefix(path, "kb/architecture/engine/alpha/"), path)
	require.True(t, factExistsOn(t, f.ri, out.ExperimentBranch, path))
	require.False(t, factExistsOn(t, f.ri, "agent/test", path))
	require.Equal(t, agentBefore, headOf(t, f.ri, "agent/test"), "the agent branch did not move")

	raw, err := f.svc.Facts().ReadFact(context.Background(), out.ExperimentBranch, path, nil)
	require.NoError(t, err)
	parsed, err := fact.ParseFact(path, raw.Content)
	require.NoError(t, err)
	require.Equal(t, "synthesis", string(parsed.Type))
	require.Equal(t, "authored", string(parsed.Origin))
	require.Equal(t, 0.63, parsed.Confidence)
	require.Equal(t, out.Run, parsed.Context["reason_run"])
	require.Len(t, parsed.Refs, 2)
	require.True(t, strings.HasSuffix(parsed.Refs[0], f.a) || strings.HasSuffix(parsed.Refs[1], f.a), parsed.Refs)
	require.True(t, strings.HasSuffix(parsed.Refs[0], f.b) || strings.HasSuffix(parsed.Refs[1], f.b), parsed.Refs)
	require.Equal(t, "Alpha holds only while beta is configured.", parsed.Title)
	tip := headOf(t, f.ri, out.ExperimentBranch)
	require.Contains(t, messageOf(t, f.ri, tip), "Knomit-Trace: "+out.Run)

	// the logs: start, one step line per host call, done — all keyed
	lines := buf.linesOf(t, out.Run)
	msgs := map[string]int{}
	for _, l := range lines {
		require.Equal(t, out.Run, l["reason_run"])
		msgs[l["message"].(string)]++
	}
	require.Equal(t, 1, msgs["reason start"])
	require.Equal(t, 1, msgs["reason done"])
	require.GreaterOrEqual(t, msgs["reason step"], 8, "query, 2x(explain, incoming), model, experiment, learn")
	require.Equal(t, 1, msgs["reason model request"])
	require.Equal(t, 1, msgs["reason model reply"])

	// decline: the asker rolls the experiment back and the answer is gone
	var rb mcpgo.CallToolRequest
	rb.Params.Arguments = map[string]any{"action": "rollback", "name": out.Experiment}
	rres, err := ExperimentHandler(nil)(f.ctx, rb)
	require.NoError(t, err)
	require.False(t, rres.IsError, resultText(t, rres))
	require.Empty(t, headOf(t, f.ri, out.ExperimentBranch), "the experiment branch is gone")
	require.Equal(t, agentBefore, headOf(t, f.ri, "agent/test"))
}

// A topic that declares no reason_run context gets its answer with NO
// context at all — the script never sends an undeclared key (no refused
// write, no Warn line) — and the run key is on the commit's Knomit-Trace.
// Sabotage: make contextFor always return {reason_run} (red: the write is
// refused and the run ends on a gap).
func TestReason_UndeclaredContextKeyNeverSent(t *testing.T) {
	buf := &reasonLogBuf{}
	prev := log.Logger
	log.Logger = zerolog.New(buf).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = prev })
	f := newReasonFixture(t, strings.Replace(reasonOntologyYAML,
		`    context:
      reason_run: {type: string, pattern: "^reason-[0-9a-f]{8}$"}
`, "", 1))
	res := f.call(t, map[string]any{"question": "Does alpha match-target hold?"})
	var out repos.ReasonResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))
	require.Equal(t, "answer", out.Outcome, resultText(t, res))
	require.Len(t, out.AnswerFacts, 1)
	raw, err := f.svc.Facts().ReadFact(context.Background(), out.ExperimentBranch, out.AnswerFacts[0], nil)
	require.NoError(t, err)
	parsed, err := fact.ParseFact(out.AnswerFacts[0], raw.Content)
	require.NoError(t, err)
	require.Empty(t, parsed.Context)
	require.Contains(t, messageOf(t, f.ri, headOf(t, f.ri, out.ExperimentBranch)), "Knomit-Trace: "+out.Run)
	for _, l := range buf.linesOf(t, out.Run) {
		require.NotEqual(t, "warn", l["level"], "no refused call: %v", l)
	}
}

// A write the topic's rules refuse ends the run on a gap naming the refusal,
// with the fact returned unwritten — never a silent success. Here the
// answer's topic refuses the synthesis type; the gap's hypothesis is then
// the one fact written.
func TestReason_RefusedWriteFallsBackToGap(t *testing.T) {
	f := newReasonFixture(t, strings.Replace(reasonOntologyYAML, `    attributes:
      learn_dedup: off
    context:`, `    attributes:
      learn_dedup: off
    validations:
      - name: no-synthesis-here
        rule: "fact.type !== 'synthesis'"
        message: "architecture takes no synthesis facts"
    context:`, 1))
	res := f.call(t, map[string]any{"question": "Does alpha match-target hold?"})
	var out repos.ReasonResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))
	require.Equal(t, "gap", out.Outcome, resultText(t, res))
	require.Len(t, out.UnwrittenFacts, 1)
	require.Equal(t, "synthesis", out.UnwrittenFacts[0].(map[string]any)["type"])
	require.NotEmpty(t, out.Gaps)
	require.Contains(t, out.Gaps[0].Reason, "the write was refused")
	require.Contains(t, out.Gaps[0].Reason, "no-synthesis-here")
	require.Len(t, out.AnswerFacts, 1, "the gap's hypothesis")
	raw, err := f.svc.Facts().ReadFact(context.Background(), out.ExperimentBranch, out.AnswerFacts[0], nil)
	require.NoError(t, err)
	parsed, err := fact.ParseFact(out.AnswerFacts[0], raw.Content)
	require.NoError(t, err)
	require.Equal(t, "hypothesis", string(parsed.Type))
	require.Equal(t, out.Run, parsed.Context["reason_run"])
}

// The deadline is the asker's budget and nothing else: a model that takes
// five seconds ends a one-second run at about one second, interrupted, with
// no answer written. Sabotage: ignore `budget` in the handler (red: the run
// waits for the model and answers).
func TestReason_DeadlineComesFromBudget(t *testing.T) {
	f := newReasonFixture(t, reasonOntologyYAML)
	f.delay = 5 * time.Second
	t0 := time.Now()
	res := f.call(t, map[string]any{"question": "Does alpha match-target hold?", "budget": 1})
	elapsed := time.Since(t0)
	require.False(t, res.IsError, resultText(t, res))
	var out repos.ReasonResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))
	require.Equal(t, "interrupted", out.Outcome, resultText(t, res))
	require.Less(t, elapsed, 4*time.Second)
	require.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
	require.Empty(t, out.AnswerFacts)
	require.Empty(t, out.Experiment)
	require.NotEmpty(t, out.Chain, "the host's chain so far comes back")
}

// Unknown argument keys are refused before anything runs; a bad budget and a
// missing question are refused too.
func TestReason_ArgumentsValidated(t *testing.T) {
	f := newReasonFixture(t, reasonOntologyYAML)
	res := f.call(t, map[string]any{"question": "q", "scope": "kb/architecture"})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), `unknown argument "scope" for knomit_reason`)

	res = f.call(t, map[string]any{"question": "q", "budget": -3})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "positive number of seconds")

	res = f.call(t, map[string]any{"question": "  "})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "needs a `question`")
	require.Equal(t, 0, f.requests, "no model call was made")
}

// knomit_reason is registered, gated, a write tool, and declares exactly
// question, budget and binding.
func TestReason_Registration(t *testing.T) {
	for _, r := range toolRegistrations(nil) {
		if r.tool.Name != "knomit_reason" {
			continue
		}
		require.True(t, r.write)
		require.Equal(t, gateRequired, r.gate)
		var keys []string
		for k := range r.tool.InputSchema.Properties {
			keys = append(keys, k)
		}
		require.ElementsMatch(t, []string{"question", "budget", "binding"}, keys)
		require.Equal(t, []string{"question"}, r.tool.InputSchema.Required)
		return
	}
	t.Fatal("knomit_reason is not registered")
}

// deadlineTools records the deadline each tool call's ctx carries.
type deadlineTools struct {
	inner repos.ScriptTools
	mu    sync.Mutex
	seen  []time.Duration // deadline minus the time of the call
}

func (d *deadlineTools) Call(ctx context.Context, tool string, args map[string]any) (string, bool, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.mu.Lock()
		d.seen = append(d.seen, time.Until(dl))
		d.mu.Unlock()
	} else {
		d.mu.Lock()
		d.seen = append(d.seen, -1)
		d.mu.Unlock()
	}
	return d.inner.Call(ctx, tool, args)
}

// The deadline comes ONLY from budget: with budget 600 s, the first tool
// call (the gather's query) sees a deadline about 600 s away — so any
// handler-side WithTimeout shorter than the budget fails this. The handler
// adds its own deadline to the tool ctx only through the budget. Sabotage: wrap
// ReasonHandler's ctx in context.WithTimeout(ctx, time.Second) (red: ~1 s).
func TestReason_DeadlineIsExactlyTheBudget(t *testing.T) {
	f := newReasonFixture(t, reasonOntologyYAML)
	rec := &deadlineTools{inner: NewScriptTools(reasonEmbedder{})}
	f.eng = repos.NewEngine(repos.EngineConfig{
		Model: config.EngineConfig{ModelURL: f.model.URL + "/v1", Model: "lfm-test"},
		Tools: rec, Home: t.TempDir(),
	})
	res := f.call(t, map[string]any{"question": "Does alpha match-target hold?", "budget": 600})
	require.False(t, res.IsError, resultText(t, res))
	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.NotEmpty(t, rec.seen)
	first := rec.seen[0]
	require.Greater(t, first, 590*time.Second, "the tool ctx deadline is the budget's, got %s", first)
	require.LessOrEqual(t, first, 600*time.Second)
}
