//go:build desktop

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	h := configInjectingHandler(testUIFS(), nil, staticBase("http://127.0.0.1:54321"))

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
	h := configInjectingHandler(testUIFS(), nil, func(ctx context.Context) (string, error) {
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
	h := configInjectingHandler(testUIFS(), nil, func(context.Context) (string, error) {
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
	h := configInjectingHandler(testUIFS(), nil, func(ctx context.Context) (string, error) {
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

// The desktop-only UI is served under /desktop/, and the shared knowledge app
// keeps the root. A regression here would either blank the main window or
// serve the settings form to server-mode users.
func TestConfigInjectingHandler_ServesDesktopUIUnderPrefix(t *testing.T) {
	desktopFS := fstest.MapFS{
		"settings.html": {Data: []byte("<html>settings</html>")},
		"logs.html":     {Data: []byte("<html>logs</html>")},
	}
	h := configInjectingHandler(testUIFS(), desktopFS, staticBase("http://127.0.0.1:19278"))

	for _, tc := range []struct{ path, want string }{
		{"/desktop/settings.html", "settings"},
		{"/desktop/logs.html", "logs"},
		{"/", "<html></html>"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", tc.path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s served %q, want it to contain %q", tc.path, rec.Body.String(), tc.want)
		}
	}
}

// The /desktop/ tree must not inherit the SPA fallback: a typo'd asset path
// there should 404 loudly rather than quietly resolve to the knowledge app's
// index.html, which is how a "blank window" bug hides for an afternoon.
func TestConfigInjectingHandler_DesktopMissRejects(t *testing.T) {
	h := configInjectingHandler(
		testUIFS(),
		fstest.MapFS{"settings.html": {Data: []byte("<html>settings</html>")}},
		staticBase("http://127.0.0.1:19278"),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desktop/nope.js", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("/desktop/nope.js status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
}

// Wails' runtime endpoints are answered by middleware wrapping this handler, so
// they must never reach it. If they ever do, the SPA fallback would return
// index.html with a 200 and the frontend would read that as a successful
// runtime call — which is how external links (Browser.OpenURL over
// /wails/runtime, see web/src/externalLinks.ts) would silently go inert again.
func TestConfigInjectingHandler_RefusesWailsRuntimePaths(t *testing.T) {
	h := configInjectingHandler(testUIFS(), nil, staticBase("http://127.0.0.1:19278"))

	for _, path := range []string{"/wails/runtime", "/wails/runtime.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404 (body %q)", path, rec.Code, rec.Body.String())
		}
	}
}

// application.App.impl — what every Dialog, Quit and InvokeSync call
// dereferences — is assigned INSIDE Run(). The boot watcher goroutine is
// started ~45 lines earlier, so a fast bootKnomit failure (an unwritable
// cfg.Home, or bootServer failing to listen) can reach Wails while impl is
// still a nil interface: Dialog.Error().Show() panics on a non-main goroutine
// with a bundle's stderr at /dev/null, and Quit() is a documented no-op. Either
// way the user gets a tray that looks installed and a server that is dead.
func TestAppStartGateDefersUntilTheApplicationHasStarted(t *testing.T) {
	var g appStartGate
	ran := 0

	g.after(func() { ran++ })
	if ran != 0 {
		t.Fatalf("work ran %d times before ApplicationStarted; it would hit a nil App.impl", ran)
	}

	g.open()
	if ran != 1 {
		t.Errorf("work ran %d times at ApplicationStarted, want exactly 1", ran)
	}
}

// A boot that fails LATE — after the app is up — must not be swallowed by a
// queue nobody will drain again.
func TestAppStartGateRunsImmediatelyOnceStarted(t *testing.T) {
	var g appStartGate
	g.open()

	ran := false
	g.after(func() { ran = true })
	if !ran {
		t.Error("work registered after ApplicationStarted never ran; a late boot failure would be silent")
	}
}

// Registration order is delivery order: the updater's startup and a boot-failure
// dialog can both be queued, and the dialog quits the app.
func TestAppStartGatePreservesRegistrationOrder(t *testing.T) {
	var g appStartGate
	var order []string
	g.after(func() { order = append(order, "first") })
	g.after(func() { order = append(order, "second") })
	g.open()

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("ran %v, want [first second]", order)
	}
}

// ApplicationStarted is not contractually once-only, and a second drain that
// re-ran the queue would show the boot-failure dialog twice and call Quit twice.
func TestAppStartGateOpenIsIdempotent(t *testing.T) {
	var g appStartGate
	ran := 0
	g.after(func() { ran++ })
	g.open()
	g.open()

	if ran != 1 {
		t.Errorf("work ran %d times across two opens, want exactly 1", ran)
	}
}

// The gate is reached from the boot watcher goroutine while ApplicationStarted
// fires on Wails' own. A check-then-call would let a failure arriving in the gap
// be appended to a list that has already been drained, and never run at all.
func TestAppStartGateIsSafeUnderConcurrentOpen(t *testing.T) {
	var g appStartGate
	var mu sync.Mutex
	ran := 0

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.open()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.after(func() { mu.Lock(); ran++; mu.Unlock() })
	}()
	wg.Wait()

	// Whichever side won, the work must have run exactly once — never zero
	// (queued after the drain) and never twice.
	mu.Lock()
	defer mu.Unlock()
	if ran != 1 {
		t.Errorf("work ran %d times, want exactly 1 regardless of which goroutine won", ran)
	}
}
