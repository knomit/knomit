package repos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
)

// F21 S2: the `sync: {push, pull}` root attribute.
//
// No real waits. syncHooks.wait records every wait a loop CHOOSES and holds
// it until the test releases it, so "the wait is 7 s" is an assertion about
// the loop's choice, and a timer round happens only when the test says so.
// syncHooks.window is PR 4's held countdown, so "a commit opened a countdown"
// is counted, not raced.

// waitSeam is the fake between-rounds wait: it records each duration a loop
// asked for and holds the channel until released.
type waitSeam struct {
	mu   sync.Mutex
	durs []time.Duration
	held []chan time.Time
}

func (w *waitSeam) open(d time.Duration) <-chan time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.durs = append(w.durs, d)
	ch := make(chan time.Time, 1)
	w.held = append(w.held, ch)
	return ch
}

func (w *waitSeam) waits() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.durs...)
}

func (w *waitSeam) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.durs)
}

// releaseAll ends every wait handed out so far (a timer round, for the loop
// that is waiting on the newest one).
func (w *waitSeam) releaseAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.held {
		select {
		case ch <- time.Time{}:
		default:
		}
	}
	w.held = nil
}

func waitForWaits(t *testing.T, w *waitSeam, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return w.count() >= n }, 20*time.Second, 5*time.Millisecond,
		"the loop never chose wait #%d", n)
	require.Equal(t, n, w.count(), "exactly %d wait(s) chosen", n)
}

// modeSeams installs a held countdown and a held wait (and an optional tick
// observer) for the test.
func modeSeams(t *testing.T, autoWindow bool, tick func(context.Context, string)) (*windowSeam, *waitSeam) {
	t.Helper()
	win := &windowSeam{auto: autoWindow}
	ws := &waitSeam{}
	setSyncHooks(t, syncHooks{window: win.open, wait: ws.open, tick: tick})
	return win, ws
}

// syncRoot renders the root attributes block setting sync.
func syncRoot(push, pull string) string {
	s := "attributes:\n  sync:\n"
	if push != "" {
		s += "    push: " + push + "\n"
	}
	if pull != "" {
		s += "    pull: " + pull + "\n"
	}
	return s
}

// putOntologyOn commits yaml as the ontology on branch (any branch: the
// consensus branch, the agent branch, a test's "trunk").
func putOntologyOn(t *testing.T, ri *RepoInstance, branch, yaml string) string {
	t.Helper()
	r, err := testService(t, ri).Facts().WriteFact(context.Background(), branch, OntologyPath, yaml, "ontology", "updated")
	require.NoError(t, err)
	return r.CommitHash
}

func modeFor(t *testing.T, ri *RepoInstance, agent string, n time.Duration) *syncMode {
	t.Helper()
	return newSyncMode(testService(t, ri), ri.Name(), agent, false, n, &ri.realtimePush)
}

// startModeLoop is startCountingLoop with a chosen agent branch and consensus
// branch, and a sync mode: runReconcileLoop against example.invalid whose
// counting resolveAuth fails (so no network, and the count is the round
// count), the instance's own kick and wake.
func startModeLoop(t *testing.T, ri *RepoInstance, agent, upstream string, mode *syncMode) *countingLoop {
	t.Helper()
	svc := testService(t, ri)
	svc.SetOrigin(&store.Origin{URL: "https://example.invalid/kb.git", Branch: upstream})
	l := &countingLoop{mode: mode}
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
	go runReconcileLoop(ctx, &wg, svc, ri.hub, ri.Name(), agent, auth, "", false, nil, ri.triggerKick, ri.syncWake, ri.breakers, mode)
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

// startRealModeLoop is startOriginLoop with a sync mode: a real local bare
// origin, anonymous file:// auth that counts the rounds.
func startRealModeLoop(t *testing.T, ri *RepoInstance, originRoot string, mode *syncMode) *atomic.Int64 {
	t.Helper()
	var ticks atomic.Int64
	auth := func(*store.Remote) (transport.AuthMethod, error) { ticks.Add(1); return nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go runReconcileLoop(ctx, &wg, testService(t, ri), ri.hub, ri.Name(), trigAgent, auth, originRoot, false, nil, ri.triggerKick, ri.syncWake, ri.breakers, mode)
	t.Cleanup(func() { cancel(); wg.Wait() })
	return &ticks
}

// pushToBareMain commits files onto the bare origin's main, as a peer or the
// forge would.
func pushToBareMain(t *testing.T, bare string, files map[string]string) {
	t.Helper()
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	for p, body := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(work, p), []byte(body), 0o644))
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "peer")
	runGit(t, work, "push", "origin", "main")
}

