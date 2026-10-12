// The reasoning engine (expert spike, E02): a goja script — reason.js,
// embedded beside this file — run on the SAME sandbox as trigger scripts and
// recipes (fact.RunScriptResult), entered on demand by one blocking Go call,
// Engine.Run, which the MCP tool knomit_reason wraps. The harness (the
// script) owns the loop's mechanics; the model owns every judgment inside it,
// reached through the engine tier's `model` host function.
//
// It lives in internal/repos, beside the script host it extends, and not in a
// package of its own: the engine tier reuses the script host's tool closures
// (toolHost) and the Binding type, both of which are this package's, and the
// project adds no directory under internal/.
//
// The engine tier is a THIRD host map, next to the script map and the recipe
// map: exactly query, explain, learn, incoming, experiment and model
// (engineHost.functions). It has no exec — the model is an HTTP call made by
// the host, so nothing in the engine starts a process — and no emit, push,
// run, update or retract.
package repos

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

// embeddedReasonJS is the engine's loop as shipped in this binary.
//
//go:embed engine_reason.js
var embeddedReasonJS string

// DefaultReasonBudget is the wall-clock budget of one run when the asker
// gives none (E02 §10). It is a request property with a default, not a
// corpus constant: the asker sets it per question.
const DefaultReasonBudget = 10 * time.Minute

// EngineConfig is what an Engine needs: the model endpoint (server
// configuration, never a script argument), the in-process tool set the
// script host already uses, and knomit's home for the reason.js override.
type EngineConfig struct {
	Model config.EngineConfig
	Tools ScriptTools
	// Home is knomit's home directory; <Home>/engine/reason.js, when it
	// exists, replaces the embedded script (development and gating tests).
	Home string
	// HTTPClient calls the model; nil means a client with no timeout of its
	// own (the run's ctx bounds every call).
	HTTPClient *http.Client
}

// Engine runs reasoning questions. One Engine serves one model endpoint, and
// runs one question at a time against it (E02 §10: there is one CPU model
// server; a second concurrent call waits for the first, bounded by its own
// ctx).
type Engine struct {
	cfg  EngineConfig
	http *http.Client
	slot chan struct{}
}

// NewEngine builds an engine over cfg.
func NewEngine(cfg EngineConfig) *Engine {
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{}
	}
	if cfg.Model.ModelURL == "" {
		cfg.Model.ModelURL = config.DefaultEngineModelURL
	}
	return &Engine{cfg: cfg, http: c, slot: make(chan struct{}, 1)}
}

// ReasonStep is one entry of the chain: what the loop did at a step. The move
// is named the way the scenario format's gold chain names one
// (internal/synthesize/scenario.go, GoldMove {move, from, arg}): Move is the
// move's name (neighbours, refs_out, refs_in, …), From the fact path a
// fact-anchored move starts from, Arg the argument of the others ("question"
// for the gather). Support is the step's per-fact support verdicts, keyed by
// fact path, in GoldStep.Support's vocabulary (supports, contradicts,
// restates, unrelated), so a grader compares a run step to a gold step field
// by field.
type ReasonStep struct {
	Step      int               `json:"step"`
	State     string            `json:"state"`
	Move      string            `json:"move,omitempty"`
	From      string            `json:"from,omitempty"`
	Arg       string            `json:"arg,omitempty"`
	Facts     []string          `json:"facts,omitempty"`
	Judgment  string            `json:"judgment,omitempty"`
	Verdict   string            `json:"verdict,omitempty"`
	Support   map[string]string `json:"support,omitempty"`
	Posterior *float64          `json:"posterior,omitempty"`
}

// ReasonGap is a structured gap (E02 §8): returned to the asker, not written.
type ReasonGap struct {
	Question       string   `json:"question"`
	Missing        string   `json:"missing,omitempty"`
	BestPartial    string   `json:"best_partial,omitempty"`
	Posterior      float64  `json:"posterior"`
	Tried          []string `json:"tried,omitempty"`
	Contradictions []string `json:"contradictions,omitempty"`
	Reason         string   `json:"reason,omitempty"`
}

