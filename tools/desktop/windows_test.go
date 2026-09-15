//go:build desktop

package main

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// uiDir is the desktop-only frontend package, relative to this test's working
// directory (tools/desktop). Its entry documents are COMMITTED SOURCE, not
// build output, which is what lets the test below run without a built bundle.
const uiDir = "ui"

// The coupling this file exists for: the window loads a URL under /desktop/,
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
}

// The desktop ships exactly ONE aux window, and the tray offers exactly one
// thing to open. Pinned as a set rather than as "settings.html exists", because
// the failure this guards against is a document left BEHIND: the Logs window
// was retired once the web UI grew a Manage → Logs tab that streams the same
// log over the API, and a stale entry document would keep being built, keep
// being served under /desktop/, and keep looking like a live feature to anyone
// who found it.
func TestDesktopShipsOneAuxWindowDocument(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(uiDir, "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	var docs []string
	for _, e := range entries {
		docs = append(docs, filepath.Base(e))
	}
	sort.Strings(docs)
	if want := []string{"settings.html"}; !slices.Equal(docs, want) {
		t.Errorf("desktop entry documents = %v, want %v", docs, want)
	}

	viteConfig, err := os.ReadFile(filepath.Join(uiDir, "vite.config.ts"))
	if err != nil {
		t.Fatalf("read vite config: %v", err)
	}
	// The other half: a document that is gone but still listed breaks the
	// build, and one that is listed but gone is the blank-window case above.
	if strings.Contains(string(viteConfig), "logs.html") {
		t.Error("vite.config.ts still lists logs.html as a rollup input")
	}
}

// Window options that encode a requirement rather than a restatement of the
// constant next door. Titles and URL prefixes are deliberately not checked
// here — asserting a literal against the literal it was defined from proves
// only that the compiler works.
func TestAuxWindowOptions_RequiredFlags(t *testing.T) {
	settings := settingsWindowOptions()

	// NewWithOptions shows a window immediately otherwise, so an aux window
	// created without this would flash on screen before Show() is reached —
	// including on the login-item launch, where no window should appear at all.
	if !settings.Hidden {
		t.Error("the window must be created hidden")
	}
	// Verified live: without it the dialog resizes like any other window.
	if !settings.DisableResize {
		t.Error("the settings dialog is a fixed-size form; DisableResize must be set")
	}
}
