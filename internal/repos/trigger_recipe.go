// The `do: run` half of the trigger dispatcher (F07 PR 5): recipe resolution,
// the recipe cache, the runner that starts a recipe BESIDE the dispatcher,
// and the trusted host a recipe sees.
//
// WHERE A RECIPE COMES FROM (user ruling D1, verbatim: "A, recipes run only
// from main. No enforcement on who actually adds the recipes in main for
// now"), per fire, one function for `do: run` and knomit.run:
//
//  1. `.knomit/recipes/<name>.js` at the tip of the repo's CONSENSUS branch —
//     UpstreamTip(UpstreamBranch()), the ref verifiedBelow reads — never the
//     agent branch. No signature, certificate, last-writer or author is
//     read: how the file reached main is main's business. A repo with no
//     upstream branch (a zero tip) has no repo tier. A main recipe that does
//     not compile is a `recipe-error` with NO fall-back (D7): silently running
//     a different program is worse than a visible error.
//  2. `<home>/recipes/<name>.js` on this machine.
//  3. Neither: unbound — `do: run` counts `unbound` and writes no row;
//     knomit.run returns {bound: false}.
//
// Repo recipes are cached by the blob of the file at main's tip; local ones by
// (mtime, size), with the content hash re-checked when either moves. A new
// blob or hash recompiles before the next fire and the old program becomes
// unreachable.
//
// HOW IT RUNS (D4): phase B only resolves and hands off — it takes a
// concurrency slot with a NON-BLOCKING try (a full recipe drops the fire as
// `busy`, D6), mints the run id, records `started`, and starts a goroutine.
// The dispatcher never waits for a recipe. The goroutine sleeps the jitter,
// runs the recipe in a fresh VM under the recipe's budget (a ctx derived from
// the dispatcher's own, so stop() interrupts it and kills its processes),
// releases the slot, and hands its RESULT row to the dispatcher through the
// late inbox (d.lateIn) — never touching d.pending, which only the dispatcher
// goroutine mutates. The dispatcher moves the inbox into phase C's buffer on
// its next run; pendingFlush.empty() counts those rows, so a result with no
// new commit is still flushed [M1].
//
// AT-MOST-ONCE after the fire is flushed: a stop or restart kills running
// recipes and nothing re-runs them. A CRASH (not a clean stop) before the
// fire's phase C is flushed leaves the bookmark behind, so the range re-fires
// on restart and the recipe runs once more [M2]. The window is the flush
// grace plus the flush; it is named, not closed.
package repos

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/serverkey"
	"knomit/internal/store"
)

// outcomeUnbound is a `do: run` evaluation that found no recipe anywhere: a
// counted no-op, like if-false, never a row (the fire-log cap holds real
// fires).
const outcomeUnbound = "unbound"

// compiledRecipe is one resolved recipe. err set means it resolved to a
// broken file (does not compile, bad header, unreadable): a `recipe-error`.
type compiledRecipe struct {
	name   string
	source string // store.RecipeSourceRepo | store.RecipeSourceLocal
	rev    string // the blob at main's tip, or sha256[:12] of the local file
	prog   *goja.Program
	limits fact.RecipeLimits
	err    error
}

// localRecipe is the local cache entry: the stat that found it and the
// compiled recipe of its content.
type localRecipe struct {
	mtime time.Time
	size  int64
	cr    *compiledRecipe
}

// recipeState is the dispatcher's `do: run` state. Every field is behind mu:
// resolution runs on the dispatcher goroutine (do: run, a script's
// knomit.run) AND on runner goroutines (a recipe's own knomit.run).
type recipeState struct {
	mu       sync.Mutex
	repo     map[string]*compiledRecipe // name → the recipe of its last seen main blob
	local    map[string]*localRecipe
	running  map[string]int  // name → recipes running now (the `concurrent` slots)
	logged   map[string]bool // name@rev keys already reported at ERROR
	compiles int             // test hook
	// wg counts running recipes; the loop waits for it at stop before the
	// final flush, so the killed recipes' `stopped` rows are written.
	wg sync.WaitGroup
}

func newRecipeState() recipeState {
	return recipeState{
		repo: map[string]*compiledRecipe{}, local: map[string]*localRecipe{},
		running: map[string]int{}, logged: map[string]bool{},
	}
}

// resolveRecipe finds the recipe name names (see the file comment). found is
// false when neither tier has it (unbound). A returned recipe with err set is
// a broken one: the caller records `recipe-error` and does NOT fall back.
func (d *triggerDispatcher) resolveRecipe(ctx context.Context, name string) (cr *compiledRecipe, found bool) {
	if cr, found := d.mainRecipe(ctx, name); found {
		return cr, true
	}
	return d.localRecipe(name)
}