// ReasonResult is what one run returns, by value; nothing re-reads it from
// anywhere. AnswerFacts and Experiment are recorded by the HOST from the
// writes and the experiment it actually performed, never taken from the
// script's own account.
type ReasonResult struct {
	Run              string       `json:"reason_run"`
	Outcome          string       `json:"outcome"` // answer | gap (the scenario format's OutcomeLabels) | interrupted | error
	Question         string       `json:"question"`
	Repo             string       `json:"repo"`
	Branch           string       `json:"branch"`
	Experiment       string       `json:"experiment,omitempty"`
	ExperimentBranch string       `json:"experiment_branch,omitempty"`
	AnswerFacts      []string     `json:"answer_facts"`
	ExistingAnswer   string       `json:"existing_answer,omitempty"`
	Posterior        float64      `json:"posterior"`
	Chain            []ReasonStep `json:"chain"`
	Gaps             []ReasonGap  `json:"gaps,omitempty"`
	UnwrittenFacts   []any        `json:"unwritten_facts,omitempty"`
	ModelCalls       int          `json:"model_calls"`
	ElapsedMS        int64        `json:"elapsed_ms"`
	Error            string       `json:"error,omitempty"`
	Next             string       `json:"next,omitempty"`
}

// scriptReturn is the shape reason.js returns.
type scriptReturn struct {
	Outcome        string       `json:"outcome"`
	Posterior      float64      `json:"posterior"`
	Chain          []ReasonStep `json:"chain"`
	Gaps           []ReasonGap  `json:"gaps"`
	ExistingAnswer string       `json:"existing_answer"`
	UnwrittenFacts []any        `json:"unwritten_facts"`
}

// newReasonRunID mints the run key every log line carries: `reason-` and 8
// lowercase hex (E02 §12). It is distinct from a recipe's run id and is also
// written into the answer facts' context and their Knomit-Trace trailer.
func newReasonRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "reason-" + hex.EncodeToString(b[:])
}

// loadReasonScript is <home>/engine/reason.js when present, else the embedded
// copy. source names which one ran, for the start line.
func (e *Engine) loadReasonScript() (prog *goja.Program, source string, err error) {
	src, source := embeddedReasonJS, "embedded"
	if e.cfg.Home != "" {
		p := filepath.Join(e.cfg.Home, "engine", "reason.js")
		if b, rerr := os.ReadFile(p); rerr == nil {
			src, source = string(b), p
		} else if !errors.Is(rerr, os.ErrNotExist) {
			return nil, "", fmt.Errorf("engine: read %s: %w", p, rerr)
		}
	}
	prog, err = fact.CompileScript("reason.js", src)
	if err != nil {
		return nil, source, fmt.Errorf("engine: reason.js (%s) does not compile: %w", source, err)
	}
	return prog, source, nil
}

