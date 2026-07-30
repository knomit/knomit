//go:build desktop

package main

import (
	"context"

	"knomit/tools/desktop/internal/logtail"
)

// logEventName is the Wails event the Logs window subscribes to. Batches of
// already-formatted lines travel over Wails IPC rather than a socket: on macOS
// the UI is served through a WKURLSchemeHandler for wails://, which cannot
// carry a WebSocket upgrade, and a real listener would put the log contents on
// a TCP port every local process can read.
//
// The frontend's half of this name lives in ui/src/events.ts and is pinned to
// this constant by TestLogEventNameMatchesFrontend.
const logEventName = "desktop:log"

// startLogStream tails path on a goroutine until ctx is done, handing each
// batch of completed lines to emit.
//
// emit is injected rather than read from the App so the wiring is testable
// without a running Wails application — the same reason trayIconState takes its
// setIcon as a field.
func startLogStream(ctx context.Context, path string, emit func([]string)) {
	go startLogStreamBlocking(ctx, path, emit)
}

// startLogStreamBlocking is startLogStream without the goroutine. Split out so
// a test can observe the tailer actually RETURNING when the context is
// cancelled; from the caller's side of a `go` statement that is unobservable,
// and a tailer that outlived its context would leak one goroutine polling a
// file every 250ms for the life of the process.
func startLogStreamBlocking(ctx context.Context, path string, emit func([]string)) {
	logtail.New(path, logtail.Options{}).Run(ctx, emit)
}