func flagIs(t *testing.T, ri *RepoInstance, want bool, msg string) {
	t.Helper()
	require.Eventually(t, func() bool { return ri.realtimePush.Load() == want }, 20*time.Second, 5*time.Millisecond, msg)
}

// ---- push: realtime

// T1 SyncMode_PushRealtimeEveryCommit: `push: realtime` at the consensus
// branch's tip; a plain learn — no trigger at all — opens the countdown
// exactly once, nothing runs before it ends, and its release runs exactly one
// round. Sabotage: remove the onCommit wake line → no countdown → red.
func TestSyncMode_PushRealtimeEveryCommit(t *testing.T) {
	_, ri := newTriggerRepo(t)
	putOntologyOn(t, ri, "main", triggerOntology(syncRoot(fact.SyncRealtime, "")))
	win, ws := modeSeams(t, false, nil)
	l := startModeLoop(t, ri, trigAgent, "main", modeFor(t, ri, trigAgent, 0))
	waitTicks(t, l, 1)
	waitForWaits(t, ws, 1)
	flagIs(t, ri, true, "the loop publishes push: realtime")
	require.Equal(t, []time.Duration{300 * time.Second}, ws.waits(), "push alone leaves the pull cadence alone")

	write(t, ri, "kb/tasks/a.md")
	waitOpens(t, win, 1)
	require.Equal(t, []time.Duration{pushWakeWindow}, win.durations())
	time.Sleep(pushQuiet)
	require.Equal(t, int64(1), l.ticks.Load(), "nothing runs before the countdown ends")
	win.releaseAll()
	waitTicks(t, l, 2)
	holdsAt(t, l, 2, win, 1, "one commit, one countdown, one round")
}

// T2 SyncMode_AbsentIsToday: no `sync` key. Five learns open no countdown,
// the flag stays false and the recorded wait is the origin's 300 s.
// Sabotage: default the flag to true (publish true when absent) → countdowns
// open; use the realtime interval when absent → the wait is 3 s.
func TestSyncMode_AbsentIsToday(t *testing.T) {
	_, ri := newTriggerRepo(t)
	win, ws := modeSeams(t, false, nil)
	l := startModeLoop(t, ri, trigAgent, "main", modeFor(t, ri, trigAgent, 0))
	waitTicks(t, l, 1)
	waitForWaits(t, ws, 1)
	for i := 0; i < 5; i++ {
		write(t, ri, "kb/tasks/t"+string(rune('a'+i))+".md")
	}
	holdsAt(t, l, 1, win, 0, "no sync key: a commit does not wake the loop")
	require.False(t, ri.realtimePush.Load())
	require.Equal(t, []time.Duration{300 * time.Second}, ws.waits(), "absent: today's 300 s")
}

// T3 SyncMode_OnlyAgentBranchWakes: with `push: realtime` published, commits
// on exp/x, on a pushed peer branch and on the consensus branch open no
// countdown and leave the wake slot empty; one agent-branch commit then opens
// exactly one (the positive control). Sabotage: wake in every onCommit arm
// (before the branch test, or in the consensus arm) → red.
func TestSyncMode_OnlyAgentBranchWakes(t *testing.T) {
	_, ri := newTriggerRepo(t)
	putOntologyOn(t, ri, "main", triggerOntology(syncRoot(fact.SyncRealtime, fact.SyncRealtime)))
	win, ws := modeSeams(t, false, nil)
	l := startModeLoop(t, ri, trigAgent, "main", modeFor(t, ri, trigAgent, 0))
	waitTicks(t, l, 1)
	waitForWaits(t, ws, 1)
	flagIs(t, ri, true, "the loop publishes push: realtime")

	ctx := context.Background()
	svc := testService(t, ri)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "exp/x", trigAgent))
	writeOn(t, ri, "exp/x", "kb/tasks/exp.md")
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/peer-0badc0de", trigAgent))
	writeOn(t, ri, "agent/peer-0badc0de", "kb/tasks/peer.md")
	writeOn(t, ri, "main", "kb/tasks/consensus.md")
	time.Sleep(pushQuiet)
	require.Len(t, ri.syncWake, 0, "no wake for exp/*, peer or consensus-branch commits")
	holdsAt(t, l, 1, win, 0, "only this instance's agent branch counts")

	write(t, ri, "kb/tasks/mine.md")
	waitOpens(t, win, 1)
}

