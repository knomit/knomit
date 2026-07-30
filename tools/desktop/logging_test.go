//go:build desktop

package main

import (
	"testing"

	"knomit/internal/config"
)

// The desktop's defaults differ from the server's on purpose. The server logs
// to stdout with file rotation opt-in; a bundled desktop app has no usable
// stdout (LaunchServices points stderr at /dev/null), so the file is
// mandatory and console is the format a human will actually read.
func TestDesktopLogConfigFillsDefaults(t *testing.T) {
	got := desktopLogConfig(config.LogConfig{}, "/tmp/knomit-desktop.log")

	if got.Format != "console" {
		t.Errorf("Format = %q, want console", got.Format)
	}
	// Matches logging.Build's own default. The Settings dialog displays this
	// field, so leaving it empty would show the user a blank level that Save
	// then rejects.
	if got.Level != "info" {
		t.Errorf("Level = %q, want info", got.Level)
	}
	if got.File != "/tmp/knomit-desktop.log" {
		t.Errorf("File = %q, want the default log path", got.File)
	}
	if got.MaxSizeMB == 0 || got.MaxBackups == 0 || got.MaxAgeDays == 0 {
		t.Errorf("rotation defaults left at zero: %+v", got)
	}
}

// An operator who set log.format = "json" in knomit.toml means it. Defaults
// fill gaps; they never override.
func TestDesktopLogConfigKeepsExplicitValues(t *testing.T) {
	in := config.LogConfig{
		Format: "json", Level: "debug", File: "/custom/path.log",
		MaxSizeMB: 5, MaxBackups: 9, MaxAgeDays: 30,
	}
	got := desktopLogConfig(in, "/tmp/knomit-desktop.log")

	if got != in {
		t.Errorf("desktopLogConfig mutated an explicit config:\n got %+v\nwant %+v", got, in)
	}
}
