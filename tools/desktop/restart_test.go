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

// A binary sitting directly under an .app (no Contents/MacOS at all, e.g. a
// misplaced dev copy) must also be rejected — walking up two directories from
// it would land on the .app's parent, which happens to satisfy none of the
// name checks but is worth pinning explicitly so a future refactor of the
// walk can't accidentally start accepting shallow paths.
func TestBundlePathForRejectsAPathDirectlyUnderTheBundle(t *testing.T) {
	if _, err := bundlePathFor("/Applications/Knomit.app/knomit-desktop"); err == nil {
		t.Error("accepted a path not nested under Contents/MacOS")
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
