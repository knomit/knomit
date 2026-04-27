package clustercache

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// defaultKeys lists the (resolution, minCommunitySize) combinations the
// background checker proactively keeps warm. These mirror the values
// synthesize.ScopedCluster passes to ClusterFacts (resolution=1.0,
// minCommunitySize=2). If a new caller uses a different key, the read
// path will fill it lazily — the checker only warms the common case.
var defaultKeys = []clusterKey{{Resolution: 1.0, MinCommunitySize: 2}}

type clusterKey struct {
	Resolution       float64
	MinCommunitySize int
}

// StartChecker launches a background goroutine that periodically scans all
// open repos and triggers cluster recomputes for branches whose HEAD has
// advanced AND whose newest commit is older than QuietThreshold (i.e.
// "activity has settled"). Returns a stop func that joins the goroutine.
//
// CheckInterval == 0 disables the loop entirely; the returned stop is a
// no-op. Recomputes are dispatched via refreshAsync, which honors the
// global concurrency semaphore and singleflight dedup, so a single slow
// branch can never starve the pool.
func (c *Cache) StartChecker(mgr *repos.Manager) (stop func()) {
	if c.cfg.CheckInterval <= 0 {
		log.Info().Msg("cluster cache: background checker disabled (check_interval=0)")
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		log.Info().
			Dur("interval", c.cfg.CheckInterval).
			Dur("quiet_threshold", c.cfg.QuietThreshold).
			Int("max_concurrent", c.cfg.MaxConcurrent).
			Msg("cluster cache: background checker started")

		t := time.NewTicker(c.cfg.CheckInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Info().Msg("cluster cache: background checker stopped")
				return
			case <-t.C:
				c.tick(ctx, mgr)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// tick runs one pass over all repos+branches. Quiet on the happy path; logs
// at debug level when it triggers a refresh.
func (c *Cache) tick(ctx context.Context, mgr *repos.Manager) {
	mgr.ForEach(func(name string, ri *repos.RepoInstance) {
		c.checkRepo(ctx, ri)
	})
}

// checkRepo iterates a repo's branches and triggers refreshes for those
// that are both "out of date" and "settled". One ListBranches call per
// repo per tick.
func (c *Cache) checkRepo(ctx context.Context, ri *repos.RepoInstance) {
	var branches []store.Branch
	var err error
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		branches, err = svc.Branches().ListBranches(ctx)
	})
	if err != nil {
		log.Debug().Err(err).Str("repo", ri.Name()).Msg("cluster cache: list branches failed")
		return
	}

	now := time.Now()
	for _, b := range branches {
		c.checkBranch(ctx, ri, b.Name, now)
	}
}

// checkBranch evaluates a single branch against every default key. Returns
// without dispatching anything if the branch has no commits, the HEAD
// already matches a fresh cache row, or activity hasn't settled yet.
func (c *Cache) checkBranch(ctx context.Context, ri *repos.RepoInstance, branch string, now time.Time) {
	var (
		head       string
		committed  time.Time
		headErr    error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		head, committed, headErr = svc.Branches().HeadCommitInfo(ctx, branch)
	})
	if headErr != nil {
		// ErrBranchNotFound is normal for branches that exist in the
		// branches table but have no git ref yet (race during repo init).
		if !errors.Is(headErr, store.ErrBranchNotFound) {
			log.Debug().Err(headErr).Str("repo", ri.Name()).Str("branch", branch).Msg("cluster cache: head info failed")
		}
		return
	}

	if now.Sub(committed) < c.cfg.QuietThreshold {
		// Activity is too recent; wait for it to settle.
		return
	}

	for _, k := range defaultKeys {
		var (
			row   store.ClusterCacheRow
			found bool
			err   error
		)
		ri.WithRead(func(svc *store.Service) {
			if svc == nil {
				return
			}
			row, found, err = svc.ClusterCache().Get(ctx, branch, k.Resolution, k.MinCommunitySize)
		})
		if err != nil {
			log.Debug().Err(err).Str("repo", ri.Name()).Str("branch", branch).Msg("cluster cache: lookup failed")
			continue
		}
		if found && row.HeadCommit == head {
			continue
		}
		log.Info().
			Str("repo", ri.Name()).
			Str("branch", branch).
			Str("head", shortHash(head)).
			Bool("cold", !found).
			Msg("cluster cache: triggering refresh")
		c.refreshAsync(ri, branch, k.Resolution, k.MinCommunitySize)
	}
}