// mainRecipe is the repo tier: the file at the tip of the consensus branch.
func (d *triggerDispatcher) mainRecipe(ctx context.Context, name string) (*compiledRecipe, bool) {
	svc, release, err := d.ri.Acquire()
	if err != nil {
		// Mid-swap or closed: whether main has the recipe is unknown, and
		// running the local one could run the wrong program. Say so.
		return &compiledRecipe{name: name, err: fmt.Errorf("recipe %s: store unavailable: %w", name, err)}, true
	}
	defer release()
	tr := svc.Triggers()
	tip, err := tr.UpstreamTip(ctx, svc.UpstreamBranch())
	if err != nil {
		return &compiledRecipe{name: name, err: fmt.Errorf("recipe %s: main unreadable: %w", name, err)}, true
	}
	if tip.IsZero() {
		return nil, false // no upstream branch: no repo tier
	}
	blob, data, err := tr.RecipeAt(ctx, tip, name)
	if errors.Is(err, store.ErrNoRecipeAtCommit) {
		return nil, false
	}
	if err != nil {
		return &compiledRecipe{name: name, source: store.RecipeSourceRepo, err: fmt.Errorf("recipe %s: %w", name, err)}, true
	}
	d.rc.mu.Lock()
	if cur := d.rc.repo[name]; cur != nil && cur.rev == blob {
		d.rc.mu.Unlock()
		return cur, true
	}
	d.rc.mu.Unlock()
	cr := d.compileRecipe(name, store.RecipeSourceRepo, blob, data)
	d.rc.mu.Lock()
	d.rc.repo[name] = cr
	d.rc.mu.Unlock()
	return cr, true
}

// localRecipe is the machine tier: <home>/recipes/<name>.js.
func (d *triggerDispatcher) localRecipe(name string) (*compiledRecipe, bool) {
	if d.home == "" {
		return nil, false
	}
	path := filepath.Join(d.home, "recipes", name+".js")
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		return &compiledRecipe{name: name, source: store.RecipeSourceLocal, err: fmt.Errorf("recipe %s: %w", name, err)}, true
	}
	if !fi.Mode().IsRegular() {
		return &compiledRecipe{name: name, source: store.RecipeSourceLocal, err: fmt.Errorf("recipe %s: %s is not a file", name, path)}, true
	}
	d.rc.mu.Lock()
	cur := d.rc.local[name]
	if cur != nil && cur.mtime.Equal(fi.ModTime()) && cur.size == fi.Size() {
		d.rc.mu.Unlock()
		return cur.cr, true
	}
	d.rc.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return &compiledRecipe{name: name, source: store.RecipeSourceLocal, err: fmt.Errorf("recipe %s: %w", name, err)}, true
	}
	sum := sha256.Sum256(data)
	rev := hex.EncodeToString(sum[:])[:12]
	var cr *compiledRecipe
	if cur != nil && cur.cr.rev == rev {
		cr = cur.cr // touched, not changed: keep the program
	} else {
		cr = d.compileRecipe(name, store.RecipeSourceLocal, rev, data)
	}
	d.rc.mu.Lock()
	d.rc.local[name] = &localRecipe{mtime: fi.ModTime(), size: fi.Size(), cr: cr}
	d.rc.mu.Unlock()
	return cr, true
}

// compileRecipe compiles one recipe version, logging a broken one at ERROR
// once per (name, rev).
func (d *triggerDispatcher) compileRecipe(name, source, rev string, data []byte) *compiledRecipe {
	cr := &compiledRecipe{name: name, source: source, rev: rev}
	var err error
	cr.prog, cr.limits, err = fact.CompileRecipe(name, string(data))
	d.rc.mu.Lock()
	d.rc.compiles++
	logNow := false
	if err != nil {
		cr.err = fmt.Errorf("recipe %s (%s %s) does not compile: %w", name, source, rev, err)
		key := name + "@" + rev
		logNow = !d.rc.logged[key]
		d.rc.logged[key] = true
	}
	d.rc.mu.Unlock()
	if logNow {
		log.Error().Str("repo", d.repo).Str("branch", d.branch).Str("recipe", capForLog(name)).
			Str("source", source).Str("rev", rev).Str("error", cr.err.Error()).Msg("invalid recipe skipped")
	}
	return cr
}

// tryTake takes one of name's `concurrent` slots without blocking (D6: a full
// recipe drops the fire as `busy`; there is no queue).
func (rc *recipeState) tryTake(name string, limit int) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.running[name] >= limit {
		return false
	}
	rc.running[name]++
	return true
}

func (rc *recipeState) release(name string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.running[name]--; rc.running[name] <= 0 {
		delete(rc.running, name)
	}
}

