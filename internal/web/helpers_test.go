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

// TestValidateURLAuth_FleetScheme is the table the wizard's
// urlAuthMismatch test mirrors (web/src/originAuth.test.ts);
// the two must not disagree. The fleet scheme is matched case-insensitively,
// as go-git matches it.
//
// SABOTAGE (against 3ebcc688): isKnomit forced false turns this red; dropping
// the fleet line from urlAuthMismatch turns six rows of the vitest table and
// the wizard render test red.
func TestValidateURLAuth_FleetScheme(t *testing.T) {
	const knomitWrong = "knomit+https origins authenticate with the instance certificate — use auth method cert"
	const certWrong = "cert auth is only valid with knomit+https:// URLs"
	for _, tc := range []struct {
		url, method, want string
	}{
		{"https://github.com/o/r.git", "token", ""},
		{"https://github.com/o/r.git", "cert", certWrong},
		{"knomit+https://h:8443/git/kb", "cert", ""},
		{"knomit+https://h:8443/git/kb", "", ""},
		{"KNOMIT+HTTPS://h:8443/git/kb", "cert", ""},
		{"knomit+https://h:8443/git/kb", "token", knomitWrong},
		{"knomit+https://h:8443/git/kb", "basic", knomitWrong},
		{"knomit+https://h:8443/git/kb", "none", knomitWrong},
		{"KNOMIT+HTTPS://h:8443/git/kb", "token", knomitWrong},
		{"git@github.com:o/r.git", "cert", certWrong},
		{"ssh://git@github.com/o/r.git", "cert", certWrong},
		{"/srv/kb", "cert", certWrong},
		// isHTTP stays false for the fleet scheme: ssh on it is refused by
		// the fleet rule, not by the HTTP one.
		{"knomit+https://h:8443/git/kb", "ssh", knomitWrong},
	} {
		err := validateURLAuth(tc.url, tc.method)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("validateURLAuth(%q, %q) = %q, want %q", tc.url, tc.method, got, tc.want)
		}
	}
}
