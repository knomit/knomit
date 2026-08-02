//go:build desktop && darwin

package main

import "testing"

// Relaunching a macOS app means relaunching the BUNDLE, not the executable
// inside it: `open -n` on Contents/MacOS/knomit-desktop starts a process with
// no bundle identity, which loses the Dock icon, the tray, and the
// notification permissions bound to com.knomit.desktop.
func TestBundlePathForWalksUpFromTheExecutable(t *testing.T) {
	got, err := bundlePathFor("/Applications/Knomit.app/Contents/MacOS/knomit-desktop")
	if err != nil {
		t.Fatalf("bundlePathFor: %v", err)
	}
	if got != "/Applications/Knomit.app" {
		t.Errorf("got %q, want /Applications/Knomit.app", got)
	}
}

// A `go run` or `go build` binary is not in a bundle. Restart must report that
// rather than invent a path and silently fail to relaunch.
func TestBundlePathForRejectsANonBundlePath(t *testing.T) {
	if _, err := bundlePathFor("/Users/dev/knomit/desktop"); err == nil {
		t.Error("accepted a path that is not inside a .app bundle")
	}
}

// The middle segment must be exactly "Contents", not just any directory one
// level above "MacOS" — e.g. an executable nested an extra level deep at
// Contents/Resources/MacOS. Without this check on its own, only the "MacOS"
// and ".app suffix" checks would ever be exercised, leaving the Contents check
// itself unverified by any test.
func TestBundlePathForRejectsWhenContentsDirIsMissing(t *testing.T) {
	if _, err := bundlePathFor("/Applications/Knomit.app/Contents/Resources/MacOS/exe"); err == nil {
		t.Error("accepted a path whose parent-of-MacOS is not named Contents")
	}
}

// The bundle name itself must genuinely end in ".app" — a directory merely
// named "MacOS" inside a "Contents" folder that is NOT under a ".app" tree
// (e.g. a stray vendored copy of the layout) must not be treated as a bundle.
func TestBundlePathForRequiresADotAppSuffix(t *testing.T) {
	if _, err := bundlePathFor("/Users/dev/Knomit/Contents/MacOS/knomit-desktop"); err == nil {
		t.Error("accepted a bundle-shaped path whose root does not end in .app")
	}
}
