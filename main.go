package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/cmd"
	"knomit/internal/crashdump"
)

func main() {
	// On any unrecovered panic or fatal runtime error (including a CGO/ONNX
	// crash) dump every goroutine's stack to stderr and abort into a core dump
	// where the OS permits — the native post-mortem path, no dependency.
	debug.SetTraceback("crash")

	// Retain the tail of the log so a crash report can show what led up to the
	// failure. Tee'd into the logger before anything else runs.
	crashdump.Global = crashdump.NewRingWriter(200)
	log.Logger = log.Output(zerolog.MultiLevelWriter(zerolog.ConsoleWriter{Out: os.Stderr}, crashdump.Global))
	_ = godotenv.Load() // .env is optional

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cmd.RootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