// T4 SyncMode_MergeConverges: a real bare origin whose consensus branch
// advances, under `push: realtime`. The round that pulls the peer's commit
// writes a merge commit on the agent branch; that merge wakes the loop
// EXACTLY once (one countdown), the round it starts finds nothing, writes
// nothing and wakes nothing, and the round count stays flat over quiet
// periods; the origin ends with the local head. This is the stated cost (not
// filtered): one extra idle round per consensus advance. Sabotage: wake again
// after every round → countdowns and rounds keep climbing → red.
func TestSyncMode_MergeConverges(t *testing.T) {
	ri, bare, root := newOriginTriggerRepo(t)
	// The same ontology on the agent branch and on the origin's main, so the
	// first round's merge of main is clean.
	ont := triggerOntology(syncRoot(fact.SyncRealtime, ""))
	setOntology(t, ri, ont)
	pushToBareMain(t, bare, map[string]string{OntologyPath: ont})

	win, ws := modeSeams(t, true, nil)
	ticks := startRealModeLoop(t, ri, root, modeFor(t, ri, trigAgent, 0))
	waitForWaits(t, ws, 1)
	flagIs(t, ri, true, "the first round brought push: realtime to main")
	require.Eventually(t, func() bool { return bareRef(t, bare, "refs/heads/"+trigAgent) == head(t, ri) },
		20*time.Second, 10*time.Millisecond, "the first round pushes the agent branch")
	time.Sleep(pushQuiet)
	initial, opens := ticks.Load(), win.opened()
	require.Equal(t, int64(1), initial)

	// The consensus branch advances on the origin; a timer round pulls it.
	pushToBareMain(t, bare, map[string]string{"kb/tasks/peer.md": factBody("peer")})
	headBefore := head(t, ri)
	ws.releaseAll()
	require.Eventually(t, func() bool {
		h := head(t, ri)
		return h != headBefore && bareRef(t, bare, "refs/heads/"+trigAgent) == h
	}, 20*time.Second, 10*time.Millisecond, "the timer round merged the peer's commit and pushed it")

	var settled int64
	for i := 0; i < 3; i++ {
		time.Sleep(pushQuiet)
		now := ticks.Load()
		require.LessOrEqual(t, now, initial+2, "the timer round plus at most ONE woken round")
		if i > 0 {
			require.Equal(t, settled, now, "the round count is stable")
		}
		settled = now
	}
	require.Equal(t, initial+2, settled, "the sync-made merge commit costs exactly one extra round")
	require.Equal(t, opens+1, win.opened(), "the merge commit woke the loop exactly once")
	require.Equal(t, head(t, ri), bareRef(t, bare, "refs/heads/"+trigAgent))
}

// T11 SyncMode_OwnBranchOnly (PR 4's OwnBranchOnly under push: realtime): a
// plain learn's round pushes this instance's agent branch and nothing else —
// a peer branch present locally never reaches the origin, main is never
// pushed. Sabotage: widen the push refspec → the peer branch appears → red.
func TestSyncMode_OwnBranchOnly(t *testing.T) {
	ri, bare, root := newOriginTriggerRepo(t)
	ont := triggerOntology(syncRoot(fact.SyncRealtime, ""))
	setOntology(t, ri, ont)
	pushToBareMain(t, bare, map[string]string{OntologyPath: ont})
	_, _ = modeSeams(t, true, nil)
	ticks := startRealModeLoop(t, ri, root, modeFor(t, ri, trigAgent, 0))
	flagIs(t, ri, true, "the first round brought push: realtime to main")
	require.Eventually(t, func() bool { return bareRef(t, bare, "refs/heads/"+trigAgent) == head(t, ri) },
		20*time.Second, 10*time.Millisecond, "the first round pushes the agent branch")
	mainBefore := bareRef(t, bare, "refs/heads/main")
	base := ticks.Load()

	svc := testService(t, ri)
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "agent/peer-0badc0de", trigAgent))
	h := write(t, ri, "kb/tasks/a.md")
	require.Eventually(t, func() bool { return bareRef(t, bare, "refs/heads/"+trigAgent) == h },
		20*time.Second, 10*time.Millisecond, "the woken round pushes the new head")
	require.Greater(t, ticks.Load(), base, "a woken round ran")
	require.Equal(t, []string{"refs/heads/" + trigAgent, "refs/heads/main"}, bareHeads(t, bare),
		"only this machine's branch is pushed; never a peer's")
	require.Equal(t, mainBefore, bareRef(t, bare, "refs/heads/main"), "main is never pushed")
}

