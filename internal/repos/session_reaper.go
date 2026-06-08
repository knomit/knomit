package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/store"
)

// sessionReaperConfig holds the parsed runtime parameters for the background
// idle-session reaper. Built once in Manager.Start from config.SessionConfig.
// SweepInterval <= 0 disables the loop.
type sessionReaperConfig struct {
	ToolIdleTTL     time.Duration
	PipelineIdleTTL time.Duration
	SweepInterval   time.Duration
}

// parseSessionReaperConfig parses the raw config.SessionConfig duration strings.
// Returns an error rather than silently substituting defaults so a
// misconfiguration surfaces at boot.
func parseSessionReaperConfig(raw config.SessionConfig) (sessionReaperConfig, error) {
	tool, err := parseSessionDur("tool_idle_ttl", raw.ToolIdleTTL, 15*time.Minute)
	if err != nil {
		return sessionReaperConfig{}, err
	}
	pipeline, err := parseSessionDur("pipeline_idle_ttl", raw.PipelineIdleTTL, 60*time.Minute)
	if err != nil {
		return sessionReaperConfig{}, err
	}
	sweep, err := parseSessionDur("sweep_interval", raw.SweepInterval, 5*time.Minute)
	if err != nil {
		return sessionReaperConfig{}, err
	}
	return sessionReaperConfig{
		ToolIdleTTL:     tool,
		PipelineIdleTTL: pipeline,
		SweepInterval:   sweep,
	}, nil
}

func parseSessionDur(field, s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("session.%s: %w", field, err)
	}
	return d, nil
}

// startSessionReaper launches a background goroutine that periodically deletes
// idle tool/pipeline sessions from every open repo's ephemeral session DB. A
// session is idle when its last_used_at is older than the per-cluster TTL;
// active sessions bump last_used_at on every page/work-item access and are never
// reaped, so this is safe under concurrent paging.
//
// SweepInterval <= 0 disables the loop entirely; the returned stop is a no-op.
// The returned stop func cancels the loop and joins the goroutine so it cannot
// outlive the store. Called by Manager.Start.
func (m *Manager) startSessionReaper(cfg sessionReaperConfig) (stop func()) {
	if cfg.SweepInterval <= 0 {
		log.Info().Msg("session reaper: disabled (sweep_interval=0)")
		return func() {}
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
