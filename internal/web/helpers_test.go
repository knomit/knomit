package web

import (
	"knomit/internal/foundation/pki"
	"knomit/internal/repos"
	"path/filepath"
	"strings"
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

// internalForms is the fleet transport's internal URL for an https URL, as
// pki.GitURL derives it, in lower and upper case: the scheme must be refused
// as input however it is spelled (go-git lowercases it).
func internalForms(t *testing.T, https string) []string {
	t.Helper()
	u, err := pki.GitURL(https, true)
	if err != nil {
		t.Fatal(err)
	}
	scheme, rest, _ := strings.Cut(u, "://")
	return []string{u, strings.ToUpper(scheme) + "://" + rest}
}

// TestValidateURLAuth_FleetPeersAreDecidedNotTyped is the table the wizard's
// urlAuthMismatch test mirrors (web/src/originAuth.test.ts) for the rows both
// apply; the two must not disagree. A fleet peer is typed as plain https and
// knomit decides it is one, so cert is never an input and the internal
// scheme is refused.
//
// SABOTAGE: dropping the cert rule turns the cert rows red; dropping the
// scheme rule turns the internal-form rows red.
func TestValidateURLAuth_FleetPeersAreDecidedNotTyped(t *testing.T) {
	certWrong := repos.ErrCertNotAnInput.Error()
	schemeWrong := pki.ErrUnsupportedOriginScheme.Error()
	type row struct{ url, method, want string }
	rows := []row{
		{"https://github.com/o/r.git", "token", ""},
		{"https://h:8443/git/kb", "", ""},
		{"https://h:8443/git/kb", "basic", ""},
		{"https://github.com/o/r.git", "cert", certWrong},
		{"git@github.com:o/r.git", "cert", certWrong},
		{"/srv/kb", "cert", certWrong},
		{"https://github.com/o/r.git", "ssh", "SSH auth cannot be used with HTTP/HTTPS URLs — use a token or basic auth instead"},
		{"git@github.com:o/r.git", "token", "token/basic auth cannot be used with SSH URLs — use SSH auth instead"},
		{"ssh://git@github.com/o/r.git", "basic", "token/basic auth cannot be used with SSH URLs — use SSH auth instead"},
	}
	for _, u := range internalForms(t, "https://h:8443/git/kb") {
		for _, method := range []string{"", "cert", "token", "ssh"} {
			rows = append(rows, row{u, method, schemeWrong})
		}
	}
	for _, tc := range rows {
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
