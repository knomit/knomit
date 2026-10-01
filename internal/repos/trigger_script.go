// The `do: script` half of the trigger dispatcher (F07 PR 3): the script
// cache, the `knomit` host object, the per-minute rate cap and the WARN-once
// maps. Phase A loads the scripts of the head (loadScripts); phase B runs one
// fire (runScript) OUTSIDE the store, exactly where `if` runs.
//
// A script's writes are REAL writes on this machine's agent branch through the
// UNCHANGED MCP handlers (learn, update, retract, query, explain), reached
// through the injected ScriptTools — internal/mcp imports internal/repos, so
// the handlers cannot be called from here directly; internal/app wires
// mcp.NewScriptTools into Deps. The host builds the ctx those handlers see:
//
//   - the budget as its deadline (context.WithTimeout(runCtx, budget)): a
//     handler's own WithTimeout (60 s learn, 30 s update/retract) derives from
//     the EARLIER deadline, so no host call outlives the budget by more than a
//     write already inside the branch lock (notifyCommit drops cancellation);
//     the same ctx's Done() arms the VM interrupt, so budget expiry OR the
//     dispatcher's cancel (stop()) interrupts the JavaScript within the
//     interrupt latency;
//   - the binding: NewBindingOfRepo(ri, agent branch) — the lens-of-one every
//     direct handler call uses. No argument names a branch, an author or a
//     key: author and committer derive from the branch name, the signer is
//     the store's, so a script writes as THIS machine, never as the author
//     whose change fired it; an experiment a session has open is never
//     pinned (the script writes the agent branch);
//   - the trailer set (store.WithTrailers): Knomit-Trace = the fire's derived
//     trace, Knomit-Cause = the firing commit, Knomit-Trigger = this
//     trigger's name, set ONCE per fire — a script cannot pass its own.
//
// Sequencing: phase B holds no Acquire; a host call takes one for its own
// duration (storeIndices) and the write takes the branch lock like any MCP
// write. The write's notifyCommit kicks the dispatcher (a non-blocking send
// on the 1-slot channel), which is received by settle AFTER this run: the
// script's commit is the NEXT advance, never a run inside a run. A SwapStore
// mid-script drains only that host call's Acquire, never the JavaScript (it
// holds none); a host call after the swap fails with "unavailable", thrown to
// the script, and the fire is `script-error`.
//
// The one place a script is denied what a session may do: `.knomit/` is
// writable job state for the MCP tools (.knomit/<area>/…) and never for a
// script (ruling R7), so the host refuses a `path` key on any learn fact and
// any private path on update/retract BEFORE the handler sees the call.
package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/crashdump"
	"knomit/internal/store"
)

// ScriptTools is the injected in-process tool set a script's host calls:
// tool is the MCP tool name (learn, update, retract, query, explain), args its
// arguments, text the tool's result text (JSON for the write and read tools),
// isError the tool's own refusal (validation, dedup, refs gate, CAS, private
// path, unavailable store), err a failure to call at all. The ctx carries the
// binding, the trailers and the budget's deadline; an implementation must pass
// it through unchanged.
type ScriptTools interface {
	Call(ctx context.Context, tool string, args map[string]any) (text string, isError bool, err error)
}

// compiledScript is one trigger's script as loaded at the observed head,
// keyed by the trigger NAME in the dispatcher's cache and by the script BLOB
// for staleness: a different blob at the new head recompiles before the next
// fire, the same blob is reused, and a blob that fails to compile (or a
// missing file) keeps its error here — the trigger is `invalid` on the
// endpoint and every fire is a `script-error` until the head fixes it.
type compiledScript struct {
	script string // the script name
	blob   string // "" when the file is missing
	prog   *goja.Program
	err    error
}

// scriptState is the dispatcher's `do: script` state. Written by the
// dispatcher goroutine; the report reads scripts under d.mu.
type scriptState struct {
	tools    ScriptTools
	rate     int // script runs per trigger per minute
	scripts  map[string]*compiledScript
	compiles int // test hook
	// logged holds the trigger@blob keys already reported at ERROR (a
	// compile error or a missing file), so a broken script is logged once per
	// blob, not on every run; the key is replaced when the blob changes.
	logged map[string]bool
	// warned holds the WARN-once keys of phase B: a runtime error per
	// (trigger, blob, message), a timeout per (trigger, blob), the rate cap
	// per (trigger, blob) per cap episode, a host panic crashdump per
	// (trigger, blob, tool).
	warned map[string]bool
	// windows is the rate cap's sliding window per trigger: the run
	// instants (the run's clock) of the last minute's script runs.
	windows map[string][]time.Time
}

