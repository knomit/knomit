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

// resolveRemoteAuth builds a transport.AuthMethod from the remote DB record and
// the static fallback config. Returns nil on error (anonymous access).
func resolveRemoteAuth(remote *store.Remote, fallbackAuth config.RemoteAuthConfig, keyPath string) transport.AuthMethod {
	authCfg := remoteAuthFromRecord(remote, fallbackAuth)
	auth, err := ResolveAuthWithOrigin(authCfg, keyPath, remote.URL)
	if err != nil {
		log.Warn().Err(err).Str("remote", remote.URL).Msg("sync: auth resolution failed, using anonymous")
		return nil
	}
	return auth
}

// runSyncLoop pulls from the configured remote on a fixed interval.
// First sync fires immediately, then every remote.Interval seconds.
// The interval and auth are re-read from the database on each tick so that
// changes made via PUT /api/v1/{repo}/origin take effect without a restart.
func runSyncLoop(ctx context.Context, wg *sync.WaitGroup, svc *store.Service, hub *TaskHub, repo, agentBranch, keyPath string, fallbackAuth config.RemoteAuthConfig) {
	defer wg.Done()

	//todo: it's possible the remote will change while the job is still running
	remote, _ := svc.Remote().GetRemote("origin")
	if remote == nil {
		return
	}
	interval := time.Duration(remote.Interval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	auth := resolveRemoteAuth(remote, fallbackAuth, keyPath)

	lg := log.With().Str("repo", repo).Str("remote", remote.URL).Logger()
	lg.Info().Dur("interval", interval).Msg("sync loop started")

	doSync := func() {
		result, err := svc.Remote().Sync(context.Background(), agentBranch, auth)
		if err != nil {
			hub.broadcastSyncError("origin", err.Error())
			lg.Warn().Err(err).Msg("sync: pull failed")
			return
		}
		if result.Synced {
			hub.broadcastSyncOK("origin", result.MergeCommit, result.FastForward)
			lg.Info().
				Bool("fast_forward", result.FastForward).
				Str("merge_commit", result.MergeCommit).
				Msg("sync: pulled changes")
		} else {
			lg.Debug().Msg("sync: up to date")
		}
	}

	// Immediate first sync.
	doSync()

	for {
		select {
		case <-ctx.Done():
			lg.Info().Msg("sync loop stopped")
			return
		case <-ticker.C:
			// Re-read remote config so interval and auth changes take effect.
			if fresh, err := svc.Remote().GetRemote("origin"); err == nil && fresh != nil {
				if d := time.Duration(fresh.Interval) * time.Second; d != interval {
					lg.Info().Dur("old", interval).Dur("new", d).Msg("sync: interval changed")
					interval = d
					ticker.Reset(interval)
				}
				auth = resolveRemoteAuth(fresh, fallbackAuth, keyPath)
			}
			doSync()
		}
	}
}

// runPushLoop pushes the agent branch to origin on a fixed interval.
// The interval and auth are re-read from the database on each tick so that
// changes made via PUT /api/v1/{repo}/origin take effect without a restart.
func runPushLoop(ctx context.Context, wg *sync.WaitGroup, svc *store.Service, hub *TaskHub, repo, agentBranch, keyPath string, fallbackAuth config.RemoteAuthConfig) {
	defer wg.Done()

	//todo: it's possible the remote will change while the job is still running
	remote, _ := svc.Remote().GetRemote("origin")
	if remote == nil {
		return
	}
	interval := time.Duration(remote.PushInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	auth := resolveRemoteAuth(remote, fallbackAuth, keyPath)

	lg := log.With().Str("repo", repo).Str("remote", remote.URL).Logger()
	lg.Info().Dur("interval", interval).Msg("push loop started")

	doPush := func() {
		result, err := svc.Remote().Push(context.Background(), agentBranch, auth)
		if err != nil {
			hub.broadcastPushError("origin", err.Error())
			lg.Warn().Err(err).Msg("push: failed")
			return
		}
		if result.Pushed {
			hub.broadcastPushOK("origin")
			lg.Info().Str("branch", agentBranch).Msg("push: pushed changes")
		} else {
			lg.Debug().Msg("push: up to date")
		}
	}

	// Immediate first push.
	doPush()

	for {
		select {
		case <-ctx.Done():
			lg.Info().Msg("push loop stopped")
			return
		case <-ticker.C:
			// Re-read remote config so interval and auth changes take effect.
			if fresh, err := svc.Remote().GetRemote("origin"); err == nil && fresh != nil {
				if d := time.Duration(fresh.PushInterval) * time.Second; d != interval {
					lg.Info().Dur("old", interval).Dur("new", d).Msg("push: interval changed")
					interval = d
					ticker.Reset(interval)
				}
				auth = resolveRemoteAuth(fresh, fallbackAuth, keyPath)
			}
			doPush()
		}
	}
}
