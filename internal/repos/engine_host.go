// The engine tier's host object (E02 §11): exactly six functions — query,
// explain and learn (the script host's own closures, toolHost), and incoming,
// experiment and model (new). Every call is wrapped by Go so that one
// structured log line per call carries the run key (E02 §12): the script
// passes a small `log` object on each call ({step, state, move, judgment,
// posterior}) and needs no log function of its own.
package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/platform/crashdump"
	"knomit/internal/store"
)

// engineHost is the `knomit` object of one engine run.
type engineHost struct {
	*toolHost
	e     *Engine
	runID string
	t0    time.Time
	// base is the run ctx WITHOUT a binding: the budget deadline and the
	// trailers. The tool ctx is base plus the current binding.
	base  context.Context
	world *Binding // the asker's world
	cur   *Binding // where the run reads and writes now: world, or the experiment
	// experiment is the one this run opened ("" before); opened names every
	// experiment the run opened, the only ones its rollback may discard.
	experiment string
	opened     map[string]bool
	written    []string
	modelCalls int

	mu    sync.Mutex
	chain []ReasonStep
}

func newEngineHost(e *Engine, runCtx context.Context, world *Binding, runID string, t0 time.Time) *engineHost {
	// The run's writes carry its key as their Knomit-Trace, set once here; a
	// script cannot pass its own trace (toolHost.learn refuses opts.trace).
	base := store.WithTrailers(runCtx, store.Trailers{Trace: runID})
	h := &engineHost{e: e, runID: runID, t0: t0, base: base, world: world, cur: world, opened: map[string]bool{}}
	ri := world.Write()
	h.toolHost = &toolHost{
		tools: e.cfg.Tools, ctx: WithBinding(base, world), root: ri.ontologyRoot, moment: "reason:" + runID,
		onPanic: func(tool string, r any) { crashdump.ReportRecovered("engine:"+runID+":"+tool, r) },
	}
	return h
}

// functions is the engine tier: exactly these six names. No exec (the model
// is a host-made HTTP call, nothing here starts a process), and no emit,
// push, run, update or retract.
func (h *engineHost) functions() fact.ScriptHost {
	return fact.ScriptHost{
		"query":      h.engQuery,
		"explain":    h.engExplain,
		"learn":      h.engLearn,
		"incoming":   h.engIncoming,
		"experiment": h.engExperiment,
		"model":      h.engModel,
	}
}

// setBinding moves the run onto b: every later query, explain, incoming and
// learn reads and writes there.
func (h *engineHost) setBinding(b *Binding) {
	h.cur = b
	h.toolHost.ctx = WithBinding(h.base, b)
}

// callLog is the script's `log` object for one call: the loop's state, which
// only the script knows.
type callLog struct {
	Step      int      `json:"step"`
	State     string   `json:"state"`
	Move      string   `json:"move"`
	From      string   `json:"from"`
	Arg       string   `json:"arg"`
	Judgment  string   `json:"judgment"`
	Posterior *float64 `json:"posterior"`
	// Facts are the paths a judgment is about (a model call touches no fact
	// by itself, so the script names them); a read's facts come from its
	// result instead.
	Facts []string `json:"facts"`
}

// display is the move as one log token: name(from|arg).
func (l callLog) display(fallback string) string {
	if l.Move == "" {
		return fallback
	}
	switch {
	case l.From != "":
		return l.Move + "(" + l.From + ")"
	case l.Arg != "":
		return l.Move + "(" + l.Arg + ")"
	}
	return l.Move
}

// takeLog removes `log` from an options object (so the tool never sees an
// argument it does not declare) and decodes it.
func takeLog(opts map[string]any) callLog {
	var l callLog
	raw, ok := opts["log"]
	if !ok {
		return l
	}
	delete(opts, "log")
	if b, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(b, &l)
	}
	return l
}

// withLogOpts returns args with the options object at i replaced by a copy
// that has no `log` key, and the decoded log.
func withLogOpts(args []any, i int) ([]any, callLog) {
	out := make([]any, len(args))
	copy(out, args)
	opts := objectArg(args, i)
	l := takeLog(opts)
	if i < len(out) {
		out[i] = opts
	} else if len(opts) > 0 {
		for len(out) < i {
			out = append(out, nil)
		}
		out = append(out, opts)
	}
	return out, l
}

