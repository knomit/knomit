//go:build desktop && !windows

package main

// Not built on Windows: it imports internal/app, whose store links sqlite-vec
// through cgo, and that alone would stop `GOOS=windows go vet -tags desktop`
// of this package (which otherwise needs no cgo there). The mapping it pins is
// the same on every OS, so the darwin and linux runs are enough.

import (
	"testing"

	knomitapp "knomit/internal/app"
	"knomit/internal/foundation/config"
)

// desktopLoggingOptions is a second spelling of app.LoggingOptions (the
// desktop must not link internal/app). Pinned field for field against the
// server's converter with every field set, so a rotation key wired into one
// and not the other fails here.
func TestDesktopLoggingOptionsMatchTheServers(t *testing.T) {
	lc := config.LogConfig{Format: "json", Level: "debug", File: "/x/y.log", MaxSizeMB: 7, MaxBackups: 4, MaxAgeDays: 9}
	if got, want := desktopLoggingOptions(lc), knomitapp.LoggingOptions(lc); got != want {
		t.Errorf("desktopLoggingOptions = %+v, app.LoggingOptions = %+v", got, want)
	}
}
