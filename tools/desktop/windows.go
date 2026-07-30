//go:build desktop

package main

import (
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

	mu       sync.Mutex
	logs     *application.WebviewWindow
	settings *application.WebviewWindow
}

func newAuxWindows(app *application.App) *auxWindows {
	return &auxWindows{app: app}
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
		win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
			e.Cancel()
			win.Hide()
		})
		a.logs = win
	}
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
		// "Settings…" click do nothing at all. Clearing the field only when it
		// still points at THIS window keeps a slow teardown from nilling out a
		// replacement that has already been opened.
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

	// Outside the lock: the closing hook above takes the same mutex, and on
	// macOS Wails can run it synchronously from the main thread while Show is
	// in flight.
	win.Show()
	win.Focus()
}
