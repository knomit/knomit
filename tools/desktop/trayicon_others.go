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

// applyTrayIcon installs the colored tray icon on Linux/Windows. (The app
// argument is unused here; macOS uses it for theme-aware icon swapping.)
func applyTrayIcon(_ *application.App, tray *application.SystemTray) {
	tray.SetIcon(trayIcon)
}
