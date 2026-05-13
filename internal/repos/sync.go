package repos

import (
	"context"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
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

// runReconcileLoop is the single background goroutine for origin sync.
// On each tick it: (1) calls Sync (fetch + reconcileMain + reconcileAgent),
// (2) calls Push (force-push agent if local advanced).
//
// Interval is min(sync, push) interval from the Remote record. Configured
// changes are picked up on the next tick (re-read from DB).
func runReconcileLoop(ctx context.Context, wg *sync.WaitGroup, svc *store.Service, hub *TaskHub, repo, agentBranch string, resolveAuth remoteAuthFn) {
	defer wg.Done()

	// Initial config read for logging context.
	remote, _ := svc.Remote().GetRemote("origin")
	if remote == nil {
		return
	}
	lg := log.With().Str("repo", repo).Str("remote", remote.URL).Logger()
	lg.Info().Msg("reconcile loop started")

	doTick := func(ctx context.Context) {
		// Read fresh remote record so resolveAuth picks up DB-stored auth.
		fresh, _ := svc.Remote().GetRemote("origin")
		if fresh == nil {
			return
		}
		auth := resolveAuth(fresh)

		// Sync first.
		syncResult, err := svc.Remote().Sync(ctx, agentBranch, auth)
		if err != nil {
			hub.broadcastSyncError("origin", err.Error())
			lg.Warn().Err(err).Msg("reconcile: sync failed")
		} else {
			mainChanged := syncResult.Main.FastForward || syncResult.Main.Rewound
			agentChanged := syncResult.Agent.Replayed || syncResult.Agent.FastForward
			if mainChanged || agentChanged {
				hub.broadcastSyncOK("origin", syncResult)
				lg.Info().
					Bool("main_fast_forward", syncResult.Main.FastForward).
					Bool("main_rewound", syncResult.Main.Rewound).
					Bool("agent_replayed", syncResult.Agent.Replayed).
					Bool("agent_fast_forward", syncResult.Agent.FastForward).
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
			hub.broadcastPushError("origin", err.Error())
			lg.Warn().Err(err).Msg("reconcile: push failed")
			return
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
		if err != nil || fresh == nil {
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
