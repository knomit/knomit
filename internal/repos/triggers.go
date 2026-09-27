// The trigger dispatcher (F07 PR 1b). One actor per writable repo that turns
// advances of THIS machine's agent branch into trigger fires.
//
// The write path does no trigger work. The commit observer callback
// (ri.onCommit, builder.go) runs under the writer's branch lock and only does
// a non-blocking send on a 1-slot channel — the kick — when the branch is the
// agent branch. The channel IS the pending flag: a second kick while one is
// pending merges into it, and the send never blocks. The dispatcher is
// level-triggered: whatever the kicks, a run processes "watermark → current
// head", so coalesced advances are diffed once and nothing is lost.
//
// A run has three phases, and the store is held (Acquire) only in the first
// and the last:
//
//	A  (Acquire) read the head; the ontology at the head → the compiled trigger
//	   set (cached by blob); the watermarks; ONE tree diff per distinct
//	   watermark; glob matching in memory; `change` built ONCE per matched
//	   path (the toucher walk, the signer, verified, the trailer).
//	   release
//	B  (no store) per (trigger, path): `if` then emit, ctx checked per path;
//	   timed as the trigger's own work for the statistics and the slow
//	   detector.
//	C  (Acquire) tx1: the run's fire rows + ONE run row; tx2: watermarks and
//	   the prune. release
//
// So a long `if` never holds a store reference (SwapStore and teardown do not
// drain behind it), and the dispatcher's SQLite writes take the process-wide
// write lock at most twice per run, never across user code.
//
// The dispatcher has its OWN context and wait group. It must not share
// syncCtx: ActivateSync cancels that to restart the reconcile loop and would
// kill the dispatcher whenever an origin is attached.
package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/platform/crashdump"
	"knomit/internal/store"
)

// triggerTraceTrailer is the commit trailer the dispatcher READS for
// change.trace. Nothing stamps it before PR 3.
const triggerTraceTrailer = "Knomit-Trace"

// maxTriggerLogField caps repo content (trigger names, paths) in the slow
// trigger WARN, the same bound the slow-request log applies to client fields.
const maxTriggerLogField = 256

// triggerHooks are test seams. Every field is nil (or zero) in production.
type triggerHooks struct {
	// afterKick runs between receiving a kick and reading the head ref. It is
	// how a test parks the worker, and how KickReceivedBeforeRefRead commits
	// in the window the ordering rule exists for.
	afterKick func()
	// beforeTx2 runs between tx1 and tx2 (the crash test panics here).
	beforeTx2 func()
	// changeDelay is added inside the change build: knomit's per-path cost,
	// which must not be charged to the trigger.
	changeDelay time.Duration
}

var (
	triggerHooksMu   sync.Mutex
	triggerTestHooks triggerHooks
)

func currentTriggerHooks() triggerHooks {
	triggerHooksMu.Lock()
	defer triggerHooksMu.Unlock()
	return triggerTestHooks
}

// triggerDispatcher is one repo's dispatcher.
type triggerDispatcher struct {
	ri       *RepoInstance
	repo     string
	branch   string // the agent branch: the only branch that kicks
	identity fact.TriggerIdentity
	slow     time.Duration
	kick     chan struct{}
	cache    fact.TriggerCache
	stats    *triggerStats

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	lastSet  *fact.TriggerSet
	lastHead string
	lastBlob string
	runSeq   int64
	// errLogged holds the name@blob keys already reported at ERROR, so a
	// broken trigger is logged once per blob and not on every advance. Reset
	// when the blob changes (the keys carry the blob, so old ones are dead).
	errLogged map[string]bool
	errBlob   string
	// ontErr is the blob (or head, when there is no ontology) whose failure
	// was already logged.
	ontErr string
	// verified caches the anchor's ancestor set built by THIS dispatcher when
	// F09's own cache was empty (R2-1), keyed by the anchor.
	verified struct {
		anchor plumbing.Hash
		set    map[plumbing.Hash]bool
	}
}

