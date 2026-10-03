package repos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// F07 PR 4: `do: push` and knomit.push() wake this machine's sync loop.
//
// The window is never real time here. The syncHooks.window seam records every
// countdown the loop opens (and the duration it asked for) and returns a
// channel the TEST releases. The burst tests put the countdown on a fake
// clock: an open at fake time t has its deadline at t+d, and advancing the
// clock releases exactly the windows whose deadline has passed — so "the
// round starts at the countdown's end, not 1 s after the last fire" is an
// assertion about fake instants, not a race against the scheduler.
//
// Ticks of a loop the test starts are counted by its resolveAuth (every
// doTick calls it once): against example.invalid it fails, so no network is
// touched and the count is the tick count. A negative "stays at N" check
// holds for pushQuiet AND asserts no further window opened — a wake that
// would become a tick must first open a window.

const pushQuiet = 300 * time.Millisecond

// pushTrig renders a `do: push` entry.
func pushTrig(name, on, match, cond string) string {
	e := fmt.Sprintf("      - name: %s\n        on: %s\n        do: push\n", name, on)
	if match != "" {
		e += fmt.Sprintf("        match: %q\n", match)
	}
	if cond != "" {
		e += fmt.Sprintf("        if: %q\n", cond)
	}
	return e
}

// setSyncHooks installs sync-loop seams for the test's duration.
func setSyncHooks(t *testing.T, h syncHooks) {
	t.Helper()
	syncHooksMu.Lock()
	syncTestHooks = h
	syncHooksMu.Unlock()
	t.Cleanup(func() {
		syncHooksMu.Lock()
		syncTestHooks = syncHooks{}
		syncHooksMu.Unlock()
	})
}

// windowSeam is the fake countdown. auto releases every open at once.
type windowSeam struct {
	mu      sync.Mutex
	auto    bool
	now     time.Duration
	durs    []time.Duration
	pending []heldWindow
}

type heldWindow struct {
	deadline time.Duration
	ch       chan time.Time
}

func (w *windowSeam) open(d time.Duration) <-chan time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.durs = append(w.durs, d)
	ch := make(chan time.Time, 1)
	if w.auto {
		ch <- time.Time{}
		return ch
	}
	w.pending = append(w.pending, heldWindow{deadline: w.now + d, ch: ch})
	return ch
}

func (w *windowSeam) opened() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.durs)
}

func (w *windowSeam) durations() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.durs...)
}

// advanceTo moves the fake clock and releases every window now due.
func (w *windowSeam) advanceTo(at time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = at
	keep := w.pending[:0]
	for _, h := range w.pending {
		if h.deadline <= at {
			h.ch <- time.Time{}
		} else {
			keep = append(keep, h)
		}
	}
	w.pending = keep
}

// releaseAll releases every held window, whatever its deadline.
func (w *windowSeam) releaseAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, h := range w.pending {
		h.ch <- time.Time{}
	}
	w.pending = nil
}

// holdWindows installs a held (fake-clock) window seam, keeping tick.
func holdWindows(t *testing.T, auto bool, tick func(context.Context, string)) *windowSeam {
	t.Helper()
	w := &windowSeam{auto: auto}
	setSyncHooks(t, syncHooks{window: w.open, tick: tick})
	return w
}

func waitOpens(t *testing.T, w *windowSeam, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return w.opened() >= n }, 10*time.Second, 5*time.Millisecond,
		"the loop never opened countdown #%d", n)
	require.Equal(t, n, w.opened(), "exactly %d countdown(s)", n)
}

// countingLoop is runReconcileLoop against example.invalid with a counting,
// failing resolveAuth, the instance's own dispatcher kick and its push wake.
type countingLoop struct {
	ticks atomic.Int64
	gates sync.Map  // tick number → chan struct{} it parks on
	mode  *syncMode // the `sync` mode the loop follows (nil: today)
}

