package web

import "testing"

// TestIsGitURL covers the remote-URL shapes knomit accepts: standard schemed
// URLs, SCP-style, local file:// URLs, and bare absolute filesystem paths.
// Relative paths are rejected — they would resolve against the server's cwd.
func TestIsGitURL(t *testing.T) {
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
		{"/srv/kb", true},               // bare absolute path
		{"/Users/me/data/kb", true},     // bare absolute path
		{"./relative/repo", false},      // relative — ambiguous vs server cwd
		{"relative/repo", false},        // relative
		{"repo", false},                 // bare name
		{"", false},                     // empty
	}
	for _, c := range cases {
		if got := isGitURL(c.url); got != c.want {
			t.Errorf("isGitURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}