// T14 SyncMode_FlagIsCached (review gap 1): the wake reads the CACHED flag,
// never the store. The loop sits in its wait with `sync` absent; then
// `push: realtime` lands on the consensus branch and a learn follows. No
// countdown opens: the flag changes only when the loop's next iteration
// refreshes it. After a timer round the same learn does open one. Sabotage:
// read the ontology at the consensus tip inside onCommit (a live read) → the
// first learn opens a countdown → red.
func TestSyncMode_FlagIsCached(t *testing.T) {
	_, ri := newTriggerRepo(t)
	win, ws := modeSeams(t, false, nil)
	l := startModeLoop(t, ri, trigAgent, "main", modeFor(t, ri, trigAgent, 0))
	waitTicks(t, l, 1)
	waitForWaits(t, ws, 1)

	putOntologyOn(t, ri, "main", triggerOntology(syncRoot(fact.SyncRealtime, "")))
	write(t, ri, "kb/tasks/a.md")
	time.Sleep(pushQuiet)
	require.Len(t, ri.syncWake, 0, "the setting is not live in onCommit")
	holdsAt(t, l, 1, win, 0, "no countdown before the loop refreshes the flag")
	require.False(t, ri.realtimePush.Load())

	ws.releaseAll() // a timer round; the next iteration refreshes
	waitTicks(t, l, 2)
	waitForWaits(t, ws, 2)
	flagIs(t, ri, true, "the next iteration publishes the flag")
	write(t, ri, "kb/tasks/b.md")
	waitOpens(t, win, 1)
}

// T12 SyncMode_ReadAtConsensusTip: the consensus branch is named "trunk".
// `sync` on the agent branch only is not active; once trunk carries it, it
// is active from the loop's next iteration. Sabotage: read at the agent
// branch → active at once → red; read at a hardcoded "main" → never active →
// red; publish only at loop start → never active → red.
func TestSyncMode_ReadAtConsensusTip(t *testing.T) {
	_, ri := newTriggerRepo(t)
	svc := testService(t, ri)
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "trunk", trigAgent))
	ont := triggerOntology(syncRoot(fact.SyncRealtime, ""))
	setOntology(t, ri, ont) // the agent branch only

	win, ws := modeSeams(t, false, nil)
	l := startModeLoop(t, ri, trigAgent, "trunk", modeFor(t, ri, trigAgent, 0))
	require.Equal(t, "trunk", svc.UpstreamBranch(), "fixture: the consensus branch is trunk")
	waitTicks(t, l, 1)
	waitForWaits(t, ws, 1)
	write(t, ri, "kb/tasks/a.md")
	holdsAt(t, l, 1, win, 0, "sync on the agent branch alone is not the repo's setting")

	putOntologyOn(t, ri, "trunk", ont)
	ws.releaseAll()
	waitTicks(t, l, 2)
	waitForWaits(t, ws, 2)
	flagIs(t, ri, true, "active from the iteration after trunk carries it")
	write(t, ri, "kb/tasks/b.md")
	waitOpens(t, win, 1)
}

// ---- pull: realtime

