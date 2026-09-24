//go:build desktop

package main

import (
	"slices"
	"testing"
)

// W3 (F19 phase 3b): the desktop never opens the OAuth listener (only `knomit
// serve` does), so it must not build the issuer either: with NoOAuth the
// approval endpoints are 404 and the web UI's pending panel stays hidden,
// instead of showing a queue no request can ever reach. The Wails CORS
// origins and API-only mode are pinned alongside, because they come from the
// same constructor.
func TestDesktopAppOptions(t *testing.T) {
	o := desktopAppOptions()
	if !o.NoOAuth {
		t.Error("the desktop builds the OAuth issuer (NoOAuth false)")
	}
	if !o.APIOnly || !slices.Equal(o.CORSOrigins, wailsOrigins) {
		t.Errorf("options = %+v; want APIOnly and the Wails origins", o)
	}
}