func (l *countingLoop) parkTick(n int64) (release func()) {
	gate := make(chan struct{})
	l.gates.Store(n, gate)
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

func startCountingLoop(t *testing.T, ri *RepoInstance, pre func(*countingLoop)) *countingLoop {
	t.Helper()
	svc := testService(t, ri)
	svc.SetOrigin(&store.Origin{URL: "https://example.invalid/kb.git", Branch: "main"})
	l := &countingLoop{}
	if pre != nil {
		pre(l)
	}
	auth := func(*store.Remote) (transport.AuthMethod, error) {
		n := l.ticks.Add(1)
		if g, ok := l.gates.Load(n); ok {
			<-g.(chan struct{})
		}
		return nil, errors.New("no credential")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runReconcileLoop(ctx, svc, ri.hub, ri.Name(), trigAgent, auth, "", false, nil, ri.triggerKick, ri.syncWake, ri.breakers, l.mode, false)
	}()
	t.Cleanup(func() {
		cancel()
		l.gates.Range(func(_, g any) bool {
			select {
			case <-g.(chan struct{}):
			default:
				close(g.(chan struct{}))
			}
			return true
		})
		wg.Wait()
	})
	return l
}

func waitTicks(t *testing.T, l *countingLoop, n int64) {
	t.Helper()
	require.Eventually(t, func() bool { return l.ticks.Load() >= n }, 10*time.Second, 5*time.Millisecond,
		"tick #%d never ran", n)
	require.Equal(t, n, l.ticks.Load(), "exactly %d tick(s)", n)
}

// holdsAt asserts that neither the tick count nor the countdown count moves
// for the quiet period.
func holdsAt(t *testing.T, l *countingLoop, ticks int64, w *windowSeam, opens int, msg string) {
	t.Helper()
	time.Sleep(pushQuiet)
	require.Equal(t, ticks, l.ticks.Load(), "ticks: %s", msg)
	require.Equal(t, opens, w.opened(), "countdowns: %s", msg)
}

func kickedOf(t *testing.T, ri *RepoInstance, name string) []store.TriggerFire {
	t.Helper()
	var out []store.TriggerFire
	for _, f := range firesOf(t, ri, name) {
		if f.Outcome == store.TriggerOutcomeKicked {
			out = append(out, f)
		}
	}
	return out
}

// ---- D-window: the countdown starts at the FIRST fire and is not a debounce.

// BurstIsOneRound (T1, D-window burst): three push fires at fake 0, 400 and
// 900 ms open ONE countdown of exactly 1 s; nothing runs at 999 ms; exactly
// one round runs at 1000 ms — the end of the countdown the FIRST fire opened,
// not 1 s after the last fire — and nothing follows. Sabotage: restart the
// countdown on each fire (a debounce) → a second countdown opens and nothing
// runs at 1000 ms; tick on the first receive (no window) → the round runs
// before release.
func TestPush_BurstIsOneRound(t *testing.T) {
	_, ri := newTriggerRepo(t, pushTrig("fast", "learn", "", ""))
	w := holdWindows(t, false, nil)
	l := startCountingLoop(t, ri, nil)
	waitTicks(t, l, 1)

	write(t, ri, "kb/tasks/a.md") // fake 0 ms
	waitOpens(t, w, 1)
	w.advanceTo(400 * time.Millisecond)
	write(t, ri, "kb/tasks/b.md")
	w.advanceTo(900 * time.Millisecond)
	write(t, ri, "kb/tasks/c.md")
	require.Len(t, kickedOf(t, ri, "fast"), 3, "three push fires, each recorded kicked")
	require.Equal(t, []time.Duration{time.Second}, w.durations(),
		"ONE countdown, of exactly 1 s: fires during it neither restart nor extend it")

	w.advanceTo(999 * time.Millisecond)
	holdsAt(t, l, 1, w, 1, "no round before the countdown ends")

	w.advanceTo(time.Second)
	waitTicks(t, l, 2)
	holdsAt(t, l, 2, w, 1, "one round carries the whole burst; nothing follows it")
}

// FollowUpDuringRound (D-window follow-up): the round the burst started is
// parked; a fire at fake 1.2 s lands while it runs; when it returns exactly
// ONE follow-up round runs after its own countdown, and nothing after that.
// Sabotage: drain the slot after doTick → no follow-up; a countdown opened
// per fire → more than one.
func TestPush_FollowUpDuringRound(t *testing.T) {
	_, ri := newTriggerRepo(t, pushTrig("fast", "learn", "", ""))
	w := holdWindows(t, false, nil)
	var release func()
	l := startCountingLoop(t, ri, func(l *countingLoop) { release = l.parkTick(2) })
	waitTicks(t, l, 1)

	write(t, ri, "kb/tasks/a.md") // fake 0 ms
	waitOpens(t, w, 1)
	w.advanceTo(time.Second)
	waitTicks(t, l, 2) // the round is running, parked in resolveAuth

	w.advanceTo(1200 * time.Millisecond)
	write(t, ri, "kb/tasks/b.md")
	require.Len(t, kickedOf(t, ri, "fast"), 2)
	holdsAt(t, l, 2, w, 1, "nothing opens while the round runs")

	release()
	waitOpens(t, w, 2)
	holdsAt(t, l, 2, w, 2, "the follow-up waits for its own countdown")
	w.advanceTo(2200 * time.Millisecond)
	waitTicks(t, l, 3)
	holdsAt(t, l, 3, w, 2, "exactly one follow-up round")
}

// KickDuringTickIsOneFollowUp (T2): three fires while a round is parked give
// exactly one follow-up (not zero, not three). Sabotage: drain after doTick
// → 2; an N-slot channel or one tick per kick → more.
func TestPush_KickDuringTickIsOneFollowUp(t *testing.T) {
	_, ri := newTriggerRepo(t, pushTrig("fast", "learn", "", ""))
	w := holdWindows(t, false, nil)
	var release func()
	l := startCountingLoop(t, ri, func(l *countingLoop) { release = l.parkTick(2) })
	waitTicks(t, l, 1)

	write(t, ri, "kb/tasks/a.md")
	waitOpens(t, w, 1)
	w.releaseAll()
	waitTicks(t, l, 2) // parked

	for _, p := range []string{"kb/tasks/b.md", "kb/tasks/c.md", "kb/tasks/d.md"} {
		write(t, ri, p)
	}
	require.Len(t, kickedOf(t, ri, "fast"), 4)
	release()
	waitOpens(t, w, 2)
	w.releaseAll()
	waitTicks(t, l, 3)
	holdsAt(t, l, 3, w, 2, "three fires during a round are one follow-up")
}

// LoopStartDrain (T11): a wake already in the slot when the loop starts is
// covered by the loop's first tick — no countdown opens, no second tick.
// Sabotage: drop the drain before the first tick → a countdown opens and a
// second tick follows.
func TestPush_LoopStartDrain(t *testing.T) {
	_, ri := newTriggerRepo(t, pushTrig("fast", "learn", "", ""))
	w := holdWindows(t, false, nil)
	write(t, ri, "kb/tasks/before-the-loop.md") // the fire fills the slot; no loop yet
	require.Len(t, kickedOf(t, ri, "fast"), 1)
	require.Len(t, ri.syncWake, 1, "fixture: the wake is waiting in the slot")

	l := startCountingLoop(t, ri, nil)
	waitTicks(t, l, 1)
	holdsAt(t, l, 1, w, 0, "the first tick covers the leftover wake")
	require.Len(t, ri.syncWake, 0, "the leftover wake was drained, not left for later")
}

// ---- D-local: no origin wakes the local loop.

// NoOriginWakesLocalLoop (T3): with no origin a push fire wakes the local
// loop; after the countdown it runs the ticker's body — kickTriggers (counted),
// then advance. An origin that appears answers the wake with a skipped
// advance, never an advance (the two loops stay mutually exclusive) and never
// an exit (the loop ends only on its ctx). Sabotage: no wake arm in the local
// loop → no advance; skip kickTriggers in the wake arm → the kick count stays;
// drop the origin check → the wake advances main behind reconcileMain.
func TestPush_NoOriginWakesLocalLoop(t *testing.T) {
	_, ri := newTriggerRepo(t, pushTrig("fast", "learn", "", ""))
	w := holdWindows(t, false, nil)
	var advances, kicks atomic.Int64
	var origin atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLocalReconcile(ctx, "repo", trigAgent, time.Hour, nil, false,
			func() (bool, error) { return origin.Load(), nil },
			func() error { advances.Add(1); return nil },
			func() { kicks.Add(1) },
			ri.syncWake)
	}()
	require.Eventually(t, func() bool { return advances.Load() == 1 && kicks.Load() == 1 },
		10*time.Second, 5*time.Millisecond, "the start tick")

	write(t, ri, "kb/tasks/a.md")
	require.Len(t, kickedOf(t, ri, "fast"), 1, "kicked, like any push fire (D-local)")
	waitOpens(t, w, 1)
	time.Sleep(pushQuiet)
	require.Equal(t, int64(1), advances.Load(), "nothing before the countdown ends")
	w.releaseAll()
	require.Eventually(t, func() bool { return advances.Load() == 2 }, 10*time.Second, 5*time.Millisecond,
		"the wake must advance main")
	require.Equal(t, int64(2), kicks.Load(), "the wake round calls kickTriggers, like a ticker round")
	time.Sleep(pushQuiet)
	require.Equal(t, int64(2), advances.Load())
	require.Equal(t, 1, w.opened())

	origin.Store(true)
	write(t, ri, "kb/tasks/b.md")
	waitOpens(t, w, 2)
	w.releaseAll()
	require.Eventually(t, func() bool { return kicks.Load() == 3 }, 10*time.Second, 5*time.Millisecond,
		"the wake round ran (it kicks the dispatcher first)")
	time.Sleep(pushQuiet)
	select {
	case <-done:
		t.Fatal("a wake on a repo that gained an origin must skip, not stop the local loop")
	default:
	}
	require.Equal(t, int64(2), advances.Load(), "an origin-backed repo's main is reconcileMain's, never advanced here")
	cancel()
	<-done
}