// T5 SyncMode_PullRealtimeWait: `pull: realtime` with the toml interval at
// 7 s → the loop's recorded wait is 7 s, and push stays off (pull alone).
// Sabotage: ignore the setting (300 s) or hardcode 3 s → red.
func TestSyncMode_PullRealtimeWait(t *testing.T) {
	_, ri := newTriggerRepo(t)
	putOntologyOn(t, ri, "main", triggerOntology(syncRoot("", fact.SyncRealtime)))
	win, ws := modeSeams(t, false, nil)
	l := startModeLoop(t, ri, trigAgent, "main", modeFor(t, ri, trigAgent, 7*time.Second))
	waitTicks(t, l, 1)
	waitForWaits(t, ws, 1)
	require.Equal(t, []time.Duration{7 * time.Second}, ws.waits())
	require.False(t, ri.realtimePush.Load(), "pull alone does not turn push on")
	write(t, ri, "kb/tasks/a.md")
	holdsAt(t, l, 1, win, 0, "pull alone: a commit waits for the next round")
}

// T6 SyncMode_NoOverlap: under `pull: realtime` a round parked for longer
// than N starts no second round and chooses no new wait; once it returns,
// the next wait is N and exactly one round follows its end. Sabotage: run
// rounds off the loop goroutine (`go doTick` in the timer arm) → a new wait
// is chosen while the round is parked → red.
func TestSyncMode_NoOverlap(t *testing.T) {
	_, ri := newTriggerRepo(t)
	putOntologyOn(t, ri, "main", triggerOntology(syncRoot("", fact.SyncRealtime)))
	_, ws := modeSeams(t, false, nil)
	l := startModeLoop(t, ri, trigAgent, "main", modeFor(t, ri, trigAgent, 7*time.Second))
	release := l.parkTick(2)
	waitTicks(t, l, 1)
	waitForWaits(t, ws, 1)

	ws.releaseAll() // N has passed: round 2 starts and parks
	waitTicks(t, l, 2)
	time.Sleep(pushQuiet)
	require.Equal(t, int64(2), l.ticks.Load(), "no second round while one is running")
	require.Equal(t, 1, ws.count(), "no new wait is chosen while a round runs")

	release()
	waitForWaits(t, ws, 2)
	require.Equal(t, []time.Duration{7 * time.Second, 7 * time.Second}, ws.waits(), "the next wait starts after the round")
	time.Sleep(pushQuiet)
	require.Equal(t, int64(2), l.ticks.Load(), "nothing queued behind the parked round")
	ws.releaseAll()
	waitTicks(t, l, 3)
	time.Sleep(pushQuiet)
	require.Equal(t, int64(3), l.ticks.Load())
}

// T10 SyncMode_SubscriptionIgnores: a loop with no agent branch (a
// subscription) whose consensus branch sets both keys realtime keeps the
// origin's 300 s wait and never publishes push. Sabotage: honour pull when
// agentBranch == "" → the wait is 3 s → red.
func TestSyncMode_SubscriptionIgnores(t *testing.T) {
	_, ri := newTriggerRepo(t)
	putOntologyOn(t, ri, "main", triggerOntology(syncRoot(fact.SyncRealtime, fact.SyncRealtime)))
	_, ws := modeSeams(t, false, nil)
	l := startModeLoop(t, ri, "", "main", modeFor(t, ri, "", 3*time.Second))
	waitTicks(t, l, 1)
	waitForWaits(t, ws, 1)
	require.Equal(t, []time.Duration{300 * time.Second}, ws.waits(), "a subscription ignores pull: realtime")
	require.False(t, ri.realtimePush.Load(), "a subscription has nothing to push")
}

