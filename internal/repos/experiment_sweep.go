package repos

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
)

// defaultExperimentSweepInterval is how often the sweeper looks.
//
// Deliberately NOT a setting. How stale is too stale is a policy the operator
// owns (experiments.expiry_days); how promptly we notice is an
// implementation detail, and exposing it would be a second control that can
// only be set wrong. Expiry is measured in DAYS, so a tick an hour late costs
// nothing and a quiet tick is one indexed query.
//
// It is still a PARAMETER of runExperimentSweep rather than a constant read
// inside it, because the loop's error path — a transient failure must not
// stop the loop — is untestable at an hour per tick.
const defaultExperimentSweepInterval = time.Hour

// experimentSweepIntervalOverride is a TEST-ONLY seam: when positive it
// replaces defaultExperimentSweepInterval for sweepers started afterwards, so a
// test can observe a tick that lands after something else has happened (an
// origin attach, an unmount) without waiting an hour. Production never sets it;
// the cadence stays deliberately unconfigurable (see the const above).
var experimentSweepIntervalOverride atomic.Int64

func experimentSweepInterval() time.Duration {
	if d := time.Duration(experimentSweepIntervalOverride.Load()); d > 0 {
		return d
	}
	return defaultExperimentSweepInterval
}

// experimentSweeper is the expiry sweep, in the shape of the trigger
// dispatcher and the consensus merger: built with the instance, run under the
// Serve stage's life, which the machine drains before the Open stage closes
// the store.
//
// It is origin-independent (an experiment is local-only), which is why it
// lives in Serve and not in Sync: an origin attach or detach restarts Sync and
// leaves Serve alone (issue #377 was the sweep dying on every attach when it
// shared the reconcile loop's lifetime).
//
// It holds no *store.Service: every tick reaches the store through acquire
// (ri.Acquire), so an in-flight tick is covered by the store handle's refcount
// drain and a tick that finds no store retries on the next one
// (kb/invariants/repos/store-lifetime).
type experimentSweeper struct {
	acquire    func() (*store.Service, func(), error)
	repo       string
	expiryDays int
}

func newExperimentSweeper(acquire func() (*store.Service, func(), error), repo string, expiryDays int) *experimentSweeper {
	return &experimentSweeper{acquire: acquire, repo: repo, expiryDays: expiryDays}
}

// run is the sweep's lifetime: it returns when ctx ends, including a tick in
// flight.
func (s *experimentSweeper) run(ctx context.Context) {
	runExperimentSweepLoop(ctx, s.acquire, s.repo, s.expiryDays, experimentSweepInterval())
}

// runExperimentSweepLoop drops experiments nobody has committed to in
// expiry_days, so an abandoned fork does not sit in the ref database and the
// UI forever.
//
// Polled, like the local reconcile loop it is modelled on, and for the same
// reason: there is nothing to hang it off. Expiry is measured in days, so a
// tick that is late costs nothing, and a tick that finds nothing costs one
// indexed query.
//
// It runs once at start so a host that was down past an expiry converges
// without waiting an interval — the same reason runLocalReconcileLoop ticks
// immediately.
//
// The store is reached through acquire PER TICK and released before the tick
// returns, on every path. An acquire that fails (ErrStoreUnavailable while the
// store is reopened, ErrRepoClosed once teardown has begun) is a failed tick: logged,
// retried next tick, never a reason to exit.
func runExperimentSweepLoop(ctx context.Context, acquire func() (*store.Service, func(), error), repo string, expiryDays int, interval time.Duration) {
	// The ONE place a non-positive interval is resolved. runExperimentSweep
	// below therefore never sees one and does not re-check: time.NewTicker
	// panics on a non-positive duration, so a second guard there would be
	// unreachable code standing in for a contract this line already keeps.
	if interval <= 0 {
		interval = defaultExperimentSweepInterval
	}
	runExperimentSweep(ctx, repo, expiryDays, interval,
		func(cutoff time.Time) ([]string, error) {
			svc, release, err := acquire()
			if err != nil {
				return nil, fmt.Errorf("store unavailable: %w", err)
			}
			defer release()
			return svc.Experiments().ExpireExperiments(ctx, cutoff)
		})
}

// runExperimentSweep is the loop itself, parameterised on its one store call.
// The parameterisation exists to make the ERROR path testable: the case that
// matters — a transient failure must not stop the loop — cannot be produced
// from a real store on demand.
//
// SKIP ON DOUBT, NEVER EXIT, in the shape the local reconcile loop uses
// (kb/invariants/store/local-reconcile/never-reset-never-exit). A sweep that
// returned on a DB error would silently stop expiring for the life of the
// process, and nothing would look wrong: experiments would simply accumulate,
// which is indistinguishable from a busy user. One skipped tick costs one
// interval of staleness on a policy measured in days.
//
// expiryDays == 0 means never expire, and the loop does not start. Rolling an
// experiment back is not reversible (there is no archive), so "no policy
// configured" must never resolve to "sweep with a zero-length window", which
// is what a cutoff of `now` would do to every experiment in the repo.
func runExperimentSweep(
	ctx context.Context,
	repo string,
	expiryDays int,
	interval time.Duration,
	expire func(cutoff time.Time) ([]string, error),
) {
	lg := log.With().Str("repo", repo).Logger()
	if expiryDays <= 0 {
		lg.Info().Msg("experiment sweep disabled: experiments.expiry_days is 0 (never expire)")
		return
	}
	window := time.Duration(expiryDays) * 24 * time.Hour

	tick := func() {
		// The cutoff is computed FRESH every tick rather than once at start:
		// a long-lived process would otherwise keep sweeping against the
		// window as it stood at boot.
		dropped, err := expire(time.Now().Add(-window))
		// Both can be non-empty at once — ExpireExperiments joins per
		// experiment failures and still reports what it managed to drop — so
		// this reports both rather than treating the error as the whole
		// answer.
		if len(dropped) > 0 {
			lg.Info().Strs("experiments", dropped).Int("expiry_days", expiryDays).
				Msg("experiment sweep: rolled back expired experiments")
		}
		if err != nil && ctx.Err() == nil {
			lg.Warn().Err(err).Msg("experiment sweep: failed; will retry next tick")
		}
	}

	lg.Info().Int("expiry_days", expiryDays).Dur("interval", interval).
		Msg("experiment sweep loop started")
	tick()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			lg.Info().Msg("experiment sweep loop stopped")
			return
		case <-t.C:
			tick()
		}
	}
}
