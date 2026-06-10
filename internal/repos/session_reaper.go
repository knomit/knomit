package repos

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/store"
)

// sessionReaperConfig holds the parsed runtime parameters for the background
// idle-session reaper. Built once in Manager.Start from config.SessionConfig.
// Every knob is positive by construction: parseSessionReaperConfig clamps a
// non-positive value to its default, because the reaper is never disabled (see
// the defaults block below).
type sessionReaperConfig struct {
	ToolIdleTTL     time.Duration
	PipelineIdleTTL time.Duration
	SweepInterval   time.Duration
}

// Session reaper defaults. Unlike the cluster checker (where 0 disables the
// loop), the reaper is never disabled: the relocated session/work-queue tables
// have no other garbage collection, so an off reaper would let the ephemeral
// session DB grow unbounded for the whole process lifetime. A non-positive or
// unset value for any knob therefore falls back to its default.
const (
	defaultToolIdleTTL     = 15 * time.Minute
	defaultPipelineIdleTTL = 60 * time.Minute
	defaultSweepInterval   = 5 * time.Minute
)

// parseSessionReaperConfig parses the raw config.SessionConfig duration strings.
// A malformed value is an error (surfaced at boot, not at first sweep); an
// empty or non-positive value falls back to the default so the reaper always
// runs and always reaps both clusters.
func parseSessionReaperConfig(raw config.SessionConfig) (sessionReaperConfig, error) {
	tool, err := parseConfigDur("session", "tool_idle_ttl", raw.ToolIdleTTL, defaultToolIdleTTL)
	if err != nil {
		return sessionReaperConfig{}, err
	}
	pipeline, err := parseConfigDur("session", "pipeline_idle_ttl", raw.PipelineIdleTTL, defaultPipelineIdleTTL)
	if err != nil {
		return sessionReaperConfig{}, err
	}
	sweep, err := parseConfigDur("session", "sweep_interval", raw.SweepInterval, defaultSweepInterval)
	if err != nil {
		return sessionReaperConfig{}, err
	}
	return sessionReaperConfig{
		ToolIdleTTL:     orDefaultDur(tool, defaultToolIdleTTL),
		PipelineIdleTTL: orDefaultDur(pipeline, defaultPipelineIdleTTL),
		SweepInterval:   orDefaultDur(sweep, defaultSweepInterval),
	}, nil
}

// orDefaultDur returns def when d is non-positive, else d.
func orDefaultDur(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// startSessionReaper launches a background goroutine that periodically deletes
// idle tool/pipeline sessions from every open repo's ephemeral session DB. A
// session is idle when its last_used_at is older than the per-cluster TTL;
// active sessions bump last_used_at on every page/work-item access and are never
// reaped, so this is safe under concurrent paging.
//
// The returned stop func cancels the loop and joins the goroutine so it cannot
// outlive the store. Called by Manager.Start. parseSessionReaperConfig clamps
// SweepInterval to a positive default, so the loop always runs; the guard below
// is only a defensive backstop against a directly-constructed zero config (a
// zero ticker would panic).
func (m *Manager) startSessionReaper(cfg sessionReaperConfig) (stop func()) {
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = defaultSweepInterval
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		log.Info().
			Dur("sweep_interval", cfg.SweepInterval).
			Dur("tool_idle_ttl", cfg.ToolIdleTTL).
			Dur("pipeline_idle_ttl", cfg.PipelineIdleTTL).
			Msg("session reaper: started")

		t := time.NewTicker(cfg.SweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Info().Msg("session reaper: stopped")
				return
			case <-t.C:
				m.tickSessionReaper(ctx, cfg)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// tickSessionReaper runs one reap pass over every repo. Quiet on the happy path;
// logs at info only when it actually deletes idle sessions.
func (m *Manager) tickSessionReaper(ctx context.Context, cfg sessionReaperConfig) {
	m.ForEach(func(name string, ri *RepoInstance) {
		var (
			n   int
			err error
		)
		ri.WithRead(func(svc *store.Service) {
			if svc == nil {
				return
			}
			n, err = svc.ReapIdleSessions(ctx, cfg.ToolIdleTTL, cfg.PipelineIdleTTL)
		})
		if err != nil {
			log.Debug().Err(err).Str("repo", name).Msg("session reaper: reap failed")
			return
		}
		if n > 0 {
			log.Info().Str("repo", name).Int("reaped", n).Msg("session reaper: reaped idle sessions")
		}
	})
}
