package repos

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/store"
)

// remoteAuthFn returns a transport.AuthMethod for the given remote record, or
// an error if auth RESOLUTION fails (e.g. an unreadable/missing SSH key or a
// malformed credential). It is constructed by the builder and captures the key
// path and fallback config.
//
// A nil AuthMethod with a nil error is the legitimate anonymous case
// (auth_method "none"/empty) and MUST still sync normally. A non-nil error, by
// contrast, means the configured credential could not be resolved — callers
// must treat that tick as a sync FAILURE and surface it, rather than silently
// downgrading to anonymous (which would mask a broken credential against a
// remote that happens to permit anonymous access).
type remoteAuthFn func(remote *store.Remote) (transport.AuthMethod, error)

// pushWakeWindow is the D-window ruling (F07 PR 4, user, 2026-09-28): a push
// fire opens a countdown of this length, and when it ends ONE sync round runs
// and carries everything on the branch at that moment. The countdown starts
// at the FIRST fire and later fires neither restart nor extend it: it is NOT
// the SSE batcher's debounce (commitObserver re-arms on every Notify). A code
// constant, not a config or ontology key.
const pushWakeWindow = time.Second

// syncHooks are test seams for the sync loops. Every field is nil in
// production.
type syncHooks struct {
	// window replaces time.After for the push-wake countdown, so a test holds
	// it open, counts its opens and releases it (no real sleeps).
	window func(d time.Duration) <-chan time.Time
	// tick runs at the top of each runReconcileLoop tick (after the deferred
	// dispatcher kick) and before each local-loop advance: a test counts the
	// ticks of a loop it did not start (ActivateSync's, startSyncLoops') and
	// can park one on its ctx.
	tick func(ctx context.Context, repo string)
	// now replaces time.Now for the circuit breakers (a fake clock).
	now func() time.Time
	// refusePush, when it returns an error, is the push step's result and
	// Push is not called: a refused push while the fetch works. go-git runs
	// no server hooks on a local origin, so this is the named mechanism.
	refusePush func(repo string) error
	// attempt observes each network step a round ATTEMPTS ("fetch" or
	// "push"), after the breaker allowed it; a skipped step is not reported.
	attempt func(repo, step string)
	// wait replaces time.After for each loop's between-rounds wait (both
	// loops), so a test records the wait the loop CHOSE — the origin's
	// interval, the local interval or the realtime pull interval — and
	// decides when it ends.
	wait func(d time.Duration) <-chan time.Time
}

var (
	syncHooksMu   sync.Mutex
	syncTestHooks syncHooks
)

func currentSyncHooks() syncHooks {
	syncHooksMu.Lock()
	defer syncHooksMu.Unlock()
	return syncTestHooks
}

// syncNow is the breakers' clock: time.Now, or the test's fake clock.
func syncNow() time.Time {
	if h := currentSyncHooks().now; h != nil {
		return h()
	}
	return time.Now()
}

// loopInterval is the origin loop's normal wait: min(interval, push_interval)
// from the remote record, 300 s when that is not positive. It is also the
// breakers' base: an open breaker skips its step for 2× this, doubling.
func loopInterval(r *store.Remote) time.Duration {
	interval := r.Interval
	if r.PushInterval > 0 && r.PushInterval < interval {
		interval = r.PushInterval
	}
	if interval <= 0 {
		interval = 300
	}
	return time.Duration(interval) * time.Second
}

// drainWake empties the 1-slot wake channel without blocking (nil-safe: a
// nil channel is never ready, so the default arm is taken).
func drainWake(wake <-chan struct{}) {
	select {
	case <-wake:
	default:
	}
}

// awaitPushWindow is the countdown after a push wake: it waits the window
// from THIS (first) wake, without listening to the channel meanwhile — fires
// during the countdown only fill the 1-slot channel, so they cannot restart
// it — then drains the slot, because those fires are this round's. A fire
// that lands while the round runs stays in the slot and opens the next
// countdown when the round returns: one round running, one pending, never a
// queue (the "defer, never discard" half of commitObserver, without its
// debounce). false when ctx ended during the countdown.
func awaitPushWindow(ctx context.Context, wake <-chan struct{}) bool {
	after := time.After
	if h := currentSyncHooks().window; h != nil {
		after = h
	}
	select {
	case <-ctx.Done():
		return false
	case <-after(pushWakeWindow):
	}
	drainWake(wake)
	return true
}