// newTriggerDispatcher builds the dispatcher for ri WITHOUT starting it, so
// the kick slot exists from build() (a write during the background heal kicks
// it) while the goroutine starts from activate() after the initial index.
func newTriggerDispatcher(ri *RepoInstance, repo, agentBranch string, signer ssh.Signer, slowMS int) *triggerDispatcher {
	return &triggerDispatcher{
		ri:       ri,
		repo:     repo,
		branch:   agentBranch,
		identity: triggerIdentityFor(agentBranch, signer),
		slow:     time.Duration(slowMS) * time.Millisecond,
		kick:     make(chan struct{}, 1),
		stats:    newTriggerStats(),
	}
}

// triggerIdentityFor is this instance's {agent}, {host} and {fp8}. {agent} is
// the branch minus "agent/", which is also the commit author's name; {fp8} is
// the first 8 hex of sha256 of the signing key (identity.go's fingerprint),
// falling back to the branch suffix when no signer is known; {host} is the
// branch name without the fingerprint suffix.
func triggerIdentityFor(agentBranch string, signer ssh.Signer) fact.TriggerIdentity {
	agent := strings.TrimPrefix(agentBranch, "agent/")
	id := fact.TriggerIdentity{Agent: agent, Host: agent}
	if signer != nil {
		h := sha256.Sum256(signer.PublicKey().Marshal())
		id.FP8 = hex.EncodeToString(h[:])[:8]
	}
	if i := strings.LastIndex(agent, "-"); i >= 0 && isHex8(agent[i+1:]) {
		id.Host = agent[:i]
		if id.FP8 == "" {
			id.FP8 = agent[i+1:]
		}
	}
	return id
}

func isHex8(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// start launches the actor with its own context derived from parent. It kicks
// itself once so advances made while the server was down fire on restart (the
// watermark makes that at-least-once).
func (d *triggerDispatcher) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	d.wg.Add(1)
	d.triggerKick()
	go d.loop(ctx)
}

// stop cancels the actor and waits for the current run to notice (phase B
// checks ctx per path, so it returns within one evaluation).
func (d *triggerDispatcher) stop() {
	if d.cancel != nil {
		d.cancel()
	}
	d.wg.Wait()
}

// triggerKick is the observer's whole job for triggers: record "the agent
// branch advanced" and return. O(1), never blocks, never allocates.
func (d *triggerDispatcher) triggerKick() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// triggerKick on the instance is what ri.onCommit calls; nil when this repo
// has no dispatcher (read-only server, subscription).
func (ri *RepoInstance) triggerKick() {
	if d := ri.triggers; d != nil {
		d.triggerKick()
	}
}

// loop receives kicks. The kick is RECEIVED before the head ref is read
// (inside run): the reverse order would swallow the kick of a commit that
// lands between the ref read and the drain, the lost-wake-up shape.
func (d *triggerDispatcher) loop(ctx context.Context) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.kick:
			d.safeRun(ctx)
		}
	}
}

// safeRun is one run with panic recovery: a panic (or a test hook that panics
// between tx1 and tx2) must not take the actor down; the range simply re-fires
// on the next run or after a restart, which is the at-least-once contract.
func (d *triggerDispatcher) safeRun(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			crashdump.ReportRecovered("triggers:"+d.repo, r)
			log.Error().Str("repo", d.repo).Str("branch", d.branch).Interface("panic", r).
				Msg("trigger dispatcher: run panicked; the range will be re-run")
		}
		d.mu.Lock()
		d.runSeq++
		d.mu.Unlock()
	}()
	if h := currentTriggerHooks().afterKick; h != nil {
		h()
	}
	d.run(ctx)
}