// ---- The push action itself.

// NoPushTriggerNoExtraTick (T4): a repo with only emit triggers and a script
// that calls nothing never wakes the loop. Sabotage: d.wakeSync() for every
// phase B fire (or from triggerKick) → countdowns open.
func TestPush_NoPushTriggerNoExtraTick(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0, trig("all", "learn", "", ""), scriptTrig("quiet", "learn", "", "noop"))
	putScript(t, ri, "noop", `var x = 1;`)
	w := holdWindows(t, false, nil)
	l := startCountingLoop(t, ri, nil)
	waitTicks(t, l, 1)
	for i := 0; i < 5; i++ {
		write(t, ri, fmt.Sprintf("kb/tasks/t%d.md", i))
	}
	require.Len(t, firesOf(t, ri, "all"), 5, "fixture: the emit trigger fired on every write")
	require.Len(t, firesOf(t, ri, "quiet"), 5, "fixture: the script ran on every write")
	holdsAt(t, l, 1, w, 0, "no push trigger, no wake")
}

// ScriptKnomitPushKicks (T5): knomit.push() three times in one run is one
// wake → one countdown → one round; it returns {ok:true, status:"kicked"};
// the fire is ONE `ran` row. Sabotage: keep the stub, or return kicked
// without sending → no countdown; record a row per push → more rows.
func TestPush_ScriptKnomitPushKicks(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0, scriptTrig("pusher", "learn", "tasks/**", "pusher"))
	putScript(t, ri, "pusher", `knomit.emit({a: knomit.push(), b: knomit.push(), c: knomit.push("agent/other")});`)
	sink := subscribePayloads(t, ri, "pusher")
	w := holdWindows(t, false, nil)
	l := startCountingLoop(t, ri, nil)
	waitTicks(t, l, 1)

	write(t, ri, "kb/tasks/x.md")
	p := sink.wait(t, 1)[0]
	kicked := map[string]any{"ok": true, "status": "kicked"}
	require.Equal(t, kicked, p["a"])
	require.Equal(t, kicked, p["b"])
	require.Equal(t, kicked, p["c"], "arguments are ignored: nothing can name a branch")
	require.Equal(t, []string{store.TriggerOutcomeRan}, outcomesOf(firesOf(t, ri, "pusher")), "one fire, ran")

	waitOpens(t, w, 1)
	w.releaseAll()
	waitTicks(t, l, 2)
	holdsAt(t, l, 2, w, 1, "three pushes in one run are one round")
}

