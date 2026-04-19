//go:build darwin

package trayui

import _ "embed"

//go:embed icon.png
var iconPNG []byte

// IconBytes returns the embedded tray-icon PNG. macOS systray treats this
// as a template image (black pixels render in the menu-bar color).
func IconBytes() []byte { return iconPNG }
