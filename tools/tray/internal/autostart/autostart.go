// Package autostart toggles whether knomit-tray launches at login.
package autostart

// Toggler is the common interface used by the tray UI.
type Toggler interface {
	Enabled() (bool, error)
	Enable() error
	Disable() error
}

// New returns the platform-appropriate Toggler.
func New() Toggler { return newToggler() }
