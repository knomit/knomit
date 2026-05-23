package claude

import (
	"os"
	"testing"
)

// These tests pin the exact URL shapes the post-edit and session-start hooks
// build. Commit 99ec329 fixed a wrong-endpoint bug where post-edit called
// `/search?path=…` (wrong param, silently dropped) and session-start called
// `/activity?limit=5` (wrong endpoint). Without these assertions, a future
// edit could silently regress: in unit tests every HTTP call fails anyway
// (no knomit running), so the hook would skip with the same `skip_reason` for
// both correct and broken URLs. Keep these red-when-the-URL-changes.

func TestPostEditSearchURL_PinsExactShape(t *testing.T) {
	t.Setenv("KNOMIT_BASE_URL", "http://localhost:19278")
	got := postEditSearchURL("knomit", "machine/host", "internal/store/foo.go")
	want := "http://localhost:19278/api/v1/repos/knomit/branches/machine%2Fhost/search?q=internal%2Fstore%2Ffoo.go&limit=20"
	if got != want {
		t.Errorf("postEditSearchURL =\n  %s\nwant\n  %s", got, want)
	}
}

func TestSessionStartFactsURL_PinsExactShape(t *testing.T) {
	t.Setenv("KNOMIT_BASE_URL", "http://localhost:19278")
	got := sessionStartFactsURL("knomit", "machine/host")
	want := "http://localhost:19278/api/v1/repos/knomit/branches/machine%2Fhost/facts?sort=recent&limit=200"
	if got != want {
		t.Errorf("sessionStartFactsURL =\n  %s\nwant\n  %s", got, want)
	}
}

func TestPostEditSearchURL_HonorsKnomitBaseURLEnv(t *testing.T) {
	t.Setenv("KNOMIT_BASE_URL", "http://example.test:9000")
	got := postEditSearchURL("r", "b", "p")
	want := "http://example.test:9000/api/v1/repos/r/branches/b/search?q=p&limit=20"
	if got != want {
		t.Errorf("env override not honored:\n got %s\nwant %s", got, want)
	}
}

// Sanity check that the env hygiene doesn't leak into other tests.
func TestKnomitBaseURL_DefaultWhenUnset(t *testing.T) {
	old, had := os.LookupEnv("KNOMIT_BASE_URL")
	os.Unsetenv("KNOMIT_BASE_URL")
	defer func() {
		if had {
			os.Setenv("KNOMIT_BASE_URL", old)
		}
	}()
	if got := knomitBaseURL(); got != "http://localhost:19278" {
		t.Errorf("knomitBaseURL() = %q, want default localhost:19278", got)
	}
}
