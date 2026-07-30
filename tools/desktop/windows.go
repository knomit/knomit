//go:build desktop

package main

import (
	"context"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// The two desktop-only documents. Both MUST sit under desktopPrefix: that is
// the only path configInjectingHandlerWithDesktop routes to the desktop bundle,
// and anything outside it lands in the knowledge app's SPA fallback — which
// answers 200 with index.html, so the symptom is a wrong window rather than an
// error anyone can see.
const (
	logsURL     = desktopPrefix + "logs.html"
	settingsURL = desktopPrefix + "settings.html"
)

// auxWindows owns the two desktop-only windows. Both are created lazily on
// first show: building them up front would pay for a webview nobody has asked
// for, on every launch, including the login-item launch.
//
// Logs HIDES on close (its scrollback and its live subscription are worth
// keeping); Settings is destroyed, because a dialog reopened later should show
// current values rather than whatever was on screen when it was dismissed.
type auxWindows struct {
	app *application.App
	// ctx is the application's lifetime, not any window's: it is what stops the
	// log tailer, which deliberately outlives every hide/show of the Logs
	// window. Held as a field because the tailer is started from ShowLogs,
	// which the tray menu calls with nothing to pass it.
	ctx     context.Context
	logPath string

	mu       sync.Mutex
	logs     *application.WebviewWindow
	settings *application.WebviewWindow
	logsOnce sync.Once
}

func newAuxWindows(ctx context.Context, app *application.App, logPath string) *auxWindows {
	return &auxWindows{ctx: ctx, app: app, logPath: logPath}
}

// logsWindowOptions describes the Logs window. Split out from ShowLogs so the
// parts worth asserting — the URL above all — are reachable from a test without
// a running Wails application.
func logsWindowOptions() application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Title:  "Knomit Logs",
		URL:    logsURL,
		Width:  900,
		Height: 600,
		Hidden: true,
	}
}

// settingsWindowOptions describes the Settings dialog. Fixed size: the form is
// a single column of controls, and a resizable window only offers the user ways
// to make it look broken.
func settingsWindowOptions() application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Title:         "Knomit Settings",
		URL:           settingsURL,
		Width:         520,
		Height:        420,
		Hidden:        true,
		DisableResize: true,
	}
}

// ShowLogs opens (or re-reveals) the Logs window.
func (a *auxWindows) ShowLogs() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.logs == nil {
		win := a.app.Window.NewWithOptions(logsWindowOptions())
		// Hide rather than destroy: the window holds the scrollback and the
		// live log subscription, and rebuilding both on every reopen would
		// discard exactly the history the user opened it to read.
		//
		// The hook closes over the local `win`, NOT over a.logs. Wails runs
		// window hooks on a goroutine of their own (windowShouldClose pushes
		// onto the windowEvents channel; application.go drains it into
		// `go a.handleWindowEvent(event)`), so reading the field here would be
		// an unsynchronized read racing with the write below.
		win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
			e.Cancel()
			win.Hide()
		})
		a.logs = win
	}

	// Lazily: there is no reason to poll a file nobody is watching. Once
	// started the tailer keeps running even while the window is hidden — it is
	// bound to the application context, not the window — so a reopened window
	// is already current instead of replaying. Its first batch is the file's
	// 64KB backlog, which is why there is no separate "fetch history" call for
	// the window to make.
	a.logsOnce.Do(func() {
		startLogStream(a.ctx, a.logPath, func(batch []string) {
			a.app.Event.Emit(logEventName, batch)
		})
	})

	a.logs.Show()
	a.logs.Focus()
}

// ShowSettings opens the Settings dialog, creating a fresh one each time so it
// always reflects what is currently on disk.
func (a *auxWindows) ShowSettings() {
	a.mu.Lock()
	win := a.settings
	if win == nil {
		win = a.app.Window.NewWithOptions(settingsWindowOptions())
		// Destroyed on close (no Cancel), so drop our reference too — holding a
		// pointer to a destroyed window is what would make the NEXT
		// "Settings…" click do nothing at all.
		//
		// Two details, both because Wails runs this hook on its own goroutine
		// (see ShowLogs): the mutex is what makes the write safe at all, and
		// the `a.settings == win` guard stops a teardown that arrives late —
		// after the user has already reopened Settings — from nilling out the
		// replacement window and stranding it.
		win.RegisterHook(events.Common.WindowClosing, func(_ *application.WindowEvent) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.settings == win {
				a.settings = nil
			}
		})
		a.settings = win
	}
	a.mu.Unlock()

	// Outside the lock only because nothing below needs it. Wails runs window
	// hooks on their own goroutine — windowShouldClose just pushes onto the
	// windowEvents channel, which application.go drains into
	// `go a.handleWindowEvent(event)` — so the hook above cannot re-enter this
	// call and holding the lock across Show would not deadlock. It would just
	// serialise two clicks for no reason.
	win.Show()
	win.Focus()
}
