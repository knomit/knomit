//go:build desktop

// knomit-desktop is the cross-platform Wails system-tray app. It boots the
// knomit server in-process (API/MCP only) on a looknomitck port and serves the
// React UI in-process via Wails.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/natefinch/lumberjack.v2"

	"knomit/tools/desktop/internal/paths"
)

var version = "0.1.0-dev"

func main() {
	logPath := setupLogging()
	if logPath != "" {
		fmt.Fprintf(os.Stderr, "knomit-desktop: logging to %s\n", logPath)
		log.Info().Str("log_file", logPath).Str("version", version).Msg("knomit-desktop starting")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Error().Err(err).Msg("knomit-desktop exited with error")
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// setupLogging tees zerolog to stderr (human-readable, visible when run from a
// terminal) AND a rotating file in the logs dir. The file is the only place
// output is visible when the app is launched as a .app bundle, because macOS
// connects a LaunchServices process's stderr to /dev/null. Returns the log
// file path, or "" if a file could not be opened (logging falls back to stderr).
//
// The logs dir is <StateDir-sibling>/Logs: ~/Library/Logs/knomit on macOS,
// the XDG state dir on Linux.
func setupLogging() string {
	console := zerolog.ConsoleWriter{Out: os.Stderr}

	dir, err := paths.LogsDir()
	if err != nil {
		log.Logger = log.Output(console)
		return ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Logger = log.Output(console)
		return ""
	}
	logPath := filepath.Join(dir, "knomit-desktop.log")
	rotator := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    10, // MB
		MaxBackups: 3,
		MaxAge:     14, // days
	}
	// Console gets pretty output; the file gets raw JSON for grep/parse.
	log.Logger = log.Output(zerolog.MultiLevelWriter(console, rotator))
	return logPath
}
