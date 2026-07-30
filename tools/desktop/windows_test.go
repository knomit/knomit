//go:build desktop

package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	desktopui "knomit/tools/desktop/ui"
)

// The two windows are only ever as good as the URLs they load. A path that
// escapes /desktop/ does not fail loudly — the knowledge app's SPA fallback
// answers 200 with index.html — so pin the prefix here rather than discovering
// it as "the Logs window shows the graph".
func TestAuxWindowOptions_LoadDesktopPrefixedURLs(t *testing.T) {
	logs := logsWindowOptions()
	settings := settingsWindowOptions()

	for name, url := range map[string]string{
		"logs":     logs.URL,
		"settings": settings.URL,
	} {
		if !strings.HasPrefix(url, desktopPrefix) {
			t.Errorf("%s window URL = %q, want it under %q", name, url, desktopPrefix)
		}
	}
	if logs.URL == settings.URL {
		t.Errorf("both windows load %q; they must be distinct documents", logs.URL)
	}

	// Hidden: NewWithOptions shows a window immediately otherwise, so a
	// non-hidden aux window would flash on screen during creation before
	// Show() is even reached.
	if !logs.Hidden || !settings.Hidden {
		t.Errorf("windows must be created hidden; logs=%v settings=%v", logs.Hidden, settings.Hidden)
	}
	if !settings.DisableResize {
		t.Error("the settings dialog is a fixed-size form; DisableResize must be set")
	}
	if logs.Title != "Knomit Logs" || settings.Title != "Knomit Settings" {
		t.Errorf("titles = %q / %q", logs.Title, settings.Title)
	}
}

// End to end over the two pieces this task added: the URL each window loads,
// resolved through the same handler the Wails asset server uses, against the
// real embedded bundle. Catches a vite config that renames an entry document,
// which neither the Go unit tests nor the frontend tests can see.
func TestAuxWindowURLs_ResolveInEmbeddedBundle(t *testing.T) {
	desktopFS, err := desktopui.FS()
	if err != nil {
		t.Fatalf("desktopui.FS(): %v", err)
	}
	if _, err := fs.Stat(desktopFS, "settings.html"); err != nil {
		t.Skip("desktop UI bundle not built; run `make desktop-ui`")
	}

	h := configInjectingHandlerWithDesktop(testUIFS(), desktopFS, staticBase("http://127.0.0.1:19278"))
	for _, url := range []string{logsWindowOptions().URL, settingsWindowOptions().URL} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", url, rec.Code)
		}
	}
}