func newScriptState(tools ScriptTools, ratePerMinute int) scriptState {
	if ratePerMinute < 1 {
		// A Config that never went through Load (a literal in a test, an
		// embedding caller): the built-in default, never "unlimited".
		ratePerMinute = config.DefaultScriptRatePerMinute
	}
	return scriptState{
		tools: tools, rate: ratePerMinute,
		scripts: map[string]*compiledScript{}, logged: map[string]bool{}, warned: map[string]bool{},
		windows: map[string][]time.Time{},
	}
}

// loadScripts (phase A, with the store) brings the cache in line with the
// head for every ACTIVE `do: script` trigger: one tree-entry lookup per
// script per run, a compile only when the blob changed. A script commit is
// not an episode (`.knomit/` never appears in the diff), so this is the only
// way a changed script takes effect — on the next run, with no restart.
func (d *triggerDispatcher) loadScripts(ctx context.Context, tr store.TriggerIndex, head plumbing.Hash, set *fact.TriggerSet) {
	for _, ct := range set.Active {
		if ct.Do != fact.TriggerDoScript {
			continue
		}
		if ct.JS != "" {
			// Inline code (F08, R9): compiled with the ontology and cached by
			// source hash (fact.compileInlineJS); the program is installed
			// here keyed by that hash, so an unchanged `js` keeps its entry
			// and a changed one replaces it before the next fire. It cannot
			// be missing or fail to compile here: that made it invalid.
			d.mu.Lock()
			if cur := d.sc.scripts[ct.Name]; cur == nil || cur.blob != ct.JSKey || cur.prog != ct.JSProgram() {
				d.sc.scripts[ct.Name] = &compiledScript{script: ct.Name + ":js", blob: ct.JSKey, prog: ct.JSProgram()}
			}
			d.mu.Unlock()
			continue
		}
		blob, data, err := tr.ScriptAt(ctx, head, ct.Script)
		d.mu.Lock()
		cur := d.sc.scripts[ct.Name]
		if cur != nil && cur.script == ct.Script && cur.blob == blob && (err == nil) == (cur.err == nil) {
			d.mu.Unlock()
			continue
		}
		cs := &compiledScript{script: ct.Script, blob: blob}
		if err != nil {
			if errors.Is(err, store.ErrNoScriptAtCommit) {
				cs.err = fmt.Errorf("script %s: %s not found at the head", ct.Script, fact.TriggerScriptPath(ct.Script))
			} else {
				cs.err = fmt.Errorf("script %s: %w", ct.Script, err)
			}
		} else {
			d.sc.compiles++
			cs.prog, cs.err = fact.CompileScript(ct.Script, string(data))
			if cs.err != nil {
				cs.err = fmt.Errorf("script %s does not compile: %w", ct.Script, cs.err)
			}
		}
		d.sc.scripts[ct.Name] = cs
		key := ct.Name + "@" + blob
		logNow := cs.err != nil && !d.sc.logged[key]
		if cs.err != nil {
			d.sc.logged[key] = true
		}
		d.mu.Unlock()
		if logNow {
			log.Error().
				Str("repo", d.repo).
				Str("branch", d.branch).
				Str("trigger", capForLog(ct.Name)).
				Str("script", capForLog(ct.Script)).
				Str("blob", blob).
				Str("error", cs.err.Error()).
				Msg("invalid trigger script skipped")
		}
	}
}

// scriptFor is the cached script of a trigger (nil before its first load).
func (d *triggerDispatcher) scriptFor(name string) *compiledScript {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sc.scripts[name]
}

// scriptError is the compile/missing error the endpoint overlays on a
// `do: script` trigger ("" when the script is fine or not loaded yet).
func (d *triggerDispatcher) scriptError(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cs := d.sc.scripts[name]; cs != nil && cs.err != nil {
		return cs.err.Error()
	}
	return ""
}

