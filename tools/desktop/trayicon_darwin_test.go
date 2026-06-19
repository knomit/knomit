//go:build desktop

package main

import (
	"bytes"
	"testing"
)

// trayIconFor must map dark mode to the light glyph (white diamond, for a dark
// menu bar) and light mode to the dark glyph — the inversion is the whole point
// and the easy thing to get backwards.
func TestTrayIconFor(t *testing.T) {
	if got := trayIconFor(true); !bytes.Equal(got, trayIconDark) {
		t.Errorf("dark mode: want trayIconDark (white diamond), got a different icon")
	}
	if got := trayIconFor(false); !bytes.Equal(got, trayIconLight) {
		t.Errorf("light mode: want trayIconLight (dark diamond), got a different icon")
	}
	if len(trayIconLight) == 0 || len(trayIconDark) == 0 {
		t.Fatal("tray icons must be embedded (non-empty)")
	}
	if bytes.Equal(trayIconLight, trayIconDark) {
		t.Error("light and dark tray icons must differ")
	}
}
