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

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var version = "0.1.0-dev"

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is defined in app.go (Task 6). Temporary stub keeps the package compiling
// until then; REMOVE in Task 6.
func run(context.Context) error { return nil }
