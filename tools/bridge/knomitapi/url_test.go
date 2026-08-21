package knomitapi

import (
	"os"
	"testing"
)

// Pins the exact recent-facts URL the session-start context uses. Commit
// 99ec329 fixed a wrong-endpoint bug here; in unit tests every HTTP call
// fails anyway, so a broken URL and a correct one would be indistinguishable
// without this assertion. Keep it red-when-the-URL-changes.
func TestRecentFactsURL_PinsExactShape(t *testing.T) {
	t.Setenv("KNOMIT_BASE_URL", "http://localhost:19278")
	got := RecentFactsURL("knomit", "machine/host", 200)
	want := "http://localhost:19278/api/v1/repos/knomit/branches/machine:host/facts?sort=recent&limit=200"
	if got != want {
		t.Errorf("RecentFactsURL =\n  %s\nwant\n  %s", got, want)
	}
}

func TestBaseURL_DefaultWhenUnset(t *testing.T) {
	old, had := os.LookupEnv("KNOMIT_BASE_URL")
	os.Unsetenv("KNOMIT_BASE_URL")
	defer func() {
		if had {
			os.Setenv("KNOMIT_BASE_URL", old)
		}
	}()
	if got := BaseURL(); got != "http://localhost:19278" {
		t.Errorf("BaseURL() = %q, want default localhost:19278", got)
	}
}

func TestEncodeBranch_SlashBecomesColon(t *testing.T) {
	if got := EncodeBranch("machine/host"); got != "machine:host" {
		t.Errorf("EncodeBranch = %q, want machine:host", got)
	}
}
