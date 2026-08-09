//go:build desktop

package main

import (
	_ "embed"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// trayIconLight / trayIconDark are the menu-bar icons (the app-icon art with
// the green diamond recolored; see `make desktop-icons`). trayIconLight is the
// dark glyph shown on a LIGHT menu bar; trayIconDark is the light glyph (white
// diamond) shown on a DARK menu bar.
//
//go:embed icon-tray-light.png
var trayIconLight []byte

//go:embed icon-tray-dark.png
var trayIconDark []byte

// trayIconFor returns the menu-bar icon for the current appearance: the white
// diamond on a dark menu bar, the dark diamond on a light one.
func trayIconFor(darkMode bool) []byte {
	if darkMode {
		return trayIconDark
	}
	return trayIconLight
}

// baseTrayIcon is the un-badged menu-bar icon for the current appearance.
func baseTrayIcon(app *application.App) []byte {
	return trayIconFor(app.Env.IsDarkMode())
}

// watchTrayAppearance makes the macOS menu-bar icon follow the system theme by
// calling apply whenever the appearance may have changed. Wails'
// SetDarkModeIcon is a no-op on macOS and a template image can only be one
// tone, so we pick the icon ourselves from app.Env.IsDarkMode() — the same
// thing Wails does internally on Windows.
//
// The initial apply fires on ApplicationStarted (== applicationDidFinishLaunching
// on macOS), NOT synchronously here. The appearance is read from the
// AppleInterfaceStyle user default, which is unreliable during boot — it reports
// light even in dark mode — so a synchronous set would stick on the wrong icon
// until the next theme change.
func watchTrayAppearance(app *application.App, apply func()) {
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) { apply() })
	app.Event.OnApplicationEvent(events.Common.ThemeChanged, func(*application.ApplicationEvent) { apply() })
}