// newRunID mints a run id: `run-` and 16 random bytes in hex. One per start;
// the `started` row, the result row, the recipe's `run.id`, the child's
// KNOMIT_RUN and knomit.run's return value all carry it (D5). It is NOT the
// trace: a trace is a story shared by every fire in it, a run id names one
// start.
func newRunID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "run-" + hex.EncodeToString(b[:])
}

// runStart is what a start attempt produced, for the fire's row and for
// knomit.run's answer. outcome "" is unbound (nothing found, no row).
type runStart struct {
	outcome string
	errText string
	source  string
	rev     string
	id      string
}

// recipeJob is one accepted start, handed to its goroutine.
type recipeJob struct {
	cr      *compiledRecipe
	id      string
	p       pendingFire
	globals map[string]any // fact, agent, change of the fire
	payload any
	// row is the result row's template: the fire's identity, its range and
	// branch (a late row is written without its run), the recipe source, rev
	// and run id. The runner fills the outcome, the error and the duration.
	row store.TriggerFire
}

// startRecipe is the whole of `do: run` inside phase B, and knomit.run: resolve
// name, take a slot, mint the id, start the goroutine. It never blocks on the
// recipe. p is the fire (trigger, path, change); globals its fact/agent/change;
// rangeFrom/rangeTo the range the fire belongs to (copied onto the late row).
func (d *triggerDispatcher) startRecipe(p pendingFire, name string, payload any, globals map[string]any, rangeFrom, rangeTo string) runStart {
	ctx := d.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	cr, found := d.resolveRecipe(ctx, name)
	if !found {
		return runStart{}
	}
	st := runStart{source: cr.source, rev: cr.rev}
	if cr.err != nil {
		st.outcome, st.errText = store.TriggerOutcomeRecipeError, cr.err.Error()
		return st
	}
	if !d.rc.tryTake(name, cr.limits.Concurrent) {
		st.outcome = store.TriggerOutcomeBusy
		st.errText = fmt.Sprintf("recipe %s is at its limit of %d concurrent run(s); the fire is dropped", name, cr.limits.Concurrent)
		return st
	}
	st.outcome, st.id = store.TriggerOutcomeStarted, newRunID()
	job := recipeJob{
		cr: cr, id: st.id, p: p, globals: globals, payload: payload,
		row: store.TriggerFire{
			Trigger: p.trig.Name, Branch: d.branch, Path: p.repoPath, Episode: p.episode, Source: p.source,
			Commit: p.commit, Trace: p.trace, Nonlinear: p.nonlinear, RangeFrom: rangeFrom, RangeTo: rangeTo,
			RecipeSource: cr.source, RecipeRev: cr.rev, RunID: st.id,
		},
	}
	d.rc.wg.Add(1)
	go d.runJob(ctx, job)
	return st
}

// runJob is one recipe run, on its own goroutine: the jitter, the recipe, the
// slot back, the result row into the late inbox, a kick.
func (d *triggerDispatcher) runJob(ctx context.Context, j recipeJob) {
	defer d.rc.wg.Done()
	t0 := time.Now()
	outcome, msg := d.execRecipe(ctx, j)
	d.rc.release(j.cr.name)
	row := j.row
	row.Outcome, row.Error, row.DurationMS = outcome, msg, time.Since(t0).Milliseconds()
	d.stats.recordRecipe(row.Trigger, outcome)
	log.Debug().Str("repo", d.repo).Str("trigger", capForLog(row.Trigger)).Str("recipe", capForLog(j.cr.name)).
		Str("run", j.id).Str("outcome", outcome).Msg("recipe finished")
	d.addLate(row)
}