// pendingFire is one (trigger, path) evaluation phase A prepared for phase B.
type pendingFire struct {
	trig      *fact.CompiledTrigger
	repoPath  string
	episode   string
	nonlinear bool
	commit    string
	source    string
	trace     string
	change    map[string]any
	factMap   map[string]any // nil when the fact is absent or unparseable
	parseable bool
}

// runState is everything one run accumulates across the phases.
type runState struct {
	head      string
	newWM     map[string]string
	del       []string
	pending   []pendingFire
	paths     int
	nonlinear bool
	rangeFrom string
	diffMS    int64
	changeMS  int64
	started   time.Time
}

func (d *triggerDispatcher) run(ctx context.Context) {
	rs := &runState{newWM: map[string]string{}, started: time.Now()}

	// ---- Phase A: with the store.
	svc, release, err := d.ri.Acquire()
	if err != nil {
		return // closed or mid-swap: the next commit kicks again
	}
	ok := d.phaseA(ctx, svc, rs)
	release()
	if !ok {
		return
	}

	// ---- Phase B: no store.
	rows, evaluated, fires, aborted := d.phaseB(ctx, rs)
	if aborted {
		return // ctx cancelled: nothing is recorded, the range re-fires
	}

	// ---- Phase C: with the store, at most two short transactions.
	svc, release, err = d.ri.Acquire()
	if err != nil {
		return
	}
	defer release()
	tr := svc.Triggers()
	run := store.TriggerRun{
		Branch: d.branch, RangeFrom: rs.rangeFrom, RangeTo: rs.head, Nonlinear: rs.nonlinear,
		Paths: rs.paths, Evaluated: evaluated, Fires: fires,
		DurationMS: time.Since(rs.started).Milliseconds(), DiffMS: rs.diffMS, ChangeMS: rs.changeMS, Rows: rows,
	}
	if rs.paths > 0 {
		if _, err := tr.RecordTriggerRun(ctx, run); err != nil {
			log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: fire log write failed; the range will be re-run")
			return
		}
	}
	if h := currentTriggerHooks().beforeTx2; h != nil {
		h()
	}
	if len(rs.newWM) > 0 || len(rs.del) > 0 {
		if err := tr.AdvanceTriggerWatermarks(ctx, d.branch, rs.newWM, rs.del); err != nil {
			log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: watermark write failed; the range will be re-run")
			return
		}
	}
	if rs.paths > 0 {
		d.stats.recordRun(TriggerLastRun{
			RangeFrom: rs.rangeFrom, RangeTo: rs.head, Nonlinear: rs.nonlinear, Paths: rs.paths,
			Evaluated: evaluated, Fires: fires, DurationMS: run.DurationMS, DiffMS: rs.diffMS, ChangeMS: rs.changeMS,
		})
	}
}

