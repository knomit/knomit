//go:build desktop

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/logging"
	"knomit/tools/desktop/internal/paths"
)

// Desktop rotation defaults, used when knomit.toml does not set them. Ten
// megabytes across three backups is a fortnight of ordinary desktop chatter
// without letting a log loop fill the disk.
const (
	defaultLogMaxSizeMB  = 10
	defaultLogMaxBackups = 3
	defaultLogMaxAgeDays = 14
)

// desktopLogConfig fills lc's gaps with desktop-appropriate defaults and
// returns the result. It NEVER overrides a value that is already set: an
// operator who wrote log.format = "json" into knomit.toml gets json.
//
// The desktop's defaults are deliberately not the server's. The server logs to
// stdout and treats a file as opt-in (a container's log driver owns rotation).
// A bundled macOS app has no usable stdout at all — LaunchServices points a
// bundle's stderr at /dev/null — so the file is the only surface a user has,
// which makes it mandatory here and makes "console" the right default format.
func desktopLogConfig(lc config.LogConfig, defaultFile string) config.LogConfig {
	if lc.Format == "" {
		lc.Format = "console"
	}
	// Mirrors logging.Build's own "" → info (internal/logging/logging.go:21-23),
	// so this is a no-op for the logger. It exists because the Settings dialog
	// SHOWS this value: a knomit.toml carrying a literal level = "" would
	// otherwise put an empty string in the form's level field, which
	// validateSettings then refuses on Save — a dialog the user cannot get out
	// of without hand-editing the file.
	if lc.Level == "" {
		lc.Level = "info"
	}
	if lc.File == "" {
		lc.File = defaultFile
	}
	if lc.MaxSizeMB == 0 {
		lc.MaxSizeMB = defaultLogMaxSizeMB
	}
	if lc.MaxBackups == 0 {
		lc.MaxBackups = defaultLogMaxBackups
	}
	if lc.MaxAgeDays == 0 {
		lc.MaxAgeDays = defaultLogMaxAgeDays
	}
	return lc
}

// applyLogConfig builds the logger from lc (after defaulting) and installs it
// globally. Called twice per launch: once with a zero config to bootstrap, and
// again once knomit.toml has been read.
func applyLogConfig(lc config.LogConfig, defaultFile string) error {
	resolved := desktopLogConfig(lc, defaultFile)
	lg, lvl, err := logging.Build(resolved, os.Stderr, os.Stderr, nil)
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}
	log.Logger = lg
	zerolog.SetGlobalLevel(lvl)
	return nil
}

// bootstrapLogging installs a logger before anything else runs, so that a
// failure in config.Load itself still lands somewhere visible. It returns the
// log file path, or "" if the logs dir could not be resolved (in which case
// logging falls back to stderr alone).
//
// This is phase ONE of two. Phase two (applyLogConfig from run(), once
// knomit.toml has been read) is what honours the user's level and format. The
// split exists because config.Load can fail, and a config error logged nowhere
// is the single worst startup failure to debug.
func bootstrapLogging() string {
	dir, err := paths.LogsDir()
	if err != nil {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
		return ""
	}
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
		return ""
	}
	path := filepath.Join(dir, "knomit-desktop.log")
	if applyErr := applyLogConfig(config.LogConfig{Level: "info"}, path); applyErr != nil {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
		return ""
	}
	return path
}

// logPathFor returns the desktop's default log file path. Separate from
// bootstrapLogging so phase two can resolve the same default without repeating
// the directory handling.
func logPathFor(_ config.Config) string {
	dir, err := paths.LogsDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "knomit-desktop.log")
}