// makeRemoteAuthFn builds a remoteAuthFn that resolves auth from a remote
// record using the given fallback config and key path.
func makeRemoteAuthFn(fallbackAuth config.RemoteAuthConfig, keyPath string) remoteAuthFn {
	return func(remote *store.Remote) (transport.AuthMethod, error) {
		authCfg := remoteAuthFromRecord(remote, fallbackAuth)
		auth, err := resolveAuthWithOrigin(authCfg, keyPath, remote.URL)
		if err != nil {
			// Do NOT downgrade to anonymous here: a configured-but-unresolvable
			// credential must fail visibly so the reconcile tick records the
			// error and counts toward escalation, rather than silently syncing
			// anonymously against a remote that permits it.
			log.Warn().Err(err).Str("remote", remote.URL).Msg("sync: auth resolution failed")
			return nil, err
		}
		return auth, nil
	}
}

// reconcileFailureEscalateThreshold is the number of consecutive sync- or
// push-failures after which the loop escalates a log line from Warn to
// Error. A repository stuck in permanent failure (revoked credentials,
// unreachable origin, etc.) otherwise looks identical to a healthy one
// after log rotation drops the early-tick warnings.
const reconcileFailureEscalateThreshold = 5

// pushAllowed reports whether the reconcile loop may push to origin. Read-only
// (demo) instances are pull-only, and a repo with no agent branch — a
// subscription — has nothing of its own to push, by construction rather than
// by policy.
func pushAllowed(readOnly bool, agentBranch string) bool { return !readOnly && agentBranch != "" }

// remoteStatusIsError reports whether a persisted remote status column
// (last_status / last_push_status) holds a failure. The column is NULL until
// the first attempt, so a nil pointer means "never ran", not "failing".
func remoteStatusIsError(status *string) bool { return status != nil && *status == "error" }

// syncChanged reports whether a sync tick actually moved either branch. The
// zero Mode ("") means the step did not run and is treated like ModeNoop.
// tickAbandoned reports whether this tick failed because the LOOP was
// cancelled, rather than because the remote said something.
//
// Such a tick established nothing, so it is not broadcast to clients and does
// not count toward failure escalation. Creating a repo against a remote ends
// with ActivateSync, which cancels this loop to restart it (builder.go); the
// loop starts with an immediate tick, so a fetch is routinely in flight when
// that lands. Reporting it put "sync failed — Sync: fetch: ...: context
// canceled" on the repo screen of a create that had just fully succeeded.
//
// CANCELLATION ONLY, and the error must be the cancellation itself. A tick that
// ran out of time, or a real refusal that happens to land as the loop is being
// torn down, is a genuine failure and is still reported — store.abandonedByCaller
// draws the same line for the persisted status, and the two must agree or a
// failure would be broadcast without being recorded, or the reverse.
func tickAbandoned(ctx context.Context, err error) bool {
	return err != nil &&
		errors.Is(ctx.Err(), context.Canceled) &&
		errors.Is(err, context.Canceled)
}

func syncChanged(result store.SyncResult) bool {
	moved := func(m store.Mode) bool { return m != store.ModeNoop && m != "" }
	return moved(result.Main.Mode) || moved(result.Agent.Mode)
}

// shouldBroadcastSyncOK reports whether a SUCCESSFUL sync tick must be pushed
// to SSE subscribers. Two reasons qualify:
//
//   - something actually moved (main or agent), which clients render as a
//     reconcile summary; or
//   - the previous persisted status was "error", making this tick the
//     error→ok RECOVERY EDGE.
//
// The recovery edge is NOT optional. A client's remote-error banner is raised
// by sync_error and lowered only by a clean remote event, so an outage that
// heals on a tick with nothing to pull — the normal case, since the outage
// itself is what stopped the traffic — would leave the banner standing for the
// life of the session even though the stored status already says "ok".
//
// The edge is read off the PERSISTED status rather than an in-memory failure
// counter so it survives a server restart, where the counter starts at zero
// but the recorded status is still "error".
func shouldBroadcastSyncOK(result store.SyncResult, wasFailing bool) bool {
	return syncChanged(result) || wasFailing
}