// stepLine writes the one Info line of a call (E02 §12 shape) and records it
// in the chain kept for an interrupted run.
func (h *engineHost) stepLine(l callLog, move, judgment, verdict string, facts []string, readCount int, callErr error) {
	move = l.display(move)
	if judgment == "" {
		judgment = l.Judgment
	}
	if len(facts) == 0 && len(l.Facts) > 0 {
		facts = l.Facts
	}
	shown := facts
	if len(shown) > 5 {
		shown = shown[:5]
	}
	var ev *zerolog.Event
	if callErr != nil {
		ev = log.Warn().Str("error", capForLog(callErr.Error()))
	} else {
		ev = log.Info()
	}
	ev = ev.Str("reason_run", h.runID).Int("step", l.Step).Str("state", l.State).Str("move", capForLog(move)).
		Strs("facts", shown).Int("facts_read", readCount)
	if judgment != "" {
		ev = ev.Str("judgment", judgment)
	}
	if verdict != "" {
		ev = ev.Str("verdict", capForLog(verdict))
	}
	if l.Posterior != nil {
		ev = ev.Float64("posterior", *l.Posterior)
	}
	ev.Int("model_calls", h.modelCalls).Int64("elapsed_ms", time.Since(h.t0).Milliseconds()).Msg("reason step")

	if callErr == nil {
		h.mu.Lock()
		name := l.Move
		if name == "" {
			name = move
		}
		h.chain = append(h.chain, ReasonStep{Step: l.Step, State: l.State, Move: name, From: l.From, Arg: l.Arg, Facts: shown,
			Judgment: judgment, Verdict: verdict, Posterior: l.Posterior})
		h.mu.Unlock()
	}
}

// chainSoFar is the chain the host logged, for a run that ended before the
// script could return its own.
func (h *engineHost) chainSoFar() []ReasonStep {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ReasonStep, len(h.chain))
	copy(out, h.chain)
	if out == nil {
		out = []ReasonStep{}
	}
	return out
}