// StatisticsAndLog (T7): two push fires and one if-false. Rows: two
// `kicked`; stats: evaluations 3, fires 2, if_false 1; no SSE `trigger` event;
// the fires write nothing to git (the head is the last write's commit).
// Sabotage: route push through `default:` (emitted + an SSE event); drop the
// stats case (fires 0); count kicked in phase B's sum only.
func TestPush_StatisticsAndLog(t *testing.T) {
	_, ri := newTriggerRepo(t, pushTrig("fast", "learn", "", "change.path !== 'kb/tasks/skip.md'"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, _ := ri.TaskHub().Subscribe(ctx)

	write(t, ri, "kb/tasks/a.md")
	write(t, ri, "kb/tasks/skip.md")
	last := write(t, ri, "kb/tasks/b.md")

	rows := firesOf(t, ri, "fast")
	require.Equal(t, []string{store.TriggerOutcomeKicked, store.TriggerOutcomeKicked}, outcomesOf(rows))
	require.ElementsMatch(t, []string{"kb/tasks/a.md", "kb/tasks/b.md"}, pathsOf(rows))
	for _, r := range rows {
		require.Empty(t, r.Error)
	}
	st := ri.triggers.stats.view("fast")
	require.Equal(t, int64(3), st.Evaluations)
	require.Equal(t, int64(2), st.Fires, "a kicked fire is a fire")
	require.Equal(t, int64(1), st.IfFalse)
	require.Equal(t, last, head(t, ri), "a push fire writes nothing to git")

	deadline := time.After(pushQuiet)
	for {
		select {
		case e := <-events:
			if ev, ok := e.(TriggerEvent); ok {
				t.Fatalf("a push fire must not emit an SSE trigger event, got %+v", ev)
			}
		case <-deadline:
			return
		}
	}
}

// KickSendNeverBlocks (T9) [M2]: the round is parked in resolveAuth — BEFORE
// Sync/Push take any lock, so this tests the send only (a script write CAN
// wait on the agent-branch lock during a real push: Risk 6). Ten more fires
// all record `kicked` and the dispatcher keeps completing runs. Sabotage: a
// blocking send → the second fire into the full slot hangs the dispatcher.
func TestPush_KickSendNeverBlocks(t *testing.T) {
	_, ri := newTriggerRepo(t, pushTrig("fast", "learn", "", ""))
	holdWindows(t, true, nil)
	l := startCountingLoop(t, ri, func(l *countingLoop) { l.parkTick(2) })
	waitTicks(t, l, 1)
	write(t, ri, "kb/tasks/first.md")
	waitTicks(t, l, 2) // parked: nobody receives on the wake now

	for i := 0; i < 10; i++ {
		h := writeOn(t, ri, trigAgent, fmt.Sprintf("kb/tasks/n%d.md", i))
		waitTriggerHeadFor(t, ri, h, 5*time.Second)
	}
	require.Len(t, kickedOf(t, ri, "fast"), 11)
	require.Equal(t, int64(2), l.ticks.Load(), "the round is still parked")
}

// ---- A real origin: what a kicked round pushes, and the cycle settles.

// newOriginTriggerRepo is a clone-mode repo on a real local bare origin
// (file://, under the manager's LocalOriginRoot), background sync off, with
// the given triggers installed on the agent branch.
func newOriginTriggerRepo(t *testing.T, entries ...string) (ri *RepoInstance, bare, originRoot string) {
	t.Helper()
	dir := t.TempDir()
	url := seedBareRemote(t, filepath.Join(dir, "remote.git"))
	home := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: dir},
		AgentBranch: trigAgent,
		KeyPath:     filepath.Join(home, "agent.key"),
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	ri, err := m.Create(context.Background(), CreateSpec{Name: testRepoName, Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main"}}, nil)
	require.NoError(t, err)
	setOntology(t, ri, triggerOntology("", entries...))
	return ri, bareOf(url), dir
}

// startOriginLoop runs runReconcileLoop against the real origin; resolveAuth
// passes through (anonymous file://) and counts the ticks.
func startOriginLoop(t *testing.T, ri *RepoInstance, originRoot string) *atomic.Int64 {
	t.Helper()
	var ticks atomic.Int64
	auth := func(*store.Remote) (transport.AuthMethod, error) { ticks.Add(1); return nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runReconcileLoop(ctx, testService(t, ri), ri.hub, ri.Name(), trigAgent, auth, originRoot, false, nil, ri.triggerKick, ri.syncWake, ri.breakers, nil, false)
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })
	return &ticks
}

// bareRef reads a ref in the bare origin ("" when absent).
func bareRef(t *testing.T, bare, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "rev-parse", "--verify", "-q", ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func bareHeads(t *testing.T, bare string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "for-each-ref", "--format=%(refname)", "refs/heads").Output()
	require.NoError(t, err)
	heads := strings.Fields(string(out))
	sort.Strings(heads)
	return heads
}

// OwnBranchOnly (T8): a kicked round pushes this machine's agent branch and
// nothing else — a peer branch present locally (as F11 registers one) never
// reaches the origin, and main is never pushed. Sabotage: widen the refspec
// to agent/* → the peer branch appears on the origin.
func TestPush_OwnBranchOnly(t *testing.T) {
	ri, bare, root := newOriginTriggerRepo(t, pushTrig("fast", "learn", "", ""))
	holdWindows(t, true, nil)
	mainBefore := bareRef(t, bare, "refs/heads/main")
	ticks := startOriginLoop(t, ri, root)
	require.Eventually(t, func() bool { return bareRef(t, bare, "refs/heads/"+trigAgent) == head(t, ri) },
		20*time.Second, 10*time.Millisecond, "the first tick pushes the agent branch")
	base := ticks.Load()

	svc := testService(t, ri)
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "agent/peer-0badc0de", trigAgent))
	h := write(t, ri, "kb/tasks/a.md")
	require.Len(t, kickedOf(t, ri, "fast"), 1)
	require.Eventually(t, func() bool { return bareRef(t, bare, "refs/heads/"+trigAgent) == h },
		20*time.Second, 10*time.Millisecond, "the kicked round pushes the new head")
	require.Greater(t, ticks.Load(), base, "a kicked round ran")
	require.Equal(t, []string{"refs/heads/" + trigAgent, "refs/heads/main"}, bareHeads(t, bare),
		"only this machine's branch is pushed; never a peer's, never a new ref")
	require.Equal(t, mainBefore, bareRef(t, bare, "refs/heads/main"), "main is never pushed")
}

