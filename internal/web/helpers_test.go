package web

import (
	"path/filepath"
	"testing"
)

// The local-origin policy gate now lives in internal/repos (it is enforced at
// the clone boundary, not the web edge); see repos.TestValidateLocalOrigin.

// TestIsGitURL covers the remote-URL shapes knomit accepts: standard schemed
// URLs, SCP-style, local file:// URLs, and bare absolute filesystem paths.
// Relative paths are rejected — they would resolve against the server's cwd.
func TestIsGitURL(t *testing.T) {
	// "Absolute" is a per-OS question and isGitURL asks filepath.IsAbs, so the
	// fixtures have to be absolute ON THIS HOST. "/srv/kb" is absolute on Unix
	// but merely rooted on Windows — relative to the current drive — and
	// IsAbs says false, so these two rows failed there while testing nothing
	// about the behaviour they were written for.
	absKB := filepath.FromSlash("/srv/kb")
	absHome := filepath.FromSlash("/Users/me/data/kb")
	if filepath.Separator != '/' {
		absKB = `C:\srv\kb`
		absHome = `C:\Users\me\data\kb`
	}

	cases := []struct {
		url  string
		want bool
	}{
		{"https://github.com/user/repo.git", true},
		{"http://example.com/repo.git", true},
		{"ssh://git@github.com/user/repo.git", true},
		{"git://example.com/repo.git", true},
		{"git@github.com:user/repo.git", true},
		{"file:///srv/kb", true},
		{absKB, true},              // bare absolute path
		{absHome, true},            // bare absolute path
		{"./relative/repo", false}, // relative — ambiguous vs server cwd
		{"relative/repo", false},   // relative
		{"repo", false},            // bare name
		{"", false},                // empty
	}
	for _, c := range cases {
		if got := isGitURL(c.url); got != c.want {
			t.Errorf("isGitURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}