// rateAllows applies the per-minute cap (ruling D-rate: per trigger, per
// minute, a sliding window in memory; fires beyond it are DROPPED, not
// delayed). now is the run's clock. Dispatcher goroutine only.
func (d *triggerDispatcher) rateAllows(name string, now time.Time) bool {
	w := d.sc.windows[name]
	cut := now.Add(-time.Minute)
	i := 0
	for i < len(w) && !w[i].After(cut) {
		i++
	}
	w = w[i:]
	if len(w) >= d.sc.rate {
		d.sc.windows[name] = w
		return false
	}
	d.sc.windows[name] = append(w, now)
	return true
}

// warnOnce reports whether key has not been warned yet, marking it.
func (d *triggerDispatcher) warnOnce(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sc.warned[key] {
		return false
	}
	d.sc.warned[key] = true
	return true
}

// runScript is phase B's action for a `do: script` trigger whose `if` held.
// It returns the fire's outcome and error text, the time spent inside host
// calls (knomit's own cost, not the script's), and the extra rows the script's
// knomit.run calls produced (their `started`/`busy`/`recipe-error` rows,
// logged after the fire's own). The caller checks the dispatcher ctx
// afterwards: a cancelled run has no outcome.
func (d *triggerDispatcher) runScript(ctx context.Context, rs *runState, p pendingFire, globals map[string]any) (outcome, errText string, hostMS time.Duration, extra []store.TriggerFire) {
	cs := d.scriptFor(p.trig.Name)
	if cs == nil {
		return store.TriggerOutcomeScriptError, fmt.Sprintf("script %s: not loaded", p.trig.Script), 0, nil
	}
	if cs.err != nil {
		return store.TriggerOutcomeScriptError, cs.err.Error(), 0, nil
	}
	if !d.rateAllows(p.trig.Name, rs.now) {
		if d.warnOnce("rate:" + p.trig.Name + "@" + cs.blob) {
			log.Warn().
				Str("repo", d.repo).
				Str("branch", d.branch).
				Str("trigger", capForLog(p.trig.Name)).
				Int("rate_per_minute", d.sc.rate).
				Msg("trigger script rate cap reached; further fires this minute are dropped")
		}
		return store.TriggerOutcomeRateLimited, fmt.Sprintf("rate cap: %d script runs per minute", d.sc.rate), 0, nil
	}
	// The cap episode ends with a run that goes through.
	d.mu.Lock()
	delete(d.sc.warned, "rate:"+p.trig.Name+"@"+cs.blob)
	d.mu.Unlock()

	budget := fact.ScriptEvalTimeout
	if h := currentTriggerHooks().scriptBudget; h > 0 {
		budget = h
	}
	hostCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	hostCtx = WithBinding(hostCtx, NewBindingOfRepo(d.ri, d.branch))
	hostCtx = store.WithTrailers(hostCtx, store.Trailers{Trace: p.trace, Cause: p.commit, Trigger: p.trig.Name})
	rf, rt := rs.fireRange()
	h := &scriptHost{d: d, p: p, cs: cs, ctx: hostCtx, globals: globals, rangeFrom: rf, rangeTo: rt}
	h.onRow = func(r store.TriggerFire) { h.rows = append(h.rows, r) }

	err := fact.RunScript(hostCtx, cs.prog, cs.script, globals, h.functions(), rs.now)
	hostMS = h.spent
	if ctx.Err() != nil {
		return "", "", hostMS, nil // the run is aborted; nothing is recorded
	}
	extra = h.rows
	var interrupted *goja.InterruptedError
	switch {
	case err == nil:
		return store.TriggerOutcomeRan, "", hostMS, extra
	case errors.As(err, &interrupted) || errors.Is(hostCtx.Err(), context.DeadlineExceeded):
		if d.warnOnce("timeout:" + p.trig.Name + "@" + cs.blob) {
			log.Warn().Str("repo", d.repo).Str("branch", d.branch).Str("trigger", capForLog(p.trig.Name)).
				Dur("budget", budget).Msg("trigger script exceeded its budget")
		}
		return store.TriggerOutcomeScriptTimeout, fmt.Sprintf("script %s exceeded %s", cs.script, budget), hostMS, extra
	default:
		msg := err.Error()
		if d.warnOnce("error:" + p.trig.Name + "@" + cs.blob + ":" + msg) {
			log.Warn().Str("repo", d.repo).Str("branch", d.branch).Str("trigger", capForLog(p.trig.Name)).
				Str("error", capForLog(msg)).Msg("trigger script failed")
		}
		return store.TriggerOutcomeScriptError, msg, hostMS, extra
	}
}