// TickDispatcherCycleSettles (T12) [M1][M4]: tick → merge → dispatcher →
// push fire (on the merged path; the trigger has NO source filter) → tick
// converges. phase C's buffer is held pending (flushGrace 1 h), so every
// tick-kicked run meets it and only overlayPending stops a re-fire. Ticks
// settle at ≤ initial + 1 (the local write) + 1 (the merged path) and stay;
// the in-memory fire count is exactly 2; after stop each (trigger, path,
// commit) row is unique; the origin has the local head. Sabotage: (a) skip
// overlayPending in phase A → the tick-kicked run re-fires the buffered range
// and the counts climb; (b) wakeSync from triggerKick → every tick's deferred
// kick starts another tick.
func TestPush_TickDispatcherCycleSettles(t *testing.T) {
	ri, bare, root := newOriginTriggerRepo(t, pushTrig("fast", "learn", "", ""))
	holdWindows(t, true, nil)
	ticks := startOriginLoop(t, ri, root)
	require.Eventually(t, func() bool { return bareRef(t, bare, "refs/heads/"+trigAgent) == head(t, ri) },
		20*time.Second, 10*time.Millisecond, "the first tick pushes the agent branch")
	initial := ticks.Load()
	h := currentTriggerHooks()
	h.flushGrace = time.Hour
	setHooks(t, h)

	// (1) a peer's fact lands on origin main.
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	require.NoError(t, os.MkdirAll(filepath.Join(work, "kb", "tasks"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, "kb", "tasks", "peer.md"), []byte(factBody("peer")), 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "learn: kb/tasks/peer.md")
	runGit(t, work, "push", "origin", "main")

	// (2) one local write.
	writeOn(t, ri, trigAgent, "kb/tasks/local.md")

	stats := func() int64 { return ri.triggers.stats.view("fast").Fires }
	require.Eventually(t, func() bool {
		return stats() == 2 && bareRef(t, bare, "refs/heads/"+trigAgent) == head(t, ri)
	}, 20*time.Second, 10*time.Millisecond, "the local fire and the merged-path fire, and the merge pushed")
	var settled int64
	for i := 0; i < 3; i++ {
		time.Sleep(pushQuiet)
		now := ticks.Load()
		require.LessOrEqual(t, now, initial+2, "at most one kicked tick per fire: the cycle must settle")
		if i > 0 {
			require.Equal(t, settled, now, "the tick count is stable")
		}
		settled = now
		require.Equal(t, int64(2), stats(), "no fire repeats")
	}
	require.Equal(t, head(t, ri), bareRef(t, bare, "refs/heads/"+trigAgent))

	restartServe(t, ri) // the Serve exit runs flushOnStop, which writes the held buffer
	seen := map[string]bool{}
	rows := firesOf(t, ri, "fast")
	for _, r := range rows {
		k := r.Trigger + "|" + r.Path + "|" + r.Commit
		require.False(t, seen[k], "a fire repeated: %s", k)
		seen[k] = true
	}
	require.ElementsMatch(t, []string{"kb/tasks/local.md", "kb/tasks/peer.md"}, pathsOf(rows))
	sources := map[string]string{}
	for _, r := range rows {
		sources[r.Path] = r.Source
	}
	require.Equal(t, map[string]string{"kb/tasks/local.md": "local", "kb/tasks/peer.md": "merged"}, sources)
}

// ---- The Sync stage wires the SAME slot into the loop it starts.

// SyncStageWiresLocalLoop (T13): background sync on, no origin, a 1 h local
// interval. A push fire moves main to the agent head through the loop that
// Sync.Enter started — the only test through that call site. Sabotage: pass a
// nil wake there → main stays behind for the hour.
func TestPush_SyncStageWiresLocalLoop(t *testing.T) {
	var localTicks atomic.Int64
	holdWindows(t, true, func(_ context.Context, repo string) {
		if repo == testRepoName {
			localTicks.Add(1)
		}
	})
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: home, OntologyRoot: "kb",
			Git: config.GitConfig{LocalReconcileInterval: time.Hour}},
		AgentBranch: trigAgent,
		KeyPath:     filepath.Join(home, "agent.key"),
		// Machine.Synchronous deliberately NOT set: the stage's loop is the point.
	})
	t.Cleanup(func() { _ = m.Close() })
	ri := bootRepo(t, m)
	setOntology(t, ri, triggerOntology("", pushTrig("fast", "learn", "", "")))
	require.Eventually(t, func() bool { return localTicks.Load() >= 1 }, 20*time.Second, 10*time.Millisecond,
		"the Sync stage's local loop ran its start tick")
	require.Equal(t, int64(1), localTicks.Load())

	h := write(t, ri, "kb/tasks/a.md")
	require.Len(t, kickedOf(t, ri, "fast"), 1)
	svc := testService(t, ri)
	require.Eventually(t, func() bool {
		m, err := svc.Branches().HeadCommit(context.Background(), svc.UpstreamBranch())
		return err == nil && m == h && localTicks.Load() == 2
	}, 10*time.Second, 10*time.Millisecond, "a push fire must move main to the agent head within the countdown, not the hour")
}