// phaseA reads everything the run needs and prepares the pending fires. It
// returns false when there is nothing to do (or the run should stop).
func (d *triggerDispatcher) phaseA(ctx context.Context, svc *store.Service, rs *runState) bool {
	tr := svc.Triggers()
	headStr, err := svc.Branches().HeadCommit(ctx, d.branch)
	if err != nil {
		log.Warn().Err(err).Str("repo", d.repo).Str("branch", d.branch).Msg("trigger dispatcher: head unreadable")
		return false
	}
	head := plumbing.NewHash(headStr)
	rs.head = headStr

	// The trigger set at THIS head, cached by the ontology blob (D-b).
	_, blob, data, oerr := tr.OntologyAtCommit(ctx, head)
	var set *fact.TriggerSet
	if oerr != nil {
		d.logOntologyErrorOnce(headStr, oerr)
		set = d.lastCompiledSet()
	} else {
		set, err = d.cache.Get(blob, data, d.identity)
		if err != nil {
			d.logOntologyErrorOnce(blob, err) // last good set is returned alongside
		} else {
			d.clearOntologyError()
		}
	}
	if set == nil {
		return false
	}
	d.recordSet(set, headStr, blob)
	d.logInvalidOnce(set)

	wms, err := tr.TriggerWatermarks(ctx, d.branch)
	if err != nil {
		log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: watermarks unreadable")
		return false
	}

	// Per declared name: first appearance, freeze, unsupported, or advance.
	active := map[string]*fact.CompiledTrigger{}
	for _, ct := range set.Active {
		active[ct.Name] = ct
	}
	states := nameStates(set)
	byW := map[string][]*fact.CompiledTrigger{}
	for name := range set.Declared {
		wm, has := wms[name]
		switch states[name] {
		case fact.TriggerActive:
			if !has {
				rs.newWM[name] = headStr // first appearance: no back-fill
				continue
			}
			if wm == headStr {
				continue
			}
			byW[wm] = append(byW[wm], active[name])
		case fact.TriggerUnsupported:
			// No bookmark until a version acts on it: it starts at the head
			// of the advance where it becomes active, never back-filling.
			if has {
				rs.del = append(rs.del, name)
			}
		default: // invalid: frozen. A first appearance while invalid starts here.
			if !has {
				rs.newWM[name] = headStr
			}
		}
	}
	for name := range wms {
		if !set.Declared[name] {
			rs.del = append(rs.del, name) // the NAME is gone: forget it
		}
	}
	sort.Strings(rs.del)
	if len(byW) == 0 {
		return len(rs.newWM) > 0 || len(rs.del) > 0
	}

	// `verified` needs the mode at the head and the anchor's history.
	verifyOn := false
	if oerr == nil {
		if vs, err := fact.ReadVerifySettings(data); err == nil && vs.Valid && vs.Mode != store.VerifyOff {
			verifyOn = true
		}
	}
	var verifiedSet map[plumbing.Hash]bool
	if verifyOn {
		verifiedSet = d.verifiedBelow(ctx, tr, svc.UpstreamBranch())
	}
	instanceFP := svc.SignerFingerprint()
	root := d.ri.ontologyRoot
	if root == "" {
		root = "kb"
	}

	// Normally every trigger shares one watermark, so this is ONE diff.
	ws := make([]string, 0, len(byW))
	for w := range byW {
		ws = append(ws, w)
	}
	sort.Strings(ws)
	for _, w := range ws {
		trigs := byW[w]
		wh := plumbing.NewHash(w)
		linear, err := tr.IsAncestor(ctx, wh, head)
		if err != nil {
			// The watermark's commit is gone (garbage-collected): reset to the
			// head, fire nothing, say so.
			log.Warn().Err(err).Str("repo", d.repo).Str("watermark", w).
				Msg("trigger dispatcher: watermark commit unreadable; resetting to head")
			for _, ct := range trigs {
				rs.newWM[ct.Name] = headStr
			}
			continue
		}
		t0 := time.Now()
		rows, err := tr.DiffFacts(ctx, wh, head)
		rs.diffMS += time.Since(t0).Milliseconds()
		if err != nil {
			log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: diff failed; the range will be re-run")
			continue
		}
		if rs.rangeFrom == "" || w < rs.rangeFrom {
			rs.rangeFrom = w
		}
		if !linear {
			rs.nonlinear = true
		}
		rs.paths += len(rows)
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return false
			}
			episode := episodeOf(row.Change)
			rel := strings.TrimPrefix(row.Path, root+"/")
			var matched []*fact.CompiledTrigger
			for _, ct := range trigs {
				if ct.OnEpisode(episode) && ct.Matches(rel) {
					matched = append(matched, ct)
				}
			}
			if len(matched) == 0 {
				continue
			}
			// change is built ONCE per matched path and shared by every
			// trigger that matched it; its cost is knomit's (change_ms), not
			// the trigger's.
			c0 := time.Now()
			pf := d.buildChange(ctx, tr, head, wh, row.Path, episode, !linear, verifyOn, verifiedSet, instanceFP)
			rs.changeMS += time.Since(c0).Milliseconds()
			for _, ct := range matched {
				p := pf
				p.trig = ct
				rs.pending = append(rs.pending, p)
			}
		}
		for _, ct := range trigs {
			rs.newWM[ct.Name] = headStr
		}
	}
	return true
}

