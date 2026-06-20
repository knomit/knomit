package repos

import (
	"context"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/store"
)

// remoteAuthFn returns a transport.AuthMethod for the given remote record.
// It is constructed by the builder and captures the key path and fallback config.
type remoteAuthFn func(remote *store.Remote) transport.AuthMethod

// makeRemoteAuthFn builds a remoteAuthFn that resolves auth from a remote
// record using the given fallback config and key path.
func makeRemoteAuthFn(fallbackAuth config.RemoteAuthConfig, keyPath string) remoteAuthFn {
	return func(remote *store.Remote) transport.AuthMethod {
		authCfg := remoteAuthFromRecord(remote, fallbackAuth)
		auth, err := resolveAuthWithOrigin(authCfg, keyPath, remote.URL)
		if err != nil {
			log.Warn().Err(err).Str("remote", remote.URL).Msg("sync: auth resolution failed, using anonymous")
			return nil
		}
		return auth
	}
}

// reconcileFailureEscalateThreshold is the number of consecutive sync- or
// push-failures after which the loop escalates a log line from Warn to
// Error. A repository stuck in permanent failure (revoked credentials,
// unreachable origin, etc.) otherwise looks identical to a healthy one
// after log rotation drops the early-tick warnings.
const reconcileFailureEscalateThreshold = 5

// runReconcileLoop is the single background goroutine for origin sync.
// On each tick it: (1) calls Sync (fetch + reconcileMain + reconcileAgent),
// (2) calls Push (force-push agent if local advanced).
//
// Interval is min(sync, push) interval from the Remote record. Configured
// changes are picked up on the next tick (re-read from DB).
func runReconcileLoop(ctx context.Context, wg *sync.WaitGroup, svc *store.Service, hub *TaskHub, repo, agentBranch string, resolveAuth remoteAuthFn, localOriginRoot string) {
	defer wg.Done()

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

	logFailure := func(count int) *zerolog.Event {
		if count >= reconcileFailureEscalateThreshold {
			return lg.Error().Int("consecutive_failures", count)
		}
		return lg.Warn().Int("consecutive_failures", count)
	}

	doTick := func(ctx context.Context) {
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
		auth := resolveAuth(fresh)

		// Sync first.
		syncResult, err := svc.Remote().Sync(ctx, agentBranch, auth)
		if err != nil {
			syncFails++
			hub.broadcastSyncError("origin", err.Error())
			logFailure(syncFails).Err(err).Msg("reconcile: sync failed")
		} else {
			if syncFails > 0 {
				lg.Info().Int("after_failures", syncFails).Msg("reconcile: sync recovered")
				syncFails = 0
			}
			mainChanged := syncResult.Main.Mode != store.ModeNoop && syncResult.Main.Mode != ""
			agentChanged := syncResult.Agent.Mode != store.ModeNoop && syncResult.Agent.Mode != ""
			if mainChanged || agentChanged {
				hub.broadcastSyncOK("origin", syncResult)
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

		// Then push.
		pushResult, err := svc.Remote().Push(ctx, agentBranch, auth)
		if err != nil {
			pushFails++
			hub.broadcastPushError("origin", err.Error())
			logFailure(pushFails).Err(err).Msg("reconcile: push failed")
			return
		}
		if pushFails > 0 {
			lg.Info().Int("after_failures", pushFails).Msg("reconcile: push recovered")
			pushFails = 0
		}
		if pushResult.Pushed {
			hub.broadcastPushOK("origin")
			lg.Info().Str("branch", agentBranch).Msg("reconcile: pushed changes")
		} else {
			lg.Debug().Msg("reconcile: push up to date")
		}
	}

	// Immediate first tick.
	doTick(ctx)

	for {
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
		interval := fresh.Interval
		if fresh.PushInterval > 0 && fresh.PushInterval < interval {
			interval = fresh.PushInterval
		}
		if interval <= 0 {
			interval = 300
		}

		select {
		case <-ctx.Done():
			lg.Info().Msg("reconcile loop stopped")
			return
		case <-time.After(time.Duration(interval) * time.Second):
			doTick(ctx)
		}
	}
}
