//go:build desktop

package main

import (
	_ "embed"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// trayIconLight / trayIconDark are the menu-bar icons (the app-icon art with
// the green diamond recolored; see `make desktop-icons`). trayIconLight is the
// dark glyph shown on a LIGHT menu bar; trayIconDark is the light glyph shown
// on a DARK menu bar.
//
//go:embed icon-tray-light.png
var trayIconLight []byte

//go:embed icon-tray-dark.png
var trayIconDark []byte

// applyTrayIcon makes the macOS menu-bar icon follow the system theme. Wails'
// SetDarkModeIcon is a no-op on macOS (it just calls setIcon), and a template
// image can only be one tone — so to keep the two-tone app-icon look AND adapt
// to light/dark we swap the icon ourselves on the ThemeChanged event, the same
// thing Wails does internally on Windows.
func applyTrayIcon(app *application.App, tray *application.SystemTray) {
	set := func() {
		if app.Env.IsDarkMode() {
			tray.SetIcon(trayIconDark)
		} else {
			tray.SetIcon(trayIconLight)
		}
	}
	set()
	app.Event.OnApplicationEvent(events.Common.ThemeChanged, func(*application.ApplicationEvent) { set() })
}