// verifiedBelow is the set of commits F09 has accepted for the upstream: the
// store's own cache when a fold filled it, else ONE history walk per run,
// reused while the anchor does not move (R2-1: the cache is nil after every
// restart until the first sync tick, after an operator accept, and for good
// on a verify-on repo with no origin).
func (d *triggerDispatcher) verifiedBelow(ctx context.Context, tr store.TriggerIndex, upstream string) map[plumbing.Hash]bool {
	anchor, cached, err := tr.VerifiedAnchor(ctx, upstream)
	if err != nil || anchor == plumbing.ZeroHash {
		return nil
	}
	if cached != nil {
		return cached
	}
	d.mu.Lock()
	if d.verified.anchor == anchor && d.verified.set != nil {
		set := d.verified.set
		d.mu.Unlock()
		return set
	}
	d.mu.Unlock()
	set, err := tr.AncestorSet(ctx, anchor)
	if err != nil {
		log.Warn().Err(err).Str("repo", d.repo).Msg("trigger dispatcher: anchor history walk failed; verified reads false this run")
		return nil
	}
	d.mu.Lock()
	d.verified.anchor, d.verified.set = anchor, set
	d.mu.Unlock()
	return set
}

// buildChange builds the `change` global for one matched path: the ORIGINAL
// commit via the treesame walk, its verified signer, `source`, `verified`,
// the trace trailer, the fact at the head (at the watermark for a retract) and
// `before` at the watermark.
func (d *triggerDispatcher) buildChange(ctx context.Context, tr store.TriggerIndex, head, wm plumbing.Hash, repoPath, episode string,
	nonlinear, verifyOn bool, verifiedSet map[plumbing.Hash]bool, instanceFP string) pendingFire {
	if delay := currentTriggerHooks().changeDelay; delay > 0 {
		time.Sleep(delay)
	}
	pf := pendingFire{repoPath: repoPath, episode: episode, nonlinear: nonlinear}
	author := map[string]any{"kind": "unknown", "id": "", "fp": "", "verified": false}
	commit, source, trace := head, "merged", ""

	// A retract in a NONLINEAR advance (a rewind replay) has no originating
	// commit in the new history, and the same-absent-blob walk would run to
	// the root: take the fallback directly.
	found := false
	if !(nonlinear && episode == fact.TriggerOnRetract) {
		res, err := tr.Toucher(ctx, head, repoPath)
		if err == nil && res.Found {
			commit, found = res.Commit, true
		} else if err != nil {
			log.Warn().Err(err).Str("repo", d.repo).Str("path", repoPath).Msg("trigger dispatcher: toucher walk failed; using the head")
		}
	}
	if found {
		info, err := tr.CommitInfo(ctx, commit)
		if err == nil {
			trace = store.TrailerValue(info.Message, triggerTraceTrailer)
			kind := "human"
			switch {
			case strings.HasSuffix(strings.ToLower(info.AuthorEmail), "@agents.knomit.io"):
				kind = "agent"
			case info.AuthorName == "" && info.AuthorEmail == "":
				kind = "unknown"
			}
			fp := ""
			if signer, err := tr.CommitSignerOf(ctx, commit); err == nil {
				fp = signer.Fingerprint
			}
			if fp != "" && fp == instanceFP {
				source = "local"
			}
			author = map[string]any{
				"kind":     kind,
				"id":       info.AuthorName,
				"fp":       fp,
				"verified": verifyOn && verifiedSet != nil && verifiedSet[commit],
			}
		}
	}
	pf.commit, pf.source, pf.trace = commit.String(), source, trace

	// fact: at the head, or at the watermark for a retract (the path is gone
	// at the head). before: the version at the WATERMARK — this machine's
	// last-processed version, not the toucher's parent, which could be an
	// intermediate version this machine never saw.
	factAt := head
	if episode == fact.TriggerOnRetract {
		factAt = wm
	}
	pf.factMap, pf.parseable = d.factGlobal(ctx, tr, factAt, repoPath)
	var before any
	if episode != fact.TriggerOnLearn {
		if m, ok := d.factGlobal(ctx, tr, wm, repoPath); ok {
			before = m
		}
	}
	pf.change = map[string]any{
		"episode": episode,
		"source":  source,
		"commit":  commit.String(),
		"trace":   trace,
		"path":    repoPath,
		"author":  author,
		"before":  before,
	}
	return pf
}

