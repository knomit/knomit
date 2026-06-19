//go:build desktop

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestConfigInjectingHandler_ServesLiveBase(t *testing.T) {
	uiFS := fstest.MapFS{
		"index.html": {Data: []byte("<html></html>")},
	}
	h := configInjectingHandler(uiFS, "http://127.0.0.1:54321")

	req := httptest.NewRequest(http.MethodGet, "/config.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `http://127.0.0.1:54321`) {
		t.Errorf("config.js missing live API base; got %q", body)
	}
	if !strings.Contains(body, "__KNOMIT_DESKTOP__") {
		t.Errorf("config.js missing desktop flag; got %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/javascript" {
		t.Errorf("Content-Type = %q, want application/javascript", ct)
	}
	// Regression: the port can change between launches (ephemeral fallback), so
	// config.js must never be cached — a stale copy points the UI at a dead port.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}