// shouldBroadcastPushOK is the push-side twin of shouldBroadcastSyncOK: a push
// that sent nothing is normally silent, but it must still be announced when it
// is the recovery edge out of a recorded push failure. A credential that starts
// working again typically has nothing left to send, so `pushed` alone would
// never clear the banner.
func shouldBroadcastPushOK(pushed, wasFailing bool) bool { return pushed || wasFailing }

// runReconcileLoop is the single background goroutine for origin sync.
// On each tick it: (1) calls Sync (fetch + reconcileMain + reconcileAgent),
// (2) calls Push (force-push agent if local advanced).
//
// Interval is min(sync, push) interval from the Remote record. Configured
// changes are picked up on the next tick (re-read from DB).
//
// kick (nil-safe) is the trigger dispatcher's kick (F07 PR 2): every tick
// kicks it, so the `on: due` sweep runs on the existing reconcile tick with no
// timer of its own. It is deferred as the FIRST statement of the tick, before
// any of the tick's early returns — an offline machine fails its push on every
// tick, and the sweep needs no network. A non-blocking send on a channel that
// is never closed, so a kick during shutdown is harmless.
//
// wake (nil-safe) is the OTHER direction (F07 PR 4): the 1-slot channel a
// `do: push` fire or knomit.push() sends on (ri.wakeSync). It lives on the
// RepoInstance, not here, so it survives ActivateSync's loop restart. A wake
// opens the push countdown (awaitPushWindow) and then runs one ordinary
// tick. The slot is drained once before the first tick, which covers any
// wake left over from before this loop existed.
//
// breakers (nil-safe) is where the loop publishes its fetch and push circuit
// breakers for the origin view (F21 S1, see breaker.go). Every round — the
// first, a timer round and a woken round alike — consults them inside doTick,
// so a wake cannot bypass an open breaker.
//
// mode (nil-safe: today's behaviour) follows the `sync` root attribute (F21
// S2, sync_mode.go). It is refreshed before the first round and at the top of
// every iteration: `pull: realtime` makes the wait — and the breakers' base —
// [git].realtime_pull_interval, and `push: realtime` is published for
// ri.onCommit, whose wakes arrive on the same wake arm as a `do: push`. A
// realtime round is an ordinary doTick, so it passes the same breakers.
func runReconcileLoop(ctx context.Context, wg *sync.WaitGroup, svc *store.Service, hub *TaskHub, repo, agentBranch string, resolveAuth remoteAuthFn, localOriginRoot string, readOnly bool, onPush func(repo string, err error), kick func(), wake <-chan struct{}, breakers *syncBreakers, mode *syncMode) {
	defer wg.Done()
	defer mode.clear()

	// Initial config read for logging context.
	remote, err := svc.Remote().GetRemote("origin")
	if err != nil {
		log.Error().Err(err).Str("repo", repo).Msg("reconcile loop: initial remote read failed; not starting")
		return
	}
	if remote == nil {
		return
	}
	lg := log.With().Str("repo", repo).Str("remote", remote.URL).Logger()
	lg.Info().Msg("reconcile loop started")

	var syncFails, pushFails int
	// The two circuit breakers. They live here, on the loop goroutine, and
	// start closed: a restarted loop (ActivateSync, a server restart) makes a
	// fresh attempt at once, which a changed origin or credential deserves.
	var fetchBrk, pushBrk breaker
	breakers.publish(fetchBrk, pushBrk)

	logFailure := func(count int) *zerolog.Event {
		if count >= reconcileFailureEscalateThreshold {
			return lg.Error().Int("consecutive_failures", count)
		}
		return lg.Warn().Int("consecutive_failures", count)
	}

	// recordStep applies one ATTEMPT's outcome to a breaker and logs its
	// transitions: WARN when it opens and on each failed probe, INFO when a
	// probe closes it. Failures 1–2 of a closed breaker log nothing here; the
	// step's own failure line (logFailure) covers them.
	recordStep := func(step string, b *breaker, ok bool, base time.Duration) {
		prev := *b
		now := syncNow()
		*b = b.record(ok, now, base)
		switch {
		case ok && prev.opens > 0:
			lg.Info().Int("after_failures", prev.fails).Msgf("reconcile: %s breaker closed", step)
		case !ok && prev.opens == 0 && b.opens > 0:
			lg.Warn().Int("consecutive_failures", b.fails).
				Dur("open_for", b.openUntil.Sub(now)).Time("open_until", b.openUntil).
				Msgf("reconcile: %s breaker open", step)
		case !ok && prev.opens > 0:
			lg.Warn().Int("consecutive_failures", b.fails).
				Dur("open_for", b.openUntil.Sub(now)).Time("open_until", b.openUntil).
				Msgf("reconcile: %s breaker probe failed; open again", step)
		}
	}

	doTick := func(ctx context.Context) {
		// The dispatcher's kick, whatever this tick does or fails to do below
		// (see the function comment). FIRST, so no early return skips it —
		// including a round whose steps are all skipped by open breakers.
		if kick != nil {
			defer kick()
		}
		if h := currentSyncHooks().tick; h != nil {
			h(ctx, repo)
		}
		// Read fresh remote record so resolveAuth picks up DB-stored auth.
		fresh, err := svc.Remote().GetRemote("origin")
		if err != nil {
			lg.Error().Err(err).Msg("reconcile tick: remote read failed; skipping tick")
			return
		}
		if fresh == nil {
			return
		}
		// Defense-in-depth at the fetch boundary: a local-path origin is fetched
		// only while it still satisfies the current local-origin policy. The URL
		// was gated when it was written, but the policy (or an in-root symlink)
		// can change afterwards — re-checking each tick stops the loop from
		// continuing to fetch a now-forbidden path off the server's disk.
		if verr := validateLocalOrigin(fresh.URL, localOriginRoot); verr != nil {
			// Recurs every tick while the policy forbids this origin; keep it at
			// Warn (not Error) so a persistently-misconfigured origin doesn't
			// drown real failures. The loop keeps running on purpose: if the
			// policy is loosened again, the next tick resumes syncing.
			lg.Warn().Err(verr).Msg("reconcile: origin blocked by local-origin policy; skipping tick")
			return
		}
		// Snapshot the PRE-tick persisted status: Sync/Push overwrite it before
		// they return, so the error→ok recovery edge is only visible from here.
		// A step skipped by an open breaker writes nothing, so after an open
		// period this still reads the last REAL failure, and the probe's
		// success is the recovery edge.
		wasSyncFailing := remoteStatusIsError(fresh.LastStatus)
		wasPushFailing := remoteStatusIsError(fresh.LastPushStatus)

		// The breakers. A step whose breaker is open is skipped outright: no
		// request, no timeout, no status row, no SSE event. Their base is the
		// round's normal wait: the realtime pull interval under `pull:
		// realtime`, the origin's interval otherwise.
		base := mode.wait(loopInterval(fresh))
		now := syncNow()
		defer func() { breakers.publish(fetchBrk, pushBrk) }()
		fetchDue := fetchBrk.allow(now)
		canPush := pushAllowed(readOnly, agentBranch)
		pushDue := canPush && pushBrk.allow(now)
		if !fetchDue {
			lg.Debug().Time("open_until", fetchBrk.openUntil).Msg("reconcile: fetch breaker open; fetch skipped")
		}
		if canPush && !pushDue {
			lg.Debug().Time("open_until", pushBrk.openUntil).Msg("reconcile: push breaker open; push skipped")
		}
		if !fetchDue && !pushDue {
			return
		}
		attempt := func(step string) {
			if h := currentSyncHooks().attempt; h != nil {
				h(repo, step)
			}
		}

		auth, authErr := resolveAuth(fresh)
		if authErr != nil {
			// Auth RESOLUTION failed (unreadable key, malformed credential).
			// Fetching anonymously here would mask a broken credential against
			// a remote that permits anonymous access. We do NOT call Sync/Push
			// with nil auth on this tick.
			//
			// It is charged to the FETCH breaker only, exactly as before the
			// breaker: the error is persisted on the remote record, broadcast,
			// and counted toward escalation and the fetch breaker. The push is
			// not attempted and is NOT a push failure.
			//
			// While the fetch breaker is open (only the push is due), an auth
			// failure is charged to nothing: the push is skipped with a Debug
			// line, with no broadcast and no push-breaker charge. Resolution is
			// local, so there is no network retry to bound; and a push_error
			// broadcast here would raise a banner no push_ok ever lowers, since
			// no push status row records it (the recovery edge reads that row).
			if fetchDue {
				syncFails++
				if serr := svc.Remote().RecordSyncError("origin", authErr.Error()); serr != nil {
					lg.Warn().Err(serr).Msg("reconcile: failed to persist auth-resolution error")
				}
				hub.broadcastSyncError("origin", authErr.Error())
				logFailure(syncFails).Err(authErr).Msg("reconcile: auth resolution failed")
				recordStep("fetch", &fetchBrk, false, base)
				return
			}
			lg.Debug().Err(authErr).Msg("reconcile: auth resolution failed while the fetch breaker is open; push skipped")
			return
		}

		// Sync first. A failed fetch does not skip the push: pushing this
		// instance's own branch without a fresh fetch is still a correct
		// force-push, and the next working fetch merges what it missed.
		if fetchDue {
			attempt("fetch")
			syncResult, err := svc.Remote().Sync(ctx, agentBranch, auth)
			if tickAbandoned(ctx, err) {
				// Our own cancellation, not the remote's verdict. Say nothing, count
				// nothing (the breaker included), and do not go on to push — that
				// would fail identically.
				lg.Debug().Err(err).Msg("reconcile: tick abandoned; loop is stopping")
				return
			}
			if err != nil {
				syncFails++
				hub.broadcastSyncError("origin", err.Error())
				logFailure(syncFails).Err(err).Msg("reconcile: sync failed")
				recordStep("fetch", &fetchBrk, false, base)
			} else {
				if syncFails > 0 {
					lg.Info().Int("after_failures", syncFails).Msg("reconcile: sync recovered")
					syncFails = 0
				}
				recordStep("fetch", &fetchBrk, true, base)
				if shouldBroadcastSyncOK(syncResult, wasSyncFailing) {
					hub.broadcastSyncOK("origin", syncResult)
				}
				if syncChanged(syncResult) {
					lg.Info().
						Str("main_mode", string(syncResult.Main.Mode)).
						Str("agent_mode", string(syncResult.Agent.Mode)).
						Int("agent_replayed_count", syncResult.Agent.NumReplayed).
						Str("agent_new_tip", syncResult.Agent.NewTip).
						Msg("reconcile: pulled changes")
				} else {
					lg.Debug().Msg("reconcile: sync up to date")
				}
			}
		}

		// Then push (skipped in read-only / pull-only mode, and for a
		// subscription, which has no agent branch to push, and while the push
		// breaker is open).
		if pushDue {
			attempt("push")
			var pushResult store.PushResult
			var err error
			if h := currentSyncHooks().refusePush; h != nil {
				err = h(repo)
			}
			if err == nil {
				pushResult, err = svc.Remote().Push(ctx, agentBranch, auth)
			}
			if tickAbandoned(ctx, err) {
				lg.Debug().Err(err).Msg("reconcile: push abandoned; loop is stopping")
				return
			}
			if onPush != nil {
				// The fleet state machine retries its pending push HERE: a
				// registration or unregistration completes on the first
				// successful push of the fleet repository (Manager.fleetPushed).
				// So while the push breaker is open, that retry waits for the
				// breaker's probe — up to breakerMaxOpen.
				onPush(repo, err)
			}
			if err != nil {
				pushFails++
				hub.broadcastPushError("origin", err.Error())
				logFailure(pushFails).Err(err).Msg("reconcile: push failed")
				recordStep("push", &pushBrk, false, base)
				return
			}
			if pushFails > 0 {
				lg.Info().Int("after_failures", pushFails).Msg("reconcile: push recovered")
				pushFails = 0
			}
			recordStep("push", &pushBrk, true, base)
			if shouldBroadcastPushOK(pushResult.Pushed, wasPushFailing) {
				hub.broadcastPushOK("origin")
			}
			if pushResult.Pushed {
				lg.Info().Str("branch", agentBranch).Msg("reconcile: pushed changes")
			} else {
				lg.Debug().Msg("reconcile: push up to date")
			}
		}
	}

	// Immediate first tick. It covers every commit made so far, so a wake
	// already in the slot is drained first rather than causing a second one.
	// The setting is read first, so the first round's breakers already use
	// the realtime base.
	mode.refresh(ctx)
	drainWake(wake)
	doTick(ctx)

	for {
		// A cancelled loop (ActivateSync is restarting it, or shutdown) stops
		// here, before the select could consume a wake it would not act on:
		// the next loop's first tick then drains it deterministically.
		if ctx.Err() != nil {
			lg.Info().Msg("reconcile loop stopped")
			return
		}
		// Re-read remote config every iteration to pick up interval changes.
		fresh, err := svc.Remote().GetRemote("origin")
		if err != nil {
			lg.Error().Err(err).Msg("reconcile loop stopped: remote read failed")
			return
		}
		if fresh == nil {
			lg.Info().Msg("reconcile loop stopped: remote disappeared")
			return
		}
		// The `sync` setting, at the consensus branch's tip as the round that
		// just ended left it. The wait is recomputed only here, after a round
		// returns, so a round longer than the realtime interval delays the
		// next one; rounds never overlap or queue.
		mode.refresh(ctx)

		select {
		case <-ctx.Done():
			lg.Info().Msg("reconcile loop stopped")
			return
		case <-loopWait(mode.wait(loopInterval(fresh))):
			doTick(ctx)
		case <-wake:
			if !awaitPushWindow(ctx, wake) {
				lg.Info().Msg("reconcile loop stopped")
				return
			}
			doTick(ctx)
		}
	}
}