// T13 SyncMode_PullRealtimeThroughBreaker (review gap 2): under
// `pull: realtime` (N = 3 s) every round still passes S1's fetch breaker,
// whose base is N: three failing fetches open it for 6 s; a round at 5.9 s
// skips the fetch; the probe at 6 s runs it and, failing, re-opens it for
// 12 s. Sabotage: bypass the breaker under pull: realtime → the fetch at
// 5.9 s runs → red; keep the origin's interval as the base → open_until is
// 600 s → red.
func TestSyncMode_PullRealtimeThroughBreaker(t *testing.T) {
	ri := newFailingOriginRepo(t)
	putOntologyOn(t, ri, "main", triggerOntology(syncRoot("", fact.SyncRealtime)))
	l := startBreakerLoop(t, ri, "", func(l *brkLoop) { l.mode = modeFor(t, ri, trigAgent, 3*time.Second) })
	l.round(t, ri)
	l.round(t, ri)
	require.Equal(t, int64(3), l.fetches.Load())
	fetch, _ := ri.SyncBreakers()
	require.Equal(t, breakerOpen, fetch.State)
	require.Equal(t, brkT0.Add(6*time.Second), *fetch.OpenUntil, "2 × the 3 s realtime interval")

	l.clock.set(5900 * time.Millisecond)
	l.round(t, ri)
	require.Equal(t, int64(3), l.fetches.Load(), "a realtime round cannot bypass the open breaker")
	l.clock.set(6 * time.Second)
	l.round(t, ri)
	require.Equal(t, int64(4), l.fetches.Load(), "the probe runs at open_until")
	fetch, _ = ri.SyncBreakers()
	require.Equal(t, brkT0.Add(6*time.Second+12*time.Second), *fetch.OpenUntil, "a failed probe doubles the period")
	require.Equal(t, 3*time.Second, l.waits.waits()[0], "the loop's own wait is the realtime interval")
}

// ---- the host (no origin): the local loop

// noOriginModeRepo is a repo with no origin whose consensus branch (the local
// main, advanced from the agent branch) carries `sync` with both keys.
func noOriginModeRepo(t *testing.T, push, pull string) *RepoInstance {
	t.Helper()
	_, ri := newTriggerRepo(t)
	setOntology(t, ri, triggerOntology(syncRoot(push, pull)))
	svc := testService(t, ri)
	_, err := svc.AdvanceLocalUpstream(context.Background(), trigAgent, svc.UpstreamBranch())
	require.NoError(t, err)
	return ri
}

// T7 SyncMode_LocalLoop: no origin, `sync: {push: realtime, pull: realtime}`,
// N = 5 s against a 30 s local interval: the recorded wait is 5 s, and a
// learn opens one countdown whose release advances the consensus branch to
// the agent head. Sabotage: the local loop ignores the setting → the wait is
// 30 s and no countdown opens → red.
func TestSyncMode_LocalLoop(t *testing.T) {
	ri := noOriginModeRepo(t, fact.SyncRealtime, fact.SyncRealtime)
	var ticks atomic.Int64
	win, ws := modeSeams(t, false, func(_ context.Context, repo string) {
		if repo == ri.Name() {
			ticks.Add(1)
		}
	})
	svc := testService(t, ri)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go runLocalReconcileLoop(ctx, &wg, svc, ri.Name(), trigAgent, 30*time.Second, ri.triggerKick, ri.syncWake, modeFor(t, ri, trigAgent, 5*time.Second))
	t.Cleanup(func() { cancel(); wg.Wait() })

	waitForWaits(t, ws, 1)
	require.Equal(t, []time.Duration{5 * time.Second}, ws.waits(), "the host's round cadence follows pull: realtime")
	require.Equal(t, int64(1), ticks.Load(), "the start tick")
	flagIs(t, ri, true, "the local loop publishes push: realtime")

	h := write(t, ri, "kb/tasks/a.md")
	waitOpens(t, win, 1)
	win.releaseAll()
	require.Eventually(t, func() bool {
		m, err := svc.Branches().HeadCommit(context.Background(), svc.UpstreamBranch())
		return err == nil && m == h
	}, 20*time.Second, 10*time.Millisecond, "the woken local round moves the consensus branch to the agent head")
	require.Equal(t, int64(2), ticks.Load())
}

// T7b SyncMode_LocalIntervalZero (review gap 3): local_reconcile_interval = 0
// disables the local loop even with both keys realtime: it returns at once,
// runs no round, never publishes push, and a learn neither fills the wake
// slot nor opens a countdown. Sabotage: substitute N for the interval before
// the `interval <= 0` return → the loop runs → red.
func TestSyncMode_LocalIntervalZero(t *testing.T) {
	ri := noOriginModeRepo(t, fact.SyncRealtime, fact.SyncRealtime)
	var ticks atomic.Int64
	win, ws := modeSeams(t, false, func(_ context.Context, repo string) {
		if repo == ri.Name() {
			ticks.Add(1)
		}
	})
	var wg sync.WaitGroup
	wg.Add(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLocalReconcileLoop(context.Background(), &wg, testService(t, ri), ri.Name(), trigAgent, 0, ri.triggerKick, ri.syncWake, modeFor(t, ri, trigAgent, time.Second))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("local_reconcile_interval = 0 must not start the local loop")
	}
	write(t, ri, "kb/tasks/a.md")
	time.Sleep(pushQuiet)
	require.Zero(t, ticks.Load(), "no local round")
	require.Zero(t, ws.count(), "no wait chosen")
	require.Zero(t, win.opened(), "no countdown")
	require.False(t, ri.realtimePush.Load(), "the flag is never published without a loop")
	require.Len(t, ri.syncWake, 0, "a commit does not wake a loop nobody runs")
}

