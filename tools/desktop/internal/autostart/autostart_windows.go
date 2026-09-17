//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

// runKey is the per-user Run key: every value under it is launched once, by
// Explorer, when this user logs in.
//
// HKEY_CURRENT_USER, not HKEY_LOCAL_MACHINE. The local-machine key starts the
// app for EVERY user of the PC and writing to it needs administrator rights,
// which a tray app toggling its own checkbox does not have and should not ask
// for. The Startup folder is the other option and was not taken: it needs a
// .lnk, which means COM (IShellLink) or a shipped helper, for behaviour this
// key provides with three API calls.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// valueName is what appears in Task Manager's Startup tab, so it is the
// product name rather than a file name.
const valueName = "knomit"

// windowsToggler carries its key path and value name rather than reading the
// constants directly, so a test can point it at a scratch subkey under
// HKCU. A test that used the real Run key would alter whether the developer's
// own machine starts knomit at login — and would pass or fail depending on
// what was already there.
//
// exePath exists for the same reason: os.Executable is the test binary during
// a test, which says nothing about whether the value is written correctly.
type windowsToggler struct {
	key     string
	value   string
	exePath func() (string, error)
}

func newToggler() Toggler {
	return windowsToggler{key: runKey, value: valueName, exePath: os.Executable}
}

// Enabled reports whether the Run value exists.
//
// A MISSING key is not an error: the Run key is normally present, but it is
// created on demand and a freshly provisioned profile may not have one yet.
// Reporting that as a failure would put an error in front of a user whose
// answer is simply "no, not enabled".
func (w windowsToggler) Enabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, w.key, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("open %s: %w", w.key, err)
	}
	defer k.Close()

	if _, _, err := k.GetStringValue(w.value); err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s\\%s: %w", w.key, w.value, err)
	}
	return true, nil
}

// Enable writes this executable's path into the Run key.
//
// The path is QUOTED. Explorer parses the value as a command line, so an
// unquoted "C:\Program Files\knomit\knomit-desktop.exe" is read as the program
// "C:\Program" with the rest as arguments — and Windows' own path-guessing
// then makes this either silently do nothing or, on a machine where someone
// has planted C:\Program.exe, run that instead.
//
// CreateKey rather than OpenKey so a profile whose Run key does not exist yet
// gets one; it opens the existing key when there is one.
func (w windowsToggler) Enable() error {
	exe, err := w.exePath()
	if err != nil {
		return fmt.Errorf("locate this executable: %w", err)
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, w.key, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open %s for writing: %w", w.key, err)
	}
	defer k.Close()

	if err := k.SetStringValue(w.value, `"`+exe+`"`); err != nil {
		return fmt.Errorf("set %s\\%s: %w", w.key, w.value, err)
	}
	return nil
}

// Disable removes the value. Already-absent is success, so unchecking a box
// that was never checked — or was cleared in Task Manager's Startup tab
// meanwhile — is not an error.
func (w windowsToggler) Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, w.key, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open %s for writing: %w", w.key, err)
	}
	defer k.Close()

	if err := k.DeleteValue(w.value); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("delete %s\\%s: %w", w.key, w.value, err)
	}
	return nil
}