// runLocalReconcileLoop is the origin-less twin of runReconcileLoop. A repo
// with no remote has nothing to pull or push, but its consensus branch must
// still follow its agent branch: main is what a peer subscribing to THIS
// instance reads, and what every host means by "the knowledge base". Before
// this loop existed, an origin-less repo's main sat on the root commit for the
// life of the repo.
//
// Polled, not commit-driven. The store has ONE commit-observer slot and the
// SSE broadcast owns it, so there is nothing to hang this off; a tick that
// finds the two tips equal costs two ref reads and does nothing. It runs once
// at start so a restarted instance converges without waiting an interval.
//
// It exits — rather than skipping — as soon as the repo has an origin, so the
// two loops are mutually exclusive by the same fact. A subscription has no
// agent branch and is excluded by the same guard.
//
// wake is the push wake (see runReconcileLoop): with no origin, a push fire
// moves main up to this machine's own branch after the countdown instead of
// within the interval (D-local, user, 2026-09-28). It never publishes other
// agents' branches: main follows only this machine's agent branch.
func runLocalReconcileLoop(ctx context.Context, wg *sync.WaitGroup, svc *store.Service, repo, agentBranch string, interval time.Duration, kick func(), wake <-chan struct{}, mode *syncMode) {
	defer wg.Done()
	runLocalReconcile(ctx, repo, agentBranch, interval, mode,
		func() (bool, error) {
			r, err := svc.Remote().GetRemote("origin")
			return r != nil, err
		},
		func() error {
			_, err := svc.AdvanceLocalUpstream(ctx, agentBranch, svc.UpstreamBranch())
			return err
		},
		kick, wake)
}

