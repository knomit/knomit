package cmd

import (
	"strings"
	"testing"

	"knomit/internal/oauth/idp/idptest"
)

// `knomit oauth idp resolve github <login>` turns a login into the stable
// id the allow list takes (R4): one public lookup, at a moment the operator
// chooses, printing the entry to paste. Nothing in the server resolves
// logins.
func TestOAuthIDPResolve(t *testing.T) {
	f := idptest.New(t, "unused", "unused")
	old := githubAPIBase
	githubAPIBase = f.URL
	t.Cleanup(func() { githubAPIBase = old })

	stdout, _, err := runSplit(t, "", "oauth", "idp", "resolve", "github", "octocat")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(stdout, `allowed_subjects = ["github-583231"]`) || !strings.Contains(stdout, `"octocat"`) {
		t.Fatalf("resolve output:\n%s", stdout)
	}

	// The login GitHub answers with is the provider's bytes: quoted.
	f.SetLookup("mona", idptest.User{ID: 1234567, Login: "mona\x1b[8m"})
	stdout, _, err = runSplit(t, "", "oauth", "idp", "resolve", "github", "mona")
	if err != nil || !strings.Contains(stdout, "github-1234567") || strings.Contains(stdout, "\x1b") {
		t.Fatalf("hostile login: %v %q", err, stdout)
	}

	if _, _, err := runSplit(t, "", "oauth", "idp", "resolve", "github", "nobody-here"); err == nil || !strings.Contains(err.Error(), "no such account") {
		t.Fatalf("unknown login: %v", err)
	}
	if _, _, err := runSplit(t, "", "oauth", "idp", "resolve", "gitlab", "x"); err == nil || !strings.Contains(err.Error(), "github") {
		t.Fatalf("unknown provider: %v", err)
	}
}
