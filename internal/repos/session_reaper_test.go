package repos

import (
	"testing"
	"time"

	"knomit/internal/config"
)

// TestParseSessionReaperConfig_NeverDisabled regresses the unbounded-growth hole
// from PR #75 review finding #6: the relocated session tables have no GC other
// than the idle reaper, so the reaper must never be configurable off. An empty,
// "0", or negative value for any knob falls back to its positive default rather
// than disabling the sweep or skipping a cluster.
func TestParseSessionReaperConfig_NeverDisabled(t *testing.T) {
	cases := []struct {
		name string
		raw  config.SessionConfig
		want sessionReaperConfig
	}{
		{
			name: "empty falls back to defaults",
			raw:  config.SessionConfig{},
			want: sessionReaperConfig{defaultToolIdleTTL, defaultPipelineIdleTTL, defaultSweepInterval, DefaultPipelineResumeWindow},
		},
		{
			name: `"0" does not disable — clamps to defaults`,
			raw:  config.SessionConfig{ToolIdleTTL: "0", PipelineIdleTTL: "0", SweepInterval: "0"},
			want: sessionReaperConfig{defaultToolIdleTTL, defaultPipelineIdleTTL, defaultSweepInterval, DefaultPipelineResumeWindow},
		},
		{
			name: `"0s" does not disable — clamps to defaults`,
			raw:  config.SessionConfig{ToolIdleTTL: "0s", PipelineIdleTTL: "0s", SweepInterval: "0s"},
			want: sessionReaperConfig{defaultToolIdleTTL, defaultPipelineIdleTTL, defaultSweepInterval, DefaultPipelineResumeWindow},
		},
		{
			name: "negative clamps to defaults",
			raw:  config.SessionConfig{ToolIdleTTL: "-5m", PipelineIdleTTL: "-1h", SweepInterval: "-30s"},
			want: sessionReaperConfig{defaultToolIdleTTL, defaultPipelineIdleTTL, defaultSweepInterval, DefaultPipelineResumeWindow},
		},
		{
			name: "valid values pass through",
			raw:  config.SessionConfig{ToolIdleTTL: "3m", PipelineIdleTTL: "2h", SweepInterval: "30s"},
			want: sessionReaperConfig{3 * time.Minute, 2 * time.Hour, 30 * time.Second, DefaultPipelineResumeWindow},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSessionReaperConfig(tc.raw)
			if err != nil {
				t.Fatalf("parseSessionReaperConfig: unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parseSessionReaperConfig = %+v, want %+v", got, tc.want)
			}
			if got.SweepInterval <= 0 || got.ToolIdleTTL <= 0 || got.PipelineIdleTTL <= 0 {
				t.Fatalf("reaper must never be disabled, got %+v", got)
			}
		})
	}
}

// TestParseSessionReaperConfig_MalformedErrors confirms a malformed duration is
// surfaced as a boot error (not silently defaulted), so a typo is caught early.
func TestParseSessionReaperConfig_MalformedErrors(t *testing.T) {
	if _, err := parseSessionReaperConfig(config.SessionConfig{SweepInterval: "not-a-duration"}); err == nil {
		t.Fatal("expected an error for a malformed sweep_interval, got nil")
	}
}

// The resume window must sit strictly between zero and the pipeline idle TTL:
// a session idle past the TTL is already reaped, so a window that long could
// never displace one. An explicit value outside that range is a boot error; an
// unset one under a short TTL is clamped so an older config keeps booting.
func TestParseSessionReaperConfig_PipelineResumeWindow(t *testing.T) {
	got, err := parseSessionReaperConfig(config.SessionConfig{PipelineResumeWindow: "3m"})
	if err != nil || got.PipelineResumeWindow != 3*time.Minute {
		t.Fatalf("explicit 3m: got %v, %v", got.PipelineResumeWindow, err)
	}
	for _, bad := range []config.SessionConfig{
		{PipelineResumeWindow: "0"},
		{PipelineResumeWindow: "-1m"},
		{PipelineResumeWindow: "60m"},
		{PipelineResumeWindow: "2h"},
		{PipelineIdleTTL: "20m", PipelineResumeWindow: "20m"},
		{PipelineResumeWindow: "soon"},
	} {
		if _, err := parseSessionReaperConfig(bad); err == nil {
			t.Errorf("%+v: want a boot error", bad)
		}
	}
	got, err = parseSessionReaperConfig(config.SessionConfig{PipelineIdleTTL: "8m"})
	if err != nil {
		t.Fatalf("unset window under an 8m TTL must boot: %v", err)
	}
	if got.PipelineResumeWindow != 4*time.Minute {
		t.Fatalf("unset window under an 8m TTL = %v, want 4m", got.PipelineResumeWindow)
	}
}
