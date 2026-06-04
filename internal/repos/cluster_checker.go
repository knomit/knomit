package repos

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/store"
)

// clusterCheckerConfig holds the parsed runtime parameters for the
// background cluster cache warmer. Built once inside Manager.Start
// from config.ClusterCacheConfig (raw TOML/env strings) via
// parseClusterCheckerConfig. CheckInterval == 0 disables the loop.
type clusterCheckerConfig struct {
	QuietThreshold   time.Duration
	CheckInterval    time.Duration
	MaxConcurrent    int
	Resolution       float64
	MinCommunitySize int
}

// parseClusterCheckerConfig parses the raw config.ClusterCacheConfig
// duration strings into a runtime clusterCheckerConfig. Returns an
// error rather than silently substituting defaults so misconfigurations
// surface at boot, not at first review.
func parseClusterCheckerConfig(raw config.ClusterCacheConfig) (clusterCheckerConfig, error) {
	q, err := parseClusterDur("quiet_threshold", raw.QuietThreshold, 10*time.Second)
	if err != nil {
		return clusterCheckerConfig{}, err
	}
	c, err := parseClusterDur("check_interval", raw.CheckInterval, 5*time.Second)
	if err != nil {
		return clusterCheckerConfig{}, err
	}
	maxC := raw.MaxConcurrent
	if maxC <= 0 {
		maxC = 1
	}
	return clusterCheckerConfig{
		QuietThreshold:   q,
		CheckInterval:    c,
		MaxConcurrent:    maxC,
		Resolution:       clusterResolutionOrDefault(raw.Resolution),
		MinCommunitySize: clusterMinCommunityOrDefault(raw.MinCommunitySize),
	}, nil
}

// defaultClusterResolution / defaultClusterMinCommunitySize are the canonical
// fallbacks when config leaves them unset. Kept here (the repos package) as the
// single source of truth shared by the checker and, via RepoInstance getters,
// the synthesize read path — so both warm/read the same cluster_cache key.
const (
	defaultClusterResolution       = 2.0
	defaultClusterMinCommunitySize = 2
)

// clusterResolutionOrDefault / clusterMinCommunityOrDefault apply the canonical
// fallbacks for unset (<=0) config values. Shared by parseClusterCheckerConfig
// (checker) and the RepoInstance builder (read path) so both resolve identically.
func clusterResolutionOrDefault(v float64) float64 {
	if v <= 0 {
		return defaultClusterResolution
	}
	return v
}

func clusterMinCommunityOrDefault(v int) int {
	if v <= 0 {
		return defaultClusterMinCommunitySize
	}
	return v
}

func parseClusterDur(field, s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("cluster_cache.%s: %w", field, err)
	}
	return d, nil
}

// clusterKey is one (resolution, minCommunitySize) combination the
// checker proactively keeps warm. The keys are derived from the parsed
// clusterCheckerConfig (config-driven, default resolution=2.0,
// minCommunitySize=2) and MUST match what synthesize.ScopedCluster passes to
// CachedClusterFacts — RepoInstance.ClusterResolution()/ClusterMinCommunitySize()
// read the same config so the checker warms the key the read path requests.
// Callers using a different key fall through to the lazy compute on first read.
type clusterKey struct {
	Resolution       float64
	MinCommunitySize int
}

// clusterKeys returns the (resolution, minCommunitySize) keys the checker warms.
func (c clusterCheckerConfig) clusterKeys() []clusterKey {
	return []clusterKey{{Resolution: c.Resolution, MinCommunitySize: c.MinCommunitySize}}
}

// clusterDispatcher bundles the concurrency primitives shared across
// the per-tick traversal: sem caps cross-repo Louvain runs in flight,
// and wg lets stop() join any goroutines launched via dispatchRefresh
// before returning. Both are owned by startClusterChecker for the
// lifetime of the loop.
type clusterDispatcher struct {
	sem chan struct{}
	wg  *sync.WaitGroup
}

