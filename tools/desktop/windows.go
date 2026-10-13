//go:build desktop

package main

import (
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// The desktop-only document. It MUST sit under desktopPrefix: that is the only
// path configInjectingHandlerWithDesktop routes to the desktop bundle, and
// anything outside it lands in the knowledge app's SPA fallback — which answers
// 200 with index.html, so the symptom is a wrong window rather than an error
// anyone can see.
//
// There used to be two. The Logs window was retired once the web UI grew a
// Manage → Logs tab that streams the same log over the API — and since the
// desktop embeds that UI, it gets the tab without a window, an IPC event or a
// file tailer of its own.
const settingsURL = desktopPrefix + "settings.html"

// auxWindows owns the desktop-only window. It is created lazily on first show:
// building it up front would pay for a webview nobody has asked for, on every
// launch, including the login-item launch.
//
// Settings is destroyed on close, because a dialog reopened later should show
// current values rather than whatever was on screen when it was dismissed.
type auxWindows struct {
	app *application.App

	mu       sync.Mutex
	settings *application.WebviewWindow
}

func newAuxWindows(app *application.App) *auxWindows {
	return &auxWindows{app: app}
}

// settingsWindowOptions describes the Settings dialog. Fixed size: a sidebar
// of four pages beside one pane, and a resizable window only offers the user
// ways to make it look broken.
func settingsWindowOptions() application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		// The name is what gates the trust-changing bindings to this window
		// (callerIsSettings): Wails reports the calling window by name. The
		// sidebar redesign kept it on purpose; renaming the window would make
		// Save, Restart, PublicKeyLine and InstallBundle refuse their own UI.
		Name:  settingsWindowName,
		Title: "Knomit Settings",
		URL:   settingsURL,
		// 172px of sidebar (General, Network, Fleet, Logs, and the server
		// status at its foot) beside a pane wide enough for the HTTP scope's
		// three-way segmented control on one line.
		Width: 680,
		// Only the page scrolls: the sidebar and the footer (restart offer,
		// outcome, Cancel and Save) are pinned in App.css, so the buttons
		// cannot leave the window whatever a page holds. This height is what
		// lets the everyday pages fit without scrolling, with the restart
		// offer standing.
		Height:        500,
		Hidden:        true,
		DisableResize: true,
	}
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
		// (windowShouldClose pushes onto the windowEvents channel, which
		// application.go drains into `go a.handleWindowEvent(event)`): the
		// mutex is what makes the write safe at all, and
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
	// hooks on their own goroutine (see above), so the hook cannot re-enter
	// this call and holding the lock across Show would not deadlock. It would
	// just serialise two clicks for no reason.
	win.Show()
	win.Focus()
}