// resultPaths collects fact paths from a tool result: the `file` of query rows
// and learn commits, the `path` of explain nodes and incoming refs.
func resultPaths(v any, key string, list string) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	rows, _ := m[list].([]any)
	var out []string
	for _, r := range rows {
		if rm, ok := r.(map[string]any); ok {
			if p, ok := rm[key].(string); ok && p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func (h *engineHost) engQuery(args []any) (any, error) {
	args, l := withLogOpts(args, 0)
	out, err := h.query(args)
	facts := resultPaths(out, "file", "facts")
	h.stepLine(l, "query", "", fmt.Sprintf("%d facts", len(facts)), facts, len(facts), err)
	return out, err
}

func (h *engineHost) engExplain(args []any) (any, error) {
	args, l := withLogOpts(args, 1)
	out, err := h.explain(args)
	facts := resultPaths(out, "path", "facts")
	file, _ := stringArg(args, 0)
	h.stepLine(l, "refs_out("+file+")", "", fmt.Sprintf("%d nodes", len(facts)), facts, len(facts), err)
	return out, err
}

// engLearn is the script host's learn behind two engine-only guards: it
// writes ONLY inside an experiment this run opened (never onto the asker's
// world), and it refuses opts.retract, so the tier cannot delete facts by the
// move form either — it has no update or retract, and only ADDS answer facts.
func (h *engineHost) engLearn(args []any) (any, error) {
	args, l := withLogOpts(args, 1)
	if h.experiment == "" || !h.opened[h.experiment] {
		err := errors.New("knomit.learn: the engine writes only inside an experiment this run opened; call knomit.experiment({action: \"open\", name}) first")
		h.stepLine(l, "learn", "", "", nil, 0, err)
		return nil, err
	}
	if _, has := objectArg(args, 1)["retract"]; has {
		err := errors.New("knomit.learn: opts.retract is refused: the engine tier only adds facts, it never deletes them")
		h.stepLine(l, "learn", "", "", nil, 0, err)
		return nil, err
	}
	out, err := h.learn(args)
	files := resultPaths(out, "file", "commits")
	if err == nil {
		h.written = append(h.written, files...)
	}
	h.stepLine(l, "learn", "", fmt.Sprintf("%d written on %s", len(files), h.cur.WriteBranch()), files, 0, err)
	return out, err
}

// engIncoming is incoming(path, {commit?}): "cited by" — every
// (path, commit) whose ref to this path resolved to this version when it was
// written. A binding over the store's IncomingAtCommit, the call the REST
// route /commits/{sha}/facts/*/incoming makes; commit defaults to the
// current branch's head.
func (h *engineHost) engIncoming(args []any) (any, error) {
	args, l := withLogOpts(args, 1)
	file, ok := stringArg(args, 0)
	if !ok || file == "" {
		err := errors.New("knomit.incoming(path, {commit?}): path must be a string")
		h.stepLine(l, "refs_in(?)", "", "", nil, 0, err)
		return nil, err
	}
	opts := objectArg(args, 1)
	commit, _ := opts["commit"].(string)
	move := "refs_in(" + file + ")"
	refs, branch, err := h.incoming(file, commit)
	var paths []string
	rows := make([]any, 0, len(refs))
	for _, r := range refs {
		paths = append(paths, r.Path)
		rows = append(rows, map[string]any{
			"path": r.Path, "title": r.Title, "type": r.Type, "commit": r.Commit, "deleted": r.Deleted,
		})
	}
	h.stepLine(l, move, "", fmt.Sprintf("%d citers", len(refs)), paths, len(refs), err)
	if err != nil {
		return nil, err
	}
	return map[string]any{"file": file, "branch": branch, "refs": rows}, nil
}

// incoming resolves the read branch (the write repo's mount in the current
// binding) and its head, then asks the store.
func (h *engineHost) incoming(file, commit string) (refs []store.RefSummary, branch string, err error) {
	t0 := time.Now()
	defer func() {
		h.spent += time.Since(t0)
		if r := recover(); r != nil {
			crashdump.ReportRecovered("engine:"+h.runID+":incoming", r)
			refs, err = nil, fmt.Errorf("knomit.incoming: panic: %v", r)
		}
	}()
	ri := h.cur.Write()
	branch = h.cur.WriteMountBranch()
	file = fact.NormalizePath(h.root, file)
	ctx := h.toolHost.ctx
	if werr := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errors.New("knomit.incoming: store unavailable")
			return
		}
		if commit == "" {
			commit, err = svc.Branches().HeadCommit(ctx, branch)
			if err != nil {
				err = fmt.Errorf("knomit.incoming: head of %s: %w", branch, err)
				return
			}
		}
		refs, err = svc.GraphStore().IncomingAtCommit(ctx, branch, file, commit)
	}); werr != nil {
		return nil, branch, fmt.Errorf("knomit.incoming: %w", werr)
	}
	if err != nil {
		return nil, branch, err
	}
	return refs, branch, nil
}

// engExperiment is experiment({action, name, description?}):
//
//   - open: forks exp/<name> from the write repo's agent branch and moves the
//     RUN onto it (reads and writes), never the asker's handle; an existing
//     experiment of that name is refused rather than resumed, so a run never
//     writes into somebody else's work.
//   - rollback: discards an experiment THIS run opened (default: the current
//     one) and moves the run back onto the asker's world.
//   - list: this repo's experiments, marking the run's.
//
// commit is deliberately absent: accepting the answer is the asker's
// decision (E02 §7).
func (h *engineHost) engExperiment(args []any) (out any, err error) {
	args, l := withLogOpts(args, 0)
	// A Go panic inside the call is recovered HERE, as every host call's is
	// (kb/architecture/fact/ontology/triggers/script-sandbox): the script
	// gets an Error and the run its Warn line, never a crashed dispatcher.
	defer func() {
		if r := recover(); r != nil {
			crashdump.ReportRecovered("engine:"+h.runID+":experiment", r)
			out, err = nil, fmt.Errorf("knomit.experiment: panic: %v", r)
			h.stepLine(l, "experiment", "", "", nil, 0, err)
		}
	}()
	opts := objectArg(args, 0)
	action, _ := opts["action"].(string)
	name, _ := opts["name"].(string)
	desc, _ := opts["description"].(string)
	switch action {
	case "open":
		out, err = h.experimentOpen(name, desc)
	case "rollback":
		if name == "" {
			name = h.experiment
		}
		out, err = h.experimentRollback(name)
	case "list":
		out, err = h.experimentList()
	case "commit":
		err = errors.New("knomit.experiment: commit is not available to the engine: accepting the answer is the asker's decision (knomit_experiment {action: \"commit\"})")
	default:
		err = fmt.Errorf("knomit.experiment({action, name}): action must be one of open, rollback, list, got %q", action)
	}
	h.stepLine(l, "experiment("+action+" "+name+")", "", "", nil, 0, err)
	return out, err
}