// scriptHost is the `knomit` object of one fire — a script's, or a recipe's
// (which adds `exec`, recipeHostFunctions).
type scriptHost struct {
	d     *triggerDispatcher
	p     pendingFire
	cs    *compiledScript
	ctx   context.Context // budget deadline + binding + trailers
	spent time.Duration   // time inside host calls (host_ms)
	// globals is the fire's fact/agent/change, handed on to a recipe that
	// knomit.run starts; rangeFrom/rangeTo the fire's range for its late row.
	globals            map[string]any
	rangeFrom, rangeTo string
	// onRow receives the rows knomit.run records: a script collects them
	// into rows (logged with its fire, phase B); a recipe sends them to the
	// late inbox (it runs outside phase B).
	onRow func(store.TriggerFire)
	rows  []store.TriggerFire
}

// functions is the host object: exactly these eight names, nothing else is
// reachable from the script.
func (h *scriptHost) functions() fact.ScriptHost {
	return fact.ScriptHost{
		"query":   h.query,
		"explain": h.explain,
		"learn":   h.learn,
		"update":  h.update,
		"retract": h.retract,
		"emit":    h.emit,
		"push":    h.push,
		"run":     h.run,
	}
}

// call runs one tool through the injected set under a recover [M2]: a Go
// panic inside a handler is reported like a tool error — thrown to the
// script, crashdumped once per (trigger, blob, tool) — never left to escape
// RunProgram, where safeRun would drop the buffer and re-run the whole range
// on every kick, forever, for a panic that is deterministic for this fire.
func (h *scriptHost) call(tool string, args map[string]any) (out any, err error) {
	t0 := time.Now()
	defer func() {
		h.spent += time.Since(t0)
		if r := recover(); r != nil {
			if h.d.warnOnce("panic:" + h.p.trig.Name + "@" + h.cs.blob + ":" + tool) {
				crashdump.ReportRecovered("triggers:"+h.d.repo+":script:"+tool, r)
			}
			out, err = nil, fmt.Errorf("knomit.%s: panic: %v", tool, r)
		}
	}()
	if h.d.sc.tools == nil {
		return nil, fmt.Errorf("knomit.%s: no script tools on this instance", tool)
	}
	text, isErr, err := h.d.sc.tools.Call(h.ctx, tool, args)
	if err != nil {
		return nil, fmt.Errorf("knomit.%s: %w", tool, err)
	}
	if isErr {
		return nil, fmt.Errorf("knomit.%s: %s", tool, text)
	}
	if text == "" {
		return nil, nil
	}
	var v any
	if json.Unmarshal([]byte(text), &v) != nil {
		return text, nil
	}
	return v, nil
}

// momentName is `opts.moment_name`, else "trigger:<name>".
func (h *scriptHost) momentName(opts map[string]any) string {
	if m, ok := opts["moment_name"].(string); ok && m != "" {
		return m
	}
	return "trigger:" + h.p.trig.Name
}

func (h *scriptHost) query(args []any) (any, error) {
	return h.call("query", objectArg(args, 0))
}

func (h *scriptHost) explain(args []any) (any, error) {
	file, ok := stringArg(args, 0)
	if !ok {
		return nil, errors.New("knomit.explain(path, opts?): path must be a string")
	}
	a := objectArg(args, 1)
	a["file"] = file
	return h.call("explain", a)
}