// execRecipe runs the recipe and maps what happened to the result row's
// outcome and error text.
func (d *triggerDispatcher) execRecipe(ctx context.Context, j recipeJob) (outcome, msg string) {
	defer func() {
		if r := recover(); r != nil {
			outcome, msg = store.TriggerOutcomeRecipeError, fmt.Sprintf("recipe %s: panic: %v", j.cr.name, r)
		}
	}()
	if jit := j.cr.limits.Jitter; jit > 0 {
		t := time.NewTimer(time.Duration(mathrand.Int64N(int64(jit) + 1)))
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
	}
	if ctx.Err() != nil {
		return store.TriggerOutcomeRecipeError, fmt.Sprintf("recipe %s: stopped before it ran", j.cr.name)
	}
	budget, cancel := context.WithTimeout(ctx, j.cr.limits.Timeout)
	defer cancel()
	hostCtx := WithBinding(budget, NewBindingOfRepo(d.ri, d.branch))
	hostCtx = store.WithTrailers(hostCtx, store.Trailers{Trace: j.p.trace, Cause: j.p.commit, Trigger: j.p.trig.Name})
	h := &scriptHost{d: d, p: j.p, cs: &compiledScript{script: "recipe " + j.cr.name, blob: j.cr.rev}, ctx: hostCtx,
		globals: j.globals, rangeFrom: j.row.RangeFrom, rangeTo: j.row.RangeTo, onRow: d.addLate}
	x := &recipeExec{ctx: hostCtx, env: d.recipeEnv(j)}
	res, err := fact.RunRecipe(hostCtx, j.cr.prog, j.cr.name, d.recipeGlobals(j), recipeHostFunctions(h, x), time.Now().UTC())
	var interrupted *goja.InterruptedError
	switch {
	case ctx.Err() != nil:
		return store.TriggerOutcomeRecipeError, fmt.Sprintf("recipe %s: stopped (knomit stopped the dispatcher)", j.cr.name)
	case err == nil:
		msg := res.Message
		if res.ID != "" {
			if msg != "" {
				msg += " "
			}
			msg += "id=" + res.ID
		}
		if res.Status == fact.RecipeStatusError {
			return store.TriggerOutcomeRecipeError, msg
		}
		return res.Status, msg // done | spawned | delivered | unreachable: the store's kinds
	case errors.As(err, &interrupted) || errors.Is(budget.Err(), context.DeadlineExceeded):
		return store.TriggerOutcomeRecipeTimeout, fmt.Sprintf("recipe %s exceeded %s", j.cr.name, j.cr.limits.Timeout)
	default:
		return store.TriggerOutcomeRecipeError, err.Error()
	}
}

// recipeGlobals is what a recipe sees besides `knomit`: the fire's fact,
// agent and change, the caller's payload (untrusted data a repo script chose —
// the core never puts it into argv), task_path (the firing fact's repo path,
// from the fire itself: a script cannot choose it), this instance's MCP
// configuration, and run = {id}.
func (d *triggerDispatcher) recipeGlobals(j recipeJob) map[string]any {
	g := map[string]any{}
	for k, v := range j.globals {
		g[k] = v
	}
	g["payload"] = j.payload
	g["task_path"] = j.p.repoPath
	g["mcp"] = recipeMCP(d.repo)
	g["run"] = map[string]any{"id": j.id}
	return g
}

// recipeMCP is this repo's MCP configuration exactly as `kb claude init`
// writes it (the same ServerKey rule), as a JSON STRING for
// `claude --mcp-config`, plus the server key and the documented server-wide
// allow form `mcp__<server>` for `--allowedTools` [N3]. `kb` finds this
// server through the local socket its inherited environment names [N1].
func recipeMCP(repo string) map[string]any {
	key := serverkey.ServerKey(repo, "")
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		key: map[string]any{"command": "kb", "args": []string{"--repo", repo}},
	}})
	return map[string]any{"server": key, "config": string(cfg), "tools": "mcp__" + key}
}

// recipeEnv is the child's base environment: knomit's own, plus
// KNOMIT_HOME (this server's home, so `kb` reaches THIS server's socket),
// KNOMIT_TRACE / KNOMIT_CAUSE (the fire's trace and firing commit) and
// KNOMIT_RUN (the run id). exec merges the recipe's own `env` over it; it
// never replaces it, so PATH and HOME survive.
func (d *triggerDispatcher) recipeEnv(j recipeJob) []string {
	over := map[string]string{"KNOMIT_TRACE": j.p.trace, "KNOMIT_CAUSE": j.p.commit, "KNOMIT_RUN": j.id}
	if d.home != "" {
		over["KNOMIT_HOME"] = d.home
	}
	return mergeEnv(os.Environ(), over)
}

// recipeHostFunctions is the TRUSTED host map: the script's eight functions
// plus `exec`. The script host (scriptHost.functions) never has `exec`; this
// is the only place the two maps differ.
func recipeHostFunctions(h *scriptHost, x *recipeExec) fact.ScriptHost {
	m := h.functions()
	m["exec"] = x.call
	return m
}

// addLate hands one row to the dispatcher (the late inbox) and kicks it. Any
// goroutine; never touches d.pending.
func (d *triggerDispatcher) addLate(row store.TriggerFire) {
	d.lateMu.Lock()
	d.lateIn = append(d.lateIn, row)
	d.lateMu.Unlock()
	d.triggerKick()
}

// drainLate moves the late inbox into phase C's buffer for store generation
// svc. Dispatcher goroutine only.
func (d *triggerDispatcher) drainLate(svc *store.Service) {
	d.lateMu.Lock()
	rows := d.lateIn
	d.lateIn = nil
	d.lateMu.Unlock()
	if len(rows) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	p := &d.pending
	if p.svc != svc {
		if !p.empty() {
			*p = pendingFlush{} // another generation's buffer: the tables are the truth
		}
		p.svc = svc
	}
	p.late = append(p.late, rows...)
}