// ---- the builder passes the mode to the loops it starts

// T15a SyncMode_BuilderWiresLocalLoop: background sync on, no origin, a 1 h
// local interval and [git].realtime_pull_interval = 5 s. Once the consensus
// branch carries `pull: realtime`, the loop startSyncLoops started chooses a
// 5 s wait. Sabotage: startSyncLoops passes a nil mode → only 1 h waits → red.
func TestSyncMode_BuilderWiresLocalLoop(t *testing.T) {
	_, ws := modeSeams(t, true, nil)
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: home, OntologyRoot: "kb",
			Git: config.GitConfig{LocalReconcileInterval: time.Hour, RealtimePullInterval: 5 * time.Second}},
		AgentBranch: trigAgent,
		KeyPath:     filepath.Join(home, "agent.key"),
		// DisableBackgroundSync deliberately NOT set: the builder's loop is the point.
	})
	t.Cleanup(func() { _ = m.Close() })
	ri := bootRepo(t, m)
	setOntology(t, ri, triggerOntology(syncRoot("", fact.SyncRealtime)))
	waitForWaits(t, ws, 1)
	require.Eventually(t, func() bool {
		for _, d := range ws.waits() {
			if d == 5*time.Second {
				return true
			}
		}
		ws.releaseAll() // a round advances the consensus branch; the next wait follows it
		return false
	}, 20*time.Second, 20*time.Millisecond, "the builder's local loop must follow pull: realtime")
	require.False(t, ri.realtimePush.Load(), "pull alone does not publish push")
}

// T15b SyncMode_BuilderWiresOriginLoop: a clone-mode create whose origin's
// main carries `pull: realtime`; the loop ActivateSync starts chooses
// [git].realtime_pull_interval (7 s). Sabotage: ActivateSync passes a nil
// mode → the wait is 300 s → red.
func TestSyncMode_BuilderWiresOriginLoop(t *testing.T) {
	_, ws := modeSeams(t, true, nil)
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	// A PARSED COPY: fact.DefaultOntology() is a process-wide singleton, and
	// setting attributes on it would leak `sync` into every later preset repo.
	def, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	o, err := fact.ParseOntology(def)
	require.NoError(t, err)
	o.Attributes = map[string]any{fact.AttrSync: map[string]any{fact.SyncPull: fact.SyncRealtime}}
	ont, err := o.Serialize()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(OntologyPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, OntologyPath), ont, 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "seed")
	runGit(t, work, "push", "origin", "main")
	runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")

	home := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: dir,
			Git: config.GitConfig{RealtimePullInterval: 7 * time.Second}},
		AgentBranch: trigAgent,
		KeyPath:     filepath.Join(home, "agent.key"),
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	url := fileuri.New(bare)
	ri, err := m.Create(context.Background(), CreateSpec{Name: testRepoName, Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main"}}, nil)
	require.NoError(t, err)
	// Let the loops the create started settle, then restart through
	// ActivateSync alone: the wait chosen after it is that loop's.
	var settled int
	require.Eventually(t, func() bool {
		n := ws.count()
		time.Sleep(pushQuiet)
		settled = ws.count()
		return n >= 1 && n == settled
	}, 20*time.Second, 10*time.Millisecond)
	require.NoError(t, ri.ActivateSync(url))
	require.Eventually(t, func() bool { return ws.count() > settled }, 20*time.Second, 10*time.Millisecond,
		"the loop ActivateSync started chose a wait")
	require.Equal(t, 7*time.Second, ws.waits()[settled], "the origin loop ActivateSync starts must follow pull: realtime")
}