func (h *scriptHost) learn(args []any) (any, error) {
	var facts []any
	switch v := argAt(args, 0).(type) {
	case []any:
		facts = v
	case map[string]any:
		facts = []any{v}
	default:
		return nil, errors.New("knomit.learn(fact | [facts], opts?): a fact object or a list of them")
	}
	for i, f := range facts {
		m, ok := f.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("knomit.learn: fact %d is not an object", i)
		}
		// The one divergence from the MCP tool (ruling R7): `path` is how a
		// session writes .knomit/<area>/ job state; a script may not.
		if _, has := m["path"]; has {
			return nil, fmt.Errorf("knomit.learn: fact %d: a script may not write under %s/ (no path; use topic and category)", i, fact.PrivateRoot)
		}
	}
	opts := objectArg(args, 1)
	// #349 [N1]: a script's or recipe's writes carry knomit's own trace
	// entries, which an agent trace may not join. update/retract pass opts
	// through, so the handler refuses a `trace` there; learn builds a fresh
	// call below and would DROP it silently — so it is refused here instead.
	if _, has := opts["trace"]; has {
		return nil, errors.New("knomit.learn: opts.trace is refused: a script's or recipe's writes carry knomit's own trace entries")
	}
	call := map[string]any{"facts": facts, "moment_name": h.momentName(opts)}
	// F04 (F08 PR A): opts.retract makes the call a move — the facts and the
	// deletions in one commit, all-or-nothing under the branch lock. Each path
	// passes the same private refusal as update/retract, so a script cannot
	// delete under .knomit/ by this route either. knomit.learn([], {retract})
	// is a batch retraction.
	if raw, has := opts["retract"]; has && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return nil, errors.New("knomit.learn: opts.retract must be a list of fact paths")
		}
		retract := make([]any, 0, len(list))
		for i, v := range list {
			p, err := h.privateRefused("learn", v)
			if err != nil {
				return nil, fmt.Errorf("knomit.learn: retract %d: %w", i, err)
			}
			retract = append(retract, p)
		}
		call["retract"] = retract
	}
	return h.call("learn", call)
}

func (h *scriptHost) update(args []any) (any, error) {
	file, err := h.factPath("update", args)
	if err != nil {
		return nil, err
	}
	a := objectArg(args, 1)
	a["file"] = file
	a["moment_name"] = h.momentName(a)
	return h.call("update", a)
}

func (h *scriptHost) retract(args []any) (any, error) {
	file, err := h.factPath("retract", args)
	if err != nil {
		return nil, err
	}
	a := objectArg(args, 1)
	a["file"] = file
	a["moment_name"] = h.momentName(a)
	return h.call("retract", a)
}

// factPath reads the path argument of update/retract and refuses a private
// one — checked on the path AS THE HANDLER WOULD WRITE IT (after
// NormalizePath, the handler's own order), so `.knomit/x` and `.knomit/x.md`
// are refused alike. The handler would accept `.knomit/<area>/…` (job state
// for sessions); a script never writes under `.knomit/`.
func (h *scriptHost) factPath(tool string, args []any) (string, error) {
	file, ok := stringArg(args, 0)
	if !ok || file == "" {
		return "", fmt.Errorf("knomit.%s(path, opts?): path must be a string", tool)
	}
	return h.privateRefused(tool, file)
}

// privateRefused is factPath's rule for one path value: a non-empty string
// whose normalised form is not under a private segment.
func (h *scriptHost) privateRefused(tool string, v any) (string, error) {
	file, ok := v.(string)
	if !ok || file == "" {
		return "", fmt.Errorf("knomit.%s: a path must be a non-empty string, got %v", tool, v)
	}
	root := h.d.ri.ontologyRoot
	if root == "" {
		root = "kb"
	}
	norm := fact.NormalizePath(root, file)
	if fact.IsPrivatePath(norm) {
		return "", fmt.Errorf("knomit.%s: a script may not write under %s/ (%s)", tool, fact.PrivateRoot, norm)
	}
	return file, nil
}

func (h *scriptHost) emit(args []any) (any, error) {
	var payload json.RawMessage
	if v := argAt(args, 0); v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("knomit.emit: payload: %w", err)
		}
		payload = b
	}
	h.d.emit(h.p, payload)
	return nil, nil
}

// push is the `push` action from a script (F07 PR 4): the same non-blocking
// wake of this machine's sync loop, and `{ok: true, status: "kicked"}` — the
// stub's shape, so a script can branch on status. It takes no arguments and
// ignores any it is given: nothing can name a branch. Several calls in one
// run (or in several fires) fold into the one slot; the per-minute script
// cap already bounds the runs. The fire's outcome stays `ran`.
func (h *scriptHost) push(args []any) (any, error) {
	h.d.wakeSync()
	return map[string]any{"ok": true, "status": store.TriggerOutcomeKicked}, nil
}

