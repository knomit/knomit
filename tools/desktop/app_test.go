//go:build desktop

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// staticBase is an apiBase resolver for a server that is already up.
func staticBase(base string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return base, nil }
}

func testUIFS() fstest.MapFS {
	return fstest.MapFS{"index.html": {Data: []byte("<html></html>")}}
}

func TestConfigInjectingHandler_ServesLiveBase(t *testing.T) {
	h := configInjectingHandler(testUIFS(), staticBase("http://127.0.0.1:54321"))

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

// The window can be opened before the server has finished booting — the whole
// point of moving the boot off the startup path. config.js must HOLD until the
// port is known rather than serve an address that does not exist yet.
func TestConfigInjectingHandler_WaitsForABootingServer(t *testing.T) {
	release := make(chan struct{})
	h := configInjectingHandler(testUIFS(), func(ctx context.Context) (string, error) {
		select {
		case <-release:
			return "http://127.0.0.1:54321", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})

	served := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config.js", nil))
		served <- rec
	}()

	select {
	case rec := <-served:
		t.Fatalf("config.js answered before the server was up: %q", rec.Body.String())
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	select {
	case rec := <-served:
		if !strings.Contains(rec.Body.String(), "http://127.0.0.1:54321") {
			t.Errorf("config.js missing the API base once booted; got %q", rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("config.js never answered after the server came up")
	}
}

// A server that never comes up must produce a 503, not a config.js pointing at
// an empty base — the UI would otherwise issue every request against the Wails
// origin and fail in a way that looks like a broken app, not a broken server.
func TestConfigInjectingHandler_ReportsAFailedBoot(t *testing.T) {
	h := configInjectingHandler(testUIFS(), func(context.Context) (string, error) {
		return "", errors.New("embedder init failed")
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config.js", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if strings.Contains(rec.Body.String(), "__KNOMIT_API_BASE__") {
		t.Errorf("a failed boot still served config.js: %q", rec.Body.String())
	}
}

// Only config.js depends on the server. Static assets must be served while the
// boot is still in flight, or the window would be blank rather than merely
// waiting for its API address.
func TestConfigInjectingHandler_ServesAssetsWhileBooting(t *testing.T) {
	h := configInjectingHandler(testUIFS(), func(ctx context.Context) (string, error) {
		<-ctx.Done() // never comes up
		return "", ctx.Err()
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/ status = %d while booting, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<html>") {
		t.Errorf("/ served %q while booting, want index.html", rec.Body.String())
	}
}