func (h *engineHost) experimentOpen(name, desc string) (any, error) {
	if name == "" {
		return nil, errors.New("knomit.experiment open needs a name")
	}
	if h.experiment != "" {
		return nil, fmt.Errorf("knomit.experiment: this run is already inside experiment %q", h.experiment)
	}
	if !h.world.WriteOK() {
		return nil, fmt.Errorf("knomit.experiment: the world %s is a read-only view; no experiment can be opened", h.world.Name())
	}
	ri := h.world.Write()
	if ri.Subscribed() {
		return nil, fmt.Errorf("knomit.experiment: repo %q is a subscription and has no agent branch to fork an experiment from", ri.Name())
	}
	if oerr := ri.OntologyError(); oerr != nil {
		return nil, fmt.Errorf("knomit.experiment: repo %q has no established ontology (%v)", ri.Name(), oerr)
	}
	parent := ri.AgentBranch()
	if !ri.WritableBranch(parent) {
		return nil, fmt.Errorf("knomit.experiment: repo %q cannot author on its agent branch %q", ri.Name(), parent)
	}
	svc, release, err := ri.Acquire()
	if err != nil {
		return nil, fmt.Errorf("knomit.experiment: %w", err)
	}
	defer release()
	ctx := h.base
	if _, exists, gerr := svc.Experiments().GetExperiment(ctx, name); gerr != nil {
		return nil, fmt.Errorf("knomit.experiment: %w", gerr)
	} else if exists {
		return nil, fmt.Errorf("knomit.experiment: an experiment named %q already exists; the engine opens only new ones", name)
	}
	exp, err := svc.Experiments().OpenExperiment(ctx, name, desc, parent)
	if err != nil {
		return nil, fmt.Errorf("knomit.experiment: %w", err)
	}
	h.experiment = exp.Name
	h.opened[exp.Name] = true
	h.setBinding(NewBindingOfRepo(ri, exp.Branch()))
	return map[string]any{"name": exp.Name, "branch": exp.Branch(), "parent": exp.Parent, "fork_commit": exp.ForkCommit}, nil
}

// experimentExists reports whether the experiment this run opened is still
// there. Any session can commit or roll it back while the run is still
// writing its result, so Engine.Run asks before it reports the answer as
// pending the asker's decision. A failed lookup is reported as existing: the
// check exists to stop a false "pending", not to invent a missing one.
func (h *engineHost) experimentExists() bool {
	if h.experiment == "" {
		return false
	}
	ri := h.world.Write()
	svc, release, err := ri.Acquire()
	if err != nil {
		return true
	}
	defer release()
	// The run ctx may be past its deadline here; the lookup is bounded on
	// its own.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, exists, err := svc.Experiments().GetExperiment(ctx, h.experiment)
	return err != nil || exists
}

func (h *engineHost) experimentRollback(name string) (any, error) {
	if name == "" || !h.opened[name] {
		return nil, fmt.Errorf("knomit.experiment: rollback discards only an experiment this run opened; %q is not one", name)
	}
	ri := h.world.Write()
	svc, release, err := ri.Acquire()
	if err != nil {
		return nil, fmt.Errorf("knomit.experiment: %w", err)
	}
	defer release()
	if err := svc.Experiments().RollbackExperiment(h.base, name); err != nil {
		return nil, fmt.Errorf("knomit.experiment: %w", err)
	}
	delete(h.opened, name)
	if h.experiment == name {
		h.experiment = ""
		h.written = nil
		h.setBinding(h.world)
	}
	return map[string]any{"name": name, "rolled_back": true}, nil
}

func (h *engineHost) experimentList() (any, error) {
	ri := h.world.Write()
	svc, release, err := ri.Acquire()
	if err != nil {
		return nil, fmt.Errorf("knomit.experiment: %w", err)
	}
	defer release()
	all, err := svc.Experiments().ListExperiments(h.base)
	if err != nil {
		return nil, fmt.Errorf("knomit.experiment: %w", err)
	}
	rows := make([]any, 0, len(all))
	for _, e := range all {
		rows = append(rows, map[string]any{"name": e.Name, "branch": e.Branch(), "parent": e.Parent, "current": e.Name == h.experiment})
	}
	return map[string]any{"experiments": rows}, nil
}
