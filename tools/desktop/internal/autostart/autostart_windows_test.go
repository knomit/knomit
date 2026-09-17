//go:build windows

package autostart

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// scratchToggler returns a toggler pointed at a throwaway subkey under HKCU,
// never the real Run key.
//
// Using the real key would change whether the developer's own machine starts
// knomit at login, and would make the result depend on what was already there
// — a test that edits the thing it is testing on a live machine is not a test.
// The subkey is deleted when the test ends.
func scratchToggler(t *testing.T, exe string) windowsToggler {
	t.Helper()
	// One key per test, so tests cannot see each other's values. Subtest
	// names contain "/", which is a separator in a registry path, so it is
	// replaced rather than allowed to create a nested key.
	name := strings.ReplaceAll(t.Name(), "/", "_")
	key := fmt.Sprintf(`Software\knomit-test\autostart-%d-%s`, os.Getpid(), name)

	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, key)
		// And the container, once the last test has emptied it. DeleteKey
		// refuses a key with subkeys, so this is a no-op until then and needs
		// no coordination between tests — it just means the registry is left
		// as the run found it rather than with an empty knomit-test key.
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\knomit-test`)
	})

	return windowsToggler{
		key:     key,
		value:   "knomit",
		exePath: func() (string, error) { return exe, nil },
	}
}

// A key that has never been created is "not enabled", not an error. A fresh
// profile can genuinely lack the Run key, and surfacing that as a failure
// would put an error in front of a user whose answer is simply "no".
func TestEnabled_MissingKeyIsFalseNotAnError(t *testing.T) {
	w := scratchToggler(t, `C:\knomit\knomit-desktop.exe`)

	on, err := w.Enabled()
	if err != nil {
		t.Fatalf("Enabled on a missing key = %v, want no error", err)
	}
	if on {
		t.Error("Enabled = true for a key that does not exist")
	}
}

// The same for a key that exists without our value — the user has other
// startup entries but not ours.
func TestEnabled_MissingValueIsFalseNotAnError(t *testing.T) {
	w := scratchToggler(t, `C:\knomit\knomit-desktop.exe`)
	k, _, err := registry.CreateKey(registry.CURRENT_USER, w.key, registry.SET_VALUE)
	if err != nil {
		t.Fatalf("create scratch key: %v", err)
	}
	if err := k.SetStringValue("something-else", "x"); err != nil {
		t.Fatalf("seed unrelated value: %v", err)
	}
	k.Close()

	on, err := w.Enabled()
	if err != nil {
		t.Fatalf("Enabled with no value of ours = %v, want no error", err)
	}
	if on {
		t.Error("Enabled = true when our value is absent")
	}
}

// Enable must write the executable path QUOTED.
//
// Explorer parses a Run value as a command line, so an unquoted
// "C:\Program Files\knomit\knomit-desktop.exe" is read as the program
// "C:\Program" with the rest as arguments. That either silently does nothing
// or, on a machine where someone has planted C:\Program.exe, runs that — which
// is why the quoting is asserted with a space-containing path rather than a
// tidy one.
func TestEnable_WritesTheExePathQuoted(t *testing.T) {
	const exe = `C:\Program Files\knomit\knomit-desktop.exe`
	w := scratchToggler(t, exe)

	if err := w.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, w.key, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("open scratch key: %v", err)
	}
	defer k.Close()
	got, _, err := k.GetStringValue(w.value)
	if err != nil {
		t.Fatalf("read value: %v", err)
	}
	if want := `"` + exe + `"`; got != want {
		t.Errorf("Run value = %s, want %s", got, want)
	}

	on, err := w.Enabled()
	if err != nil || !on {
		t.Errorf("Enabled after Enable = %v, %v; want true, nil", on, err)
	}
}

// Enable then Disable returns to "not enabled", and Disable on an
// already-absent value is success — unchecking a box that was never checked,
// or that was cleared from Task Manager's Startup tab meanwhile, is not an
// error the user can act on.
func TestDisable_RemovesTheValueAndIsIdempotent(t *testing.T) {
	w := scratchToggler(t, `C:\knomit\knomit-desktop.exe`)

	if err := w.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := w.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	on, err := w.Enabled()
	if err != nil {
		t.Fatalf("Enabled after Disable: %v", err)
	}
	if on {
		t.Error("still enabled after Disable")
	}

	if err := w.Disable(); err != nil {
		t.Errorf("second Disable = %v, want nil (already absent is success)", err)
	}
}

// Disable must not fail when the key itself was never created.
func TestDisable_MissingKeyIsSuccess(t *testing.T) {
	w := scratchToggler(t, `C:\knomit\knomit-desktop.exe`)
	if err := w.Disable(); err != nil {
		t.Errorf("Disable on a missing key = %v, want nil", err)
	}
}

// The real toggler must be wired to the real Run key and the product name —
// the scratch tests above would happily pass against a toggler pointed at
// nothing.
func TestNewTogglerUsesTheRunKey(t *testing.T) {
	w, ok := newToggler().(windowsToggler)
	if !ok {
		t.Fatal("newToggler did not return a windowsToggler")
	}
	if w.key != `Software\Microsoft\Windows\CurrentVersion\Run` {
		t.Errorf("key = %q, want the HKCU Run key", w.key)
	}
	if w.value != "knomit" {
		t.Errorf("value = %q, want knomit (what Task Manager's Startup tab shows)", w.value)
	}
	if w.exePath == nil {
		t.Error("exePath is nil; Enable would panic")
	}
}