// Run answers question in world, blocking until the loop ends or budget
// runs out. budget <= 0 means DefaultReasonBudget. The deadline is the
// budget's alone: the caller's ctx is honoured for cancellation, and nothing
// here wraps the run in a handler-sized timeout.
//
// Writes go into an experiment the script opens on world's write repo; the
// experiment is never committed here (accept is the asker's). A run that ends
// on its deadline or on a cancelled ctx returns outcome "interrupted" with
// whatever the host recorded so far, and no Go error.
func (e *Engine) Run(ctx context.Context, world *Binding, question string, budget time.Duration) (ReasonResult, error) {
	if world == nil || world.Write() == nil {
		return ReasonResult{}, errors.New("engine: no world: the call carries no binding")
	}
	if budget <= 0 {
		budget = DefaultReasonBudget
	}
	t0 := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	id := newReasonRunID()
	ri := world.Write()
	res := ReasonResult{Run: id, Question: question, Repo: ri.Name(), Branch: world.WriteMountBranch(), AnswerFacts: []string{}, Chain: []ReasonStep{}}

	// One run at a time per model endpoint; the wait counts against the
	// budget, so a queued run cannot outlive its own deadline.
	select {
	case e.slot <- struct{}{}:
		defer func() { <-e.slot }()
	case <-runCtx.Done():
		res.Outcome, res.Error = "interrupted", "budget ran out while waiting for the model endpoint (another run holds it)"
		res.ElapsedMS = time.Since(t0).Milliseconds()
		return res, nil
	}

	prog, source, err := e.loadReasonScript()
	if err != nil {
		return ReasonResult{}, err
	}

	h := newEngineHost(e, runCtx, world, id, t0)
	log.Info().Str("reason_run", id).Str("world", ri.Name()+"@"+res.Branch).Str("question", capForLog(question)).
		Dur("budget", budget).Str("script", source).Msg("reason start")

	ont := ri.Ontology()
	globals := map[string]any{
		"ontology": map[string]any{"context": declaredContextKeys(ont)},
		"question": question,
		"run": map[string]any{
			"id": id, "budget_ms": budget.Milliseconds(),
		},
		"world": map[string]any{
			"repo": ri.Name(), "repo_id": ri.ShortID(), "branch": res.Branch, "writable": world.WriteOK(),
		},
	}
	out, runErr := fact.RunScriptResult(runCtx, prog, "engine reason.js", globals, h.functions(), time.Now().UTC())

	// What the host saw is authoritative for the writes and the world.
	res.Experiment, res.AnswerFacts, res.ModelCalls = h.experiment, h.written, h.modelCalls
	if h.experiment != "" {
		res.ExperimentBranch = store.ExperimentBranch(h.experiment)
	}
	if res.AnswerFacts == nil {
		res.AnswerFacts = []string{}
	}

	var interrupted *goja.InterruptedError
	switch {
	case runErr == nil:
		var sr scriptReturn
		b, _ := json.Marshal(out)
		if jerr := json.Unmarshal(b, &sr); jerr != nil || sr.Outcome == "" {
			res.Outcome, res.Error = "error", fmt.Sprintf("reason.js returned no outcome (%v)", out)
			break
		}
		res.Outcome, res.Posterior, res.Gaps = sr.Outcome, sr.Posterior, sr.Gaps
		res.ExistingAnswer, res.UnwrittenFacts = sr.ExistingAnswer, sr.UnwrittenFacts
		if sr.Chain != nil {
			res.Chain = sr.Chain
		}
	case errors.As(runErr, &interrupted) || runCtx.Err() != nil:
		res.Outcome = "interrupted"
		res.Error = fmt.Sprintf("the run ended before the loop did (budget %s): %v", budget, runErr)
		res.Chain = h.chainSoFar()
	default:
		res.Outcome, res.Error = "error", runErr.Error()
		res.Chain = h.chainSoFar()
	}
	if res.Experiment != "" {
		res.Next = fmt.Sprintf("The answer facts are on experiment %q (branch %s) and nothing has been committed. "+
			"Read them with knomit_explain; accept with knomit_experiment {action: \"commit\", name: %q}, "+
			"or decline with knomit_experiment {action: \"rollback\", name: %q}.",
			res.Experiment, res.ExperimentBranch, res.Experiment, res.Experiment)
	}
	res.ElapsedMS = time.Since(t0).Milliseconds()

	ev := log.Info()
	if res.Outcome == "error" || res.Outcome == "interrupted" {
		ev = log.Warn().Str("error", capForLog(res.Error))
	}
	ev.Str("reason_run", id).Str("outcome", res.Outcome).Int("answer_facts", len(res.AnswerFacts)).
		Str("experiment", res.Experiment).Float64("posterior", res.Posterior).Int("model_calls", res.ModelCalls).
		Int64("elapsed_ms", res.ElapsedMS).Msg("reason done")
	return res, nil
}

// declaredContextKeys is, for every topic path the ontology declares (topics
// and their children, lowercased, "/"-joined), the context keys a fact there
// may carry — Ontology.ContextSpec's answer, which already folds in every
// parent's declarations. A path with none is absent. The script resolves a
// fact's topic/category against it the way ContextSpec walks (deepest
// declared prefix), so it puts a key into `context` ONLY where the ontology
// declares it: a topic with no declaration refuses every key, and no root
// block exists to fall back on.
func declaredContextKeys(o *fact.Ontology) map[string]any {
	out := map[string]any{}
	if o == nil {
		return out
	}
	var walk func(prefix string, nodes map[string]*fact.OntologyNode)
	walk = func(prefix string, nodes map[string]*fact.OntologyNode) {
		for name, n := range nodes {
			p := strings.ToLower(name)
			if prefix != "" {
				p = prefix + "/" + p
			}
			if spec := o.ContextSpec(p); len(spec) > 0 {
				keys := make([]string, 0, len(spec))
				for k := range spec {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				out[p] = keys
			}
			if n != nil {
				walk(p, n.Children)
			}
		}
	}
	walk("", o.Topics)
	return out
}
