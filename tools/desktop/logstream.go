//go:build desktop

package main

import (
	"context"

	"github.com/wailsapp/wails/v3/pkg/application"

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

// logEventTarget is the ONE window a batch is delivered to. Narrowed to the
// single method both so the emit callback is unit-testable without a running
// Wails application and so the type system says out loud that this is addressed
// to a window rather than to the application.
type logEventTarget interface {
	DispatchWailsEvent(event *application.CustomEvent)
}

// The real target. If a Wails upgrade changes this method's signature, the
// build breaks here rather than at the call site in ShowLogs.
var _ logEventTarget = (*application.WebviewWindow)(nil)

// newLogEmitter returns the tailer's emit callback, addressed to a single
// window.
//
// Deliberately NOT app.Event.Emit. That fans out through EventIPCTransport to
// EVERY window (transport_event_ipc.go), and each window's ExecJS appends to an
// unbounded pendingJS queue until that window reports "wails:runtime:ready" —
// a message only @wailsio/runtime's module init sends (webview_window.go). The
// main knowledge window is served from web/, which does not depend on the
// runtime at all (it hand-posts "wails:drag" to webkit.messageHandlers in
// TopBar.tsx for exactly that reason), so its queue would never flush: once the
// Logs window was opened, every batch would append another JSON-escaped copy of
// the log text to a queue that grows for the life of the process. Worse, it
// would all be executed at once if web/ ever did import the runtime.
//
// Targeting the window keeps the buffering that the backlog depends on:
// WebviewWindow.DispatchWailsEvent goes through the same ExecJS, so a batch
// emitted before the Logs window's own runtime is ready is queued for it and
// flushed on its ready message. That is precisely the first batch — the file's
// 64KB backlog — so the buffering is load-bearing, not incidental.
func newLogEmitter(target logEventTarget) func([]string) {
	return func(batch []string) {
		target.DispatchWailsEvent(&application.CustomEvent{
			Name: logEventName,
			Data: batch,
		})
	}
}

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
