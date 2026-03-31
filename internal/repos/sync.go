package repos

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
)

// runSyncLoop pulls from the configured remote on a fixed interval.
// First sync fires immediately, then every remote.Interval seconds.
// The interval is re-read from the database on each tick so that changes
// made via PUT /api/v1/{repo}/origin take effect without a restart.
func runSyncLoop(ctx context.Context, wg *sync.WaitGroup, svc *store.Service, hub *TaskHub, remote *store.Remote, repo, agentBranch string) {
	defer wg.Done()

	interval := time.Duration(remote.Interval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lg := log.With().Str("repo", repo).Str("remote", remote.URL).Logger()
	lg.Info().Dur("interval", interval).Msg("sync loop started")

	doSync := func() {
		result, err := svc.Sync(context.Background(), agentBranch, remote.Branch)
		if err != nil {
			errMsg := err.Error()
			_ = svc.UpdateRemoteStatus(remote.Name, "error", &errMsg)
			hub.BroadcastSyncError(remote.Name, errMsg)
			lg.Warn().Err(err).Msg("sync: pull failed")
			return
		}
		_ = svc.UpdateRemoteStatus(remote.Name, "ok", nil)
		if result.Synced {
			hub.BroadcastSyncOK(remote.Name, result.MergeCommit, result.FastForward)
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
			// Re-read remote config so interval changes via PUT /origin take effect.
			if fresh, err := svc.GetRemote(remote.Name); err == nil && fresh != nil {
				if d := time.Duration(fresh.Interval) * time.Second; d != interval {
					lg.Info().Dur("old", interval).Dur("new", d).Msg("sync: interval changed")
					interval = d
					ticker.Reset(interval)
				}
			}
			doSync()
		}
	}
}

// runPushLoop pushes the agent branch to origin on a fixed interval.
// The interval is re-read from the database on each tick so that changes
// made via PUT /api/v1/{repo}/origin take effect without a restart.
func runPushLoop(ctx context.Context, wg *sync.WaitGroup, svc *store.Service, hub *TaskHub, remote *store.Remote, repo, agentBranch string) {
	defer wg.Done()

	interval := time.Duration(remote.PushInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lg := log.With().Str("repo", repo).Str("remote", remote.URL).Logger()
	lg.Info().Dur("interval", interval).Msg("push loop started")

	doPush := func() {
		result, err := svc.Push(context.Background(), agentBranch)
		if err != nil {
			errMsg := err.Error()
			_ = svc.UpdateRemotePushStatus(remote.Name, "error", &errMsg)
			hub.BroadcastPushError(remote.Name, errMsg)
			lg.Warn().Err(err).Msg("push: failed")
			return
		}
		_ = svc.UpdateRemotePushStatus(remote.Name, "ok", nil)
		if result.Pushed {
			hub.BroadcastPushOK(remote.Name)
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
			// Re-read remote config so interval changes via PUT /origin take effect.
			if fresh, err := svc.GetRemote(remote.Name); err == nil && fresh != nil {
				if d := time.Duration(fresh.PushInterval) * time.Second; d != interval {
					lg.Info().Dur("old", interval).Dur("new", d).Msg("push: interval changed")
					interval = d
					ticker.Reset(interval)
				}
			}
			doPush()
		}
	}
}