// factGlobal reads and parses the fact at commit; ok is false when absent or
// unparseable (the `if` then sees fact === null).
func (d *triggerDispatcher) factGlobal(ctx context.Context, tr store.TriggerIndex, commit plumbing.Hash, repoPath string) (map[string]any, bool) {
	content, ok, err := tr.BlobAt(ctx, commit, repoPath)
	if err != nil || !ok {
		return nil, false
	}
	f, err := fact.ParseFact(repoPath, content)
	if err != nil {
		return nil, false
	}
	return fact.FactGlobal(f), true
}

// phaseB evaluates every pending fire OUTSIDE the store: `if`, then emit. It
// returns the rows to log (every outcome but if-false), the evaluation and
// fire counts, and whether ctx cancelled the run.
func (d *triggerDispatcher) phaseB(ctx context.Context, rs *runState) (rows []store.TriggerFire, evaluated, fires int, aborted bool) {
	agent := map[string]any{"id": d.identity.Agent, "host": d.identity.Host, "fp8": d.identity.FP8, "branch": d.branch}
	for _, p := range rs.pending {
		if ctx.Err() != nil {
			return nil, 0, 0, true
		}
		var factGlobal any
		if p.factMap != nil {
			factGlobal = p.factMap
		}
		globals := map[string]any{"fact": factGlobal, "agent": agent, "change": p.change}

		// The trigger's OWN work starts here: its `if` and its action.
		t0 := time.Now()
		pass, err := p.trig.EvalIf(globals)
		outcome, errText := "", ""
		switch {
		case err != nil:
			var interrupted *goja.InterruptedError
			if errors.As(err, &interrupted) {
				outcome = store.TriggerOutcomeIfTimeout
			} else {
				outcome = store.TriggerOutcomeIfError
			}
			errText = err.Error()
		case !pass:
			outcome = store.TriggerOutcomeIfFalse
		default:
			outcome = store.TriggerOutcomeEmitted
			if !p.parseable {
				outcome = store.TriggerOutcomeUnparseable
			}
			d.emit(p)
		}
		elapsed := time.Since(t0)
		evaluated++
		if outcome == store.TriggerOutcomeEmitted || outcome == store.TriggerOutcomeUnparseable {
			fires++
		}
		slow := d.slow > 0 && elapsed > d.slow
		d.stats.record(p.trig.Name, outcome, elapsed, slow)
		if slow {
			log.Warn().
				Str("repo", d.repo).
				Str("branch", d.branch).
				Str("trigger", capForLog(p.trig.Name)).
				Str("path", capForLog(p.repoPath)).
				Str("episode", p.episode).
				Str("commit", shortHash(p.commit)).
				Str("outcome", outcome).
				Dur("elapsed", elapsed).
				Dur("threshold", d.slow).
				Int64("change_ms", rs.changeMS).
				Msg("slow trigger")
		}
		if outcome != store.TriggerOutcomeIfFalse {
			rows = append(rows, store.TriggerFire{
				Trigger: p.trig.Name, Path: p.repoPath, Episode: p.episode, Source: p.source,
				Commit: p.commit, Trace: p.trace, Outcome: outcome, Error: errText, Nonlinear: p.nonlinear,
			})
		}
	}
	return rows, evaluated, fires, false
}