// startClusterChecker launches a background goroutine that periodically
// scans every open repo and triggers a Louvain refresh for branches
// whose HEAD has advanced AND whose newest commit is older than
// QuietThreshold ("activity has settled"). The returned stop func
// cancels the loop, joins the ticker goroutine, and waits for any
// in-flight refresh goroutines so they cannot outlive the store.
//
// CheckInterval == 0 disables the loop entirely; the returned stop is a
// no-op. Refreshes are dispatched as goroutines bounded by MaxConcurrent
// so a server hosting many repos cannot launch unbounded simultaneous
// Louvain runs. Within a single repo, the per-Service singleflight in
// CachedClusterFacts collapses the checker's trigger and any concurrent
// review-path call to one compute.
//
// Called by Manager.Start; not part of the public API.
func (m *Manager) startClusterChecker(cfg clusterCheckerConfig) (stop func()) {
	if cfg.CheckInterval <= 0 {
		log.Info().Msg("cluster cache: background checker disabled (check_interval=0)")
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	disp := &clusterDispatcher{
		sem: make(chan struct{}, cfg.MaxConcurrent),
		wg:  &sync.WaitGroup{},
	}

	go func() {
		defer close(done)
		log.Info().
			Dur("interval", cfg.CheckInterval).
			Dur("quiet_threshold", cfg.QuietThreshold).
			Int("max_concurrent", cfg.MaxConcurrent).
			Msg("cluster cache: background checker started")

		t := time.NewTicker(cfg.CheckInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Info().Msg("cluster cache: background checker stopped")
				return
			case <-t.C:
				m.tickClusterChecker(ctx, cfg, disp)
			}
		}
	}()
	return func() {
		cancel()
		<-done
		// Block until every goroutine launched via dispatchRefresh has
		// returned. Without this, refreshes that grabbed a sem slot
		// just before cancel can keep talking to the (about-to-close)
		// SQL store and log "refresh failed" warnings during shutdown.
		disp.wg.Wait()
	}
}

// tickClusterChecker runs one pass over every repo. Quiet on the happy
// path; logs at info when it fires a refresh.
func (m *Manager) tickClusterChecker(ctx context.Context, cfg clusterCheckerConfig, disp *clusterDispatcher) {
	m.ForEach(func(name string, ri *RepoInstance) {
		checkRepoClusters(ctx, ri, cfg, disp)
	})
}

// checkRepoClusters iterates a repo's branches and dispatches refreshes
// for those that are both stale AND settled.
func checkRepoClusters(ctx context.Context, ri *RepoInstance, cfg clusterCheckerConfig, disp *clusterDispatcher) {
	var (
		branches []store.Branch
		err      error
	)
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
		checkBranchClusters(ctx, ri, b.Name, now, cfg, disp)
	}
}

// checkBranchClusters evaluates a single branch against every default
// key and fires async refreshes (gated by disp) when stale + settled.
// Returns without dispatching anything if the branch has no commits,
// HEAD already matches a fresh cache row, or activity hasn't settled.
func checkBranchClusters(ctx context.Context, ri *RepoInstance, branch string, now time.Time, cfg clusterCheckerConfig, disp *clusterDispatcher) {
	var (
		head      string
		committed time.Time
		headErr   error
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

	if now.Sub(committed) < cfg.QuietThreshold {
		// Activity is too recent; wait for it to settle.
		return
	}

	for _, k := range cfg.clusterKeys() {
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
			Bool("cold", !found).
			Msg("cluster cache: triggering refresh")
		dispatchRefresh(ri, branch, k.Resolution, k.MinCommunitySize, disp)
	}
}

// runRefresh performs the actual cluster-cache recompute for one
// (repo, branch, key). Extracted from dispatchRefresh as a package var
// so tests can replace it with a controllable stub to verify the
// WaitGroup tracking behavior of dispatchRefresh.
var runRefresh = func(ctx context.Context, ri *RepoInstance, branch string, resolution float64, minCommunitySize int) error {
	var err error
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errors.New("store unavailable")
			return
		}
		_, err = svc.Search().CachedClusterFacts(ctx, branch, resolution, minCommunitySize)
	})
	return err
}

// dispatchRefresh launches a goroutine that calls CachedClusterFacts
// with a detached context. The per-Service singleflight in store
// collapses concurrent triggers for the same key, so even if the
// checker re-fires on the next tick before the previous compute
// finishes, only one Louvain run is in flight per (branch, key) at any
// moment. disp.sem caps the cross-repo total; disp.wg lets stop() join
// every dispatched goroutine before returning, so refreshes cannot
// outlive the store they read from.
func dispatchRefresh(ri *RepoInstance, branch string, resolution float64, minCommunitySize int, disp *clusterDispatcher) {
	// Track BEFORE launching the goroutine — otherwise stop() can race
	// past wg.Wait() while the goroutine hasn't yet incremented.
	disp.wg.Add(1)
	go func() {
		defer disp.wg.Done()
		select {
		case disp.sem <- struct{}{}:
		default:
			// Pool full — drop this trigger. Singleflight in the Service
			// will collapse with whatever's running; the next tick will
			// re-evaluate.
			log.Debug().Str("repo", ri.Name()).Str("branch", branch).Msg("cluster cache: refresh skipped (pool full)")
			return
		}
		defer func() { <-disp.sem }()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := runRefresh(ctx, ri, branch, resolution, minCommunitySize); err != nil {
			log.Warn().Err(err).Str("repo", ri.Name()).Str("branch", branch).Msg("cluster cache: refresh failed")
		}
	}()
}
