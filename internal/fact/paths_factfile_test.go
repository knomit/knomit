package fact

import "testing"

func TestIsFactFilePath(t *testing.T) {
	for path, want := range map[string]bool{
		"kb/decisions/x/abc.md":       true,
		".knomit/jobs/crawl/state.md": true,
		".github/notes.md":            false,
		".knomit/loose.md":            false,
		"kb/.hidden/x.md":             false,
		"README.md":                   false,
		"kb/decisions/x/abc":          false,
		"other/decisions/x/abc.md":    false,
		".knomit/jobs/../escape/x.md": false,
	} {
		if got := IsFactFilePath("kb", path); got != want {
			t.Errorf("IsFactFilePath(%q) = %v, want %v", path, got, want)
		}
	}
}
