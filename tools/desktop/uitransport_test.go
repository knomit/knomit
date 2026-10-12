//go:build desktop

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The UI picks its event transport from this flag. It must be present in BOTH
// worlds: a page that loaded while the server booted adopts the base from
// /boot/status and never re-reads config.js, so a flag set only once the server
// is up would leave that page on plain EventSource.
func TestConfigInjectingHandler_FlagsTheWailsStreamTransportBootingAndReady(t *testing.T) {
	for name, st := range map[string]func() bootStatus{
		"ready":   staticBase(desktopOrigin()),
		"booting": bootingAt(phaseStartingEngine),
	} {
		h := configInjectingHandler(testUIFS(), nil, st)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config.js", nil))
		if body := rec.Body.String(); !strings.Contains(body, `window.__KNOMIT_EVENT_TRANSPORT__ = "wails-stream";`) {
			t.Errorf("%s: config.js lacks the event transport flag; got %q", name, body)
		}
	}
}

// With the proxy in front, an /api/ request never reaches the asset handler's
// SPA fallback (which redirect-loops a nested path), whether or not the server
// is up.
func TestAPIProxyInFrontOfTheAssetHandler_APINeverReachesTheSPAFallback(t *testing.T) {
	for name, ready := range map[string]bool{"booting": false, "ready, nothing listening": true} {
		st := bootingAt(phaseStartingEngine)
		if ready {
			st = staticBase(desktopOrigin())
		}
		h := apiProxy(configInjectingHandler(testUIFS(), nil, st), func() bool { return st().Ready }, &uiUpstream{})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil))
		if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadGateway {
			t.Errorf("%s: status = %d (%q), want a 503/502 problem, not the app", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "<html>") {
			t.Errorf("%s: the SPA fallback answered an API path", name)
		}
	}
}
