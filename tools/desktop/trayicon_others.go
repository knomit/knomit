//go:build desktop && !darwin

package main

import (
	_ "embed"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// trayIcon is the colored knomit logo shown in the tray/notification area
// (rendered from web/public/logo.svg; see `make desktop-icons`). Linux and
// Windows have no template-image convention — the full-color mark reads best
// against their tray backgrounds — so it is used as-is.
//
//go:embed icon.png
var trayIcon []byte

// baseTrayIcon is the un-badged tray icon. There is only one, and it does not
// vary with the theme. (The app argument is unused here; macOS uses it for
// theme-aware icon swapping.)
func baseTrayIcon(_ *application.App) []byte {
	return trayIcon
}

// watchTrayAppearance installs the tray icon. Unlike macOS there is no
// appearance to track, so apply runs straight away and is only called again
// when something else — the update badge — changes the icon.
func watchTrayAppearance(_ *application.App, apply func()) {
	apply()
}
