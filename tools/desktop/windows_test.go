//go:build desktop

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uiDir is the desktop-only frontend package, relative to this test's working
// directory (tools/desktop). Its entry documents are COMMITTED SOURCE, not
// build output, which is what lets the test below run without a built bundle.
const uiDir = "ui"

// The coupling this file exists for: each window loads a URL under /desktop/,
// and the file that URL names has to be a real vite entry document. Break
// either half and nothing fails loudly — an unbuilt entry 404s inside a webview
// nobody is watching, and a path that escapes /desktop/ is answered 200 with
// the knowledge app's index.html by the SPA fallback. Both present as "the
// Settings window looks wrong", hours after the rename that caused it.
//
// Asserted against the committed .html sources rather than dist/, so it runs in
// CI: .github/workflows/tests.yml builds ./tools/desktop directly and never
// runs `make desktop-ui`, so dist/ there holds only the .gitkeep sentinel. A
// version of this test that read the embedded FS would skip on every CI run —
// which is to say, it would never have run at all.
func TestAuxWindowURLs_MatchCommittedEntryDocuments(t *testing.T) {
	viteConfig, err := os.ReadFile(filepath.Join(uiDir, "vite.config.ts"))
	if err != nil {
		t.Fatalf("read vite config: %v", err)
	}

	for name, url := range map[string]string{
		"logs":     logsWindowOptions().URL,
		"settings": settingsWindowOptions().URL,
	} {
		doc, ok := strings.CutPrefix(url, desktopPrefix)
		if !ok {
			t.Errorf("%s window URL = %q, want it under %q", name, url, desktopPrefix)
			continue
		}
		if _, err := os.Stat(filepath.Join(uiDir, doc)); err != nil {
			t.Errorf("%s window loads %q, but %s/%s does not exist: %v", name, url, uiDir, doc, err)
			continue
		}
		// Existing but unlisted is the nastier half: the source document sits
		// there looking correct while vite never emits it, so dist/ has no such
		// file and the window comes up blank.
		if !strings.Contains(string(viteConfig), "'"+doc+"'") {
			t.Errorf("%s/%s exists but is not a rollup input in vite.config.ts, so it is never built", uiDir, doc)
		}
	}

	if logsWindowOptions().URL == settingsWindowOptions().URL {
		t.Error("both windows load the same URL; they must be distinct documents")
	}
}

// Window options that encode a requirement rather than a restatement of the
// constant next door. Titles and URL prefixes are deliberately not checked
// here — asserting a literal against the literal it was defined from proves
// only that the compiler works.
func TestAuxWindowOptions_RequiredFlags(t *testing.T) {
	logs := logsWindowOptions()
	settings := settingsWindowOptions()

	// NewWithOptions shows a window immediately otherwise, so an aux window
	// created without this would flash on screen before Show() is reached —
	// including on the login-item launch, where no window should appear at all.
	if !logs.Hidden || !settings.Hidden {
		t.Errorf("windows must be created hidden; logs=%v settings=%v", logs.Hidden, settings.Hidden)
	}
	// Verified live: without it the dialog resizes like any other window.
	if !settings.DisableResize {
		t.Error("the settings dialog is a fixed-size form; DisableResize must be set")
	}
}