// emit is the `emit` action: a log line and the SSE `trigger` event on the
// branch stream, published straight to the hub (no debounce).
func (d *triggerDispatcher) emit(p pendingFire) {
	log.Info().
		Str("repo", d.repo).
		Str("branch", d.branch).
		Str("trigger", capForLog(p.trig.Name)).
		Str("episode", p.episode).
		Str("source", p.source).
		Str("path", capForLog(p.repoPath)).
		Str("commit", shortHash(p.commit)).
		Str("trace", capForLog(p.trace)).
		Msg("trigger fired")
	if hub := d.ri.hub; hub != nil {
		hub.broadcastTrigger(TriggerEvent{
			Branch: d.branch, Trigger: p.trig.Name, Path: p.repoPath, Episode: p.episode,
			Source: p.source, Commit: p.commit, Trace: p.trace,
		})
	}
}

// nameStates folds the per-declaration states into one state per NAME: active
// when the winning declaration compiled; else unsupported when the winner is
// unsupported; else invalid (a duplicate's loser is invalid, so a duplicated
// active name stays active).
func nameStates(set *fact.TriggerSet) map[string]string {
	out := map[string]string{}
	for _, st := range set.States {
		if st.Name == "" {
			continue
		}
		cur, has := out[st.Name]
		switch {
		case st.State == fact.TriggerActive:
			out[st.Name] = fact.TriggerActive
		case st.State == fact.TriggerUnsupported && (!has || cur == fact.TriggerInvalid):
			out[st.Name] = fact.TriggerUnsupported
		case !has:
			out[st.Name] = fact.TriggerInvalid
		}
	}
	return out
}

func episodeOf(change string) string {
	switch change {
	case store.ChangeAdded:
		return fact.TriggerOnLearn
	case store.ChangeDeleted:
		return fact.TriggerOnRetract
	default:
		return fact.TriggerOnUpdate
	}
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func capForLog(s string) string {
	if len(s) <= maxTriggerLogField {
		return s
	}
	return s[:maxTriggerLogField] + "...[truncated]"
}

// recordSet remembers the last compiled set for the report.
func (d *triggerDispatcher) recordSet(set *fact.TriggerSet, head, blob string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastSet, d.lastHead, d.lastBlob = set, head, blob
}

func (d *triggerDispatcher) lastCompiledSet() *fact.TriggerSet {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastSet
}

// logInvalidOnce logs each INVALID trigger at ERROR once per (name, blob).
// Unsupported triggers are not errors and are not logged.
func (d *triggerDispatcher) logInvalidOnce(set *fact.TriggerSet) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.errBlob != set.Blob {
		d.errBlob, d.errLogged = set.Blob, map[string]bool{}
	}
	for _, st := range set.States {
		if st.State != fact.TriggerInvalid || d.errLogged[st.Key] {
			continue
		}
		d.errLogged[st.Key] = true
		name := st.Name
		if name == "" {
			name = "(unnamed)"
		}
		log.Error().
			Str("repo", d.repo).
			Str("branch", d.branch).
			Str("trigger", capForLog(name)).
			Str("topic", st.Node).
			Str("blob", set.Blob).
			Str("error", st.Error).
			Msg("invalid trigger skipped")
	}
}

// logOntologyErrorOnce reports an ontology at the head that cannot be read or
// parsed, once per blob (or head), keeping the last good trigger set.
func (d *triggerDispatcher) logOntologyErrorOnce(key string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ontErr == key {
		return
	}
	d.ontErr = key
	log.Error().Err(err).Str("repo", d.repo).Str("branch", d.branch).Str("blob", key).
		Msg("triggers: ontology at head unusable; keeping the last good trigger set")
}

func (d *triggerDispatcher) clearOntologyError() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ontErr = ""
}

// runSequence is how many runs have completed (any outcome). Tests wait on it.
func (d *triggerDispatcher) runSequence() (head string, seq int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastHead, d.runSeq
}

