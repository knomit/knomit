package repos

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
)

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
func runExperimentSweepLoop(ctx context.Context, wg *sync.WaitGroup, svc *store.Service, repo string, expiryDays int, interval time.Duration) {
	defer wg.Done()
	runExperimentSweep(ctx, repo, expiryDays, interval,
		func(cutoff time.Time) ([]string, error) {
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
	if interval <= 0 {
		lg.Warn().Dur("interval", interval).
			Msg("experiment sweep not started: experiments.sweep_interval must be positive")
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