// run is knomit.run(name, payload?) (F07 PR 5): the `run` action from a
// script. The name must be a kebab-case recipe name (it becomes a path; a bad
// one throws). It resolves and STARTS the recipe exactly as `do: run` does
// and answers at once, never waiting for the recipe (ruling D5: "yes,
// started, and hopefully with a task id which we can use to correlate the
// results later on"):
//
//   - {ok: true, status: "started", id: "run-<32 hex>", source: "repo"|"local"}
//   - {bound: false} — no recipe on main or on this machine (no row)
//   - {ok: false, status: "busy"} — the recipe is at its `concurrent` limit
//   - {ok: false, status: "recipe-error", error} — the recipe does not compile
//
// Every answer but unbound also logs a row for this trigger (started, busy
// or recipe-error, with the run id when one was minted); the recipe's result
// is a later row with the same run id, found with GET …/triggers?run=<id>.
// payload is untrusted data a repo script chose: the recipe gets it as
// `payload`, and the core never puts it into argv. The script's own fire
// stays `ran`.
func (h *scriptHost) run(args []any) (any, error) {
	name, ok := stringArg(args, 0)
	if !ok || !fact.ValidRecipeName(name) {
		return nil, fmt.Errorf("knomit.run(name, payload?): name must be a kebab-case recipe name (.knomit/recipes/<name>.js), got %v", argAt(args, 0))
	}
	st := h.d.startRecipe(h.p, name, argAt(args, 1), h.globals, h.rangeFrom, h.rangeTo)
	if st.outcome == "" {
		return map[string]any{"bound": false}, nil
	}
	h.d.stats.recordRecipe(h.p.trig.Name, st.outcome)
	if h.onRow != nil {
		h.onRow(store.TriggerFire{
			Trigger: h.p.trig.Name, Branch: h.d.branch, Path: h.p.repoPath, Episode: h.p.episode, Source: h.p.source,
			Commit: h.p.commit, Trace: h.p.trace, Nonlinear: h.p.nonlinear, RangeFrom: h.rangeFrom, RangeTo: h.rangeTo,
			Outcome: st.outcome, Error: st.errText, RecipeSource: st.source, RecipeRev: st.rev, RunID: st.id,
		})
	}
	switch st.outcome {
	case store.TriggerOutcomeStarted:
		return map[string]any{"ok": true, "status": st.outcome, "id": st.id, "source": st.source}, nil
	case store.TriggerOutcomeBusy:
		return map[string]any{"ok": false, "status": st.outcome}, nil
	default:
		return map[string]any{"ok": false, "status": st.outcome, "error": st.errText}, nil
	}
}

func argAt(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}
	return nil
}

func stringArg(args []any, i int) (string, bool) {
	s, ok := argAt(args, i).(string)
	return s, ok
}

// objectArg is the object at i as a fresh map (an absent or non-object
// argument is an empty map), so the host may add keys without touching what
// the script passed.
func objectArg(args []any, i int) map[string]any {
	out := map[string]any{}
	if m, ok := argAt(args, i).(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// deriveTrace is the fire's trace (design: "firing on commit C hands
// trace(C): read from C's trailer, minted if C has none"), in this order:
// the toucher's Knomit-Trace, copied forward unchanged (the story continues);
// else, when the firing fact is an F01 task — a pragmatic `signal` with
// exactly one entity, the task id — that entity; else the firing commit's own
// hash, which is the same 40 hex on every machine and on a re-fire, so one
// cause is one story and `git log --all --grep='^Knomit-Trace: <C>'` lists
// its descendants below the root C itself. Never empty.
func deriveTrace(trailer string, factMap map[string]any, commit string) string {
	if trailer != "" {
		return trailer
	}
	if factMap != nil && factMap["kind"] == string(fact.Pragmatic) && factMap["type"] == string(fact.Signal) {
		if ents, ok := factMap["entities"].([]string); ok && len(ents) == 1 && strings.TrimSpace(ents[0]) != "" {
			return ents[0]
		}
	}
	return commit
}