// runLocalReconcile is the loop itself, parameterised on its two store reads.
// The parameterisation exists to make the ERROR paths testable: the case that
// matters — a transient failure of the origin read must not stop the loop —
// cannot be produced from a real store on demand.
//
// hasOrigin answers "is this repo origin-backed?", and its error is a THIRD
// state, not a third way of saying yes. GetRemote returns (nil, nil) for the
// absent case, so an error there is a real failure of the status query —
// SQLITE_BUSY under concurrent writes is the plausible one in this store. An
// earlier version treated that as "has an origin" and exited: one transient
// error then stopped main advancing for the life of the process, and a
// subscribing peer silently stopped seeing facts, indistinguishable from a
// quiet repo. Skipping a tick costs one interval of staleness; exiting costs
// permanent staleness. This matches the sibling loop, which re-reads its
// config fresh every tick and never exits on a read failure
// (kb/architecture/repos/reconcile-loop-fresh-config-per-tick).
//
// kick (nil-safe) is the trigger dispatcher's kick (F07 PR 2), called on EVERY
// tick of the ticker whatever hasOrigin or advance answered — the `on: due`
// sweep needs neither — so a failing advance never silences the sweep.
//
// wake (nil-safe) is the push wake: after the countdown and the drain it runs
// exactly the timer's body — kickTriggers, then ownsMain with its exit on a
// definite origin, then advance only when this loop owns main.
//
// mode (nil-safe: today's behaviour) is the `sync` root attribute (F21 S2),
// refreshed at start and before every wait. On a host, `pull: realtime` is
// the round's cadence (the realtime pull interval instead of interval): it
// fetches nothing, it advances the consensus branch and carries the `on: due`
// sweep. `push: realtime` is published for ri.onCommit, whose wakes run the
// wake arm. interval <= 0 still disables the loop entirely, BEFORE the
// setting is read: then neither key does anything and the flag stays false.
func runLocalReconcile(
	ctx context.Context,
	repo, agentBranch string,
	interval time.Duration,
	mode *syncMode,
	hasOrigin func() (bool, error),
	advance func() error,
	kick func(),
	wake <-chan struct{},
) {
	if interval <= 0 || agentBranch == "" {
		return
	}
	defer mode.clear()
	lg := log.With().Str("repo", repo).Logger()
	kickTriggers := func() {
		if kick != nil {
			kick()
		}
	}

	// ownsMain is true only on a DEFINITE "no origin". An unreadable answer
	// decides nothing: skip the work and let the next tick ask again.
	ownsMain := func() (owns, definite bool) {
		origin, err := hasOrigin()
		if err != nil {
			lg.Warn().Err(err).Msg("local reconcile: origin read failed; skipping this tick")
			return false, false
		}
		if origin {
			return false, true
		}
		return true, true
	}

	tick := func() {
		if h := currentSyncHooks().tick; h != nil {
			h(ctx, repo)
		}
		if err := advance(); err != nil && ctx.Err() == nil {
			lg.Warn().Err(err).Msg("local reconcile: advance failed; will retry next tick")
		}
	}

	// One round of the ticker's body; false means "stop: the repo gained an
	// origin" (reconcileMain owns main now).
	round := func() bool {
		kickTriggers() // unconditional: before the origin read, before advance
		owns, definite := ownsMain()
		if definite && !owns {
			lg.Info().Msg("local reconcile loop stopped: repo gained an origin")
			return false
		}
		if owns {
			tick()
		}
		return true
	}

	// The first tick below covers every commit so far: drain a leftover wake.
	mode.refresh(ctx)
	drainWake(wake)

	// Exit at start only on a definite "this repo has an origin" — then
	// reconcileMain owns main and the two loops stay mutually exclusive by the
	// same fact. An unreadable answer enters the loop without ticking, rather
	// than either advancing main behind reconcileMain's back or never starting.
	if owns, definite := ownsMain(); definite && !owns {
		return
	} else if owns {
		lg.Info().Dur("interval", interval).Msg("local reconcile loop started")
		tick()
	}
	kickTriggers()

	// A per-round timer rather than a ticker, so the wait can follow the
	// setting: it is recomputed after each round returns.
	for {
		mode.refresh(ctx)
		select {
		case <-ctx.Done():
			lg.Info().Msg("local reconcile loop stopped")
			return
		case <-loopWait(mode.wait(interval)):
			if !round() {
				return
			}
		case <-wake:
			if !awaitPushWindow(ctx, wake) {
				lg.Info().Msg("local reconcile loop stopped")
				return
			}
			if !round() {
				return
			}
		}
	}
}