// ---- The slot survives a Sync stage restart.

// KickAcrossSyncRestart (coordinator R1): the running loop's round is parked;
// a push fire fills the slot; an AttachOrigin restarts Sync, cancelling that
// loop and starting a new one. The dying loop does not consume the wake (ctx
// check at the loop top), the new loop's first tick drains it and covers the
// commit — EXACTLY one tick, no countdown — and the origin has it. A fire
// after the restart still reaches the new loop: one countdown, one tick.
// Sabotage: Sync.Enter passes nil (or a fresh channel) → the post-restart fire
// never opens a countdown; drop the loop-start drain → a second tick follows.
func TestPush_KickAcrossSyncRestart(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemote(t, filepath.Join(dir, "remote.git"))
	bare := bareOf(url)
	home := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))

	var ticks, parkAt atomic.Int64
	w := holdWindows(t, false, func(ctx context.Context, repo string) {
		if repo != testRepoName {
			return
		}
		if n := ticks.Add(1); n == parkAt.Load() {
			<-ctx.Done() // parked until the Sync restart cancels this loop
		}
	})
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: dir},
		AgentBranch: trigAgent,
		KeyPath:     filepath.Join(home, "agent.key"),
		// Machine.Synchronous deliberately NOT set: the restarted loop is the point.
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	ri, err := m.Create(context.Background(), CreateSpec{Name: testRepoName, Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main"}}, nil)
	require.NoError(t, err)
	// Keep the repo's own ontology id: the attach guard refuses an origin whose
	// agent branch is governed by a different knowledge base.
	require.NotNil(t, ri.Ontology())
	setOntology(t, ri, strings.Replace(triggerOntology("", pushTrig("fast", "learn", "", "")),
		"id: trig\n", "id: "+ri.Ontology().ID+"\n", 1))

	// Let the loop(s) the create started settle (a periodic tick is 300 s away).
	var base int64
	require.Eventually(t, func() bool {
		b := ticks.Load()
		time.Sleep(pushQuiet)
		base = ticks.Load()
		return b >= 1 && b == base
	}, 20*time.Second, 10*time.Millisecond)
	require.Equal(t, 0, w.opened())
	parkAt.Store(base + 1)

	write(t, ri, "kb/tasks/a.md")
	waitOpens(t, w, 1)
	w.releaseAll()
	require.Eventually(t, func() bool { return ticks.Load() == base+1 }, 10*time.Second, 5*time.Millisecond,
		"the kicked round starts and parks")

	hb := write(t, ri, "kb/tasks/b.md") // lands while the round is parked: the slot fills
	require.Len(t, ri.syncWake, 1, "fixture: the wake waits in the slot")
	_, err = m.Send(context.Background(), ri, AttachOrigin(OriginSpec{URL: url, Branch: "main"}))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return bareRef(t, bare, "refs/heads/"+trigAgent) == hb },
		20*time.Second, 10*time.Millisecond, "the new loop's first tick carries the commit the wake was for")
	time.Sleep(pushQuiet)
	require.Equal(t, base+2, ticks.Load(), "exactly one tick across the restart")
	require.Equal(t, 1, w.opened(), "the leftover wake is drained by the first tick, not a second round")

	hc := write(t, ri, "kb/tasks/c.md")
	waitOpens(t, w, 2)
	w.releaseAll()
	require.Eventually(t, func() bool { return bareRef(t, bare, "refs/heads/"+trigAgent) == hc },
		20*time.Second, 10*time.Millisecond, "after the restart a fire still reaches the loop")
	time.Sleep(pushQuiet)
	require.Equal(t, base+3, ticks.Load())
	require.Equal(t, 2, w.opened())
}
