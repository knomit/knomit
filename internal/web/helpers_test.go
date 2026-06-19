package web

import "testing"

// TestValidateLocalOrigin covers the local-origin policy gate: network origins
// always pass; local origins (bare absolute paths and file:// URLs) are allowed
// only when a root is configured AND the path resolves within it. An empty root
// disables local origins entirely.
func TestValidateLocalOrigin(t *testing.T) {
	cases := []struct {
		name string
		url  string
		root string
		ok   bool
	}{
		// Network origins are never gated, regardless of root.
		{"https no root", "https://github.com/user/repo.git", "", true},
		{"ssh no root", "git@github.com:user/repo.git", "", true},
		{"ssh scheme no root", "ssh://git@github.com/user/repo.git", "", true},

		// Local origins are rejected when no root is configured.
		{"bare abs disabled", "/srv/kb", "", false},
		{"file url disabled", "file:///srv/kb", "", false},

		// Local origins within the configured root are allowed.
		{"bare abs within root", "/srv/kb/work", "/srv/kb", true},
		{"bare abs equal root", "/srv/kb", "/srv/kb", true},
		{"file url within root", "file:///srv/kb/work", "/srv/kb", true},

		// Local origins outside the root are rejected (incl. traversal).
		{"bare abs outside root", "/etc/passwd", "/srv/kb", false},
		{"file url outside root", "file:///etc/passwd", "/srv/kb", false},
		{"traversal escape", "/srv/kb/../etc", "/srv/kb", false},
		{"sibling prefix not contained", "/srv/kb-evil", "/srv/kb", false},

		// A relative (misconfigured) root cannot contain an absolute path.
		{"relative root rejects", "/srv/kb", "relative/root", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateLocalOrigin(c.url, c.root)
			if c.ok && err != nil {
				t.Errorf("validateLocalOrigin(%q, %q) = %v, want nil", c.url, c.root, err)
			}
			if !c.ok && err == nil {
				t.Errorf("validateLocalOrigin(%q, %q) = nil, want error", c.url, c.root)
			}
		})
	}
}

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
		{"/srv/kb", true},           // bare absolute path
		{"/Users/me/data/kb", true}, // bare absolute path
		{"./relative/repo", false},  // relative — ambiguous vs server cwd
		{"relative/repo", false},    // relative
		{"repo", false},             // bare name
		{"", false},                 // empty
	}
	for _, c := range cases {
		if got := isGitURL(c.url); got != c.want {
			t.Errorf("isGitURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}
