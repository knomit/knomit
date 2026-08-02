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
	"syscall"

	"github.com/rs/zerolog/log"

	"knomit/internal/version"
)

// wantsVersion reports whether the CLI args request a version print
// (`knomit-desktop version` or `--version`/`-version`). The desktop binary is
// a GUI, so this is its CLI surface for versioning.
func wantsVersion(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "version", "--version", "-version":
		return true
	}
	return false
}

func main() {
	if wantsVersion(os.Args[1:]) {
		fmt.Println(version.String())
		return
	}

	logPath := bootstrapLogging()
	if logPath != "" {
		fmt.Fprintf(os.Stderr, "knomit-desktop: logging to %s\n", logPath)
		log.Info().Str("log_file", logPath).Str("version", version.String()).Msg("knomit-desktop starting")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Error().Err(err).Msg("knomit-desktop exited with error")
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