// ---- The report behind GET …/branches/{branch}/triggers.

// TriggerReport is the trigger resource of one repo: the ONLY statistics
// surface (user ruling D-d: no stats block, no CLI, no Prometheus counters).
type TriggerReport struct {
	Enabled  bool                `json:"enabled"`
	Reason   string              `json:"reason,omitempty"`
	Branch   string              `json:"branch"`
	Head     string              `json:"head,omitempty"`
	Blob     string              `json:"ontology_blob,omitempty"`
	Runs     TriggerRunStats     `json:"runs"`
	Triggers []TriggerView       `json:"triggers"`
	Fires    []store.TriggerFire `json:"fires"`
}

// TriggerView is one declared trigger as the endpoint shows it. State is
// active | invalid | unsupported | frozen, where frozen is an invalid trigger
// that holds a bookmark (its fires wait for the fix).
type TriggerView struct {
	Name      string           `json:"name"`
	Node      string           `json:"node"`
	Match     string           `json:"match,omitempty"`
	On        []string         `json:"on"`
	Do        string           `json:"do"`
	State     string           `json:"state"`
	Error     string           `json:"error,omitempty"`
	Watermark string           `json:"watermark,omitempty"`
	Stats     TriggerStatsView `json:"stats"`
}

// TriggerReport renders the repo's trigger resource. logN is how many recent
// fire rows to include (0 for none).
func (ri *RepoInstance) TriggerReport(ctx context.Context, logN int) (TriggerReport, error) {
	rep := TriggerReport{Branch: ri.agentBranch, Triggers: []TriggerView{}, Fires: []store.TriggerFire{}}
	d := ri.triggers
	if d == nil {
		switch {
		case ri.subscribed || ri.agentBranch == "":
			rep.Reason = "subscription: no agent branch to observe"
		default:
			rep.Reason = "read-only server"
		}
		return rep, nil
	}
	rep.Enabled = true
	rep.Runs = d.stats.runView()

	var wms map[string]string
	err := ri.WithRead(func(svc *store.Service) {
		var werr error
		wms, werr = svc.Triggers().TriggerWatermarks(ctx, d.branch)
		if werr != nil {
			return
		}
		if logN > 0 {
			rep.Fires, werr = svc.Triggers().RecentTriggerFires(ctx, d.branch, logN)
		}
		if werr != nil {
			log.Warn().Err(werr).Str("repo", ri.Name()).Msg("trigger report: fire log unreadable")
		}
	})
	if err != nil {
		return rep, err
	}

	d.mu.Lock()
	set, head, blob := d.lastSet, d.lastHead, d.lastBlob
	d.mu.Unlock()
	rep.Head, rep.Blob = head, blob
	if set == nil {
		return rep, nil
	}
	active := map[string]*fact.CompiledTrigger{}
	for _, ct := range set.Active {
		active[ct.Name] = ct
	}
	for _, st := range set.States {
		v := TriggerView{Name: st.Name, Node: st.Node, Match: st.Match, On: st.On, Do: st.Do, State: st.State, Error: st.Error}
		if v.On == nil {
			v.On = []string{}
		}
		if ct := active[st.Name]; ct != nil && st.State == fact.TriggerActive {
			v.Match = ct.Match
		}
		if wm, has := wms[st.Name]; has {
			v.Watermark = wm
			if st.State == fact.TriggerInvalid {
				v.State = "frozen"
			}
		}
		v.Stats = d.stats.view(st.Name)
		rep.Triggers = append(rep.Triggers, v)
	}
	sort.SliceStable(rep.Triggers, func(i, j int) bool { return rep.Triggers[i].Name < rep.Triggers[j].Name })
	return rep, nil
}

// String renders the identity for logs.
func (d *triggerDispatcher) String() string {
	return fmt.Sprintf("triggers(%s@%s)", d.repo, d.branch)
}
