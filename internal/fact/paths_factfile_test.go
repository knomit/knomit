package fact

import "testing"

func TestIsFactFilePath(t *testing.T) {
	for path, want := range map[string]bool{
		"kb/decisions/x/abc.md":         true,
		"artifacts/jobs/crawl/state.md": true,
		"artifacts/loose.md":            false,
		"artifacts/jobs/.x/state.md":    false,
		"artifacts/jobs/crawl/state":    false,
		// F25: .knomit/ is closed to the fact tools, the old job-slot shape
		// included.
		".knomit/jobs/crawl/state.md": false,
		".knomit/skills/s/extra.md":   false,
		".github/notes.md":            false,
		".knomit/loose.md":            false,
		"kb/.hidden/x.md":             false,
		"kb/a/.x.md":                  false,
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
