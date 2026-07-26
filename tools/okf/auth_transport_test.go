package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/require"
)

func endpoint(t *testing.T, raw string) *transport.Endpoint {
	t.Helper()
	ep, err := transport.NewEndpoint(raw)
	require.NoError(t, err)
	return ep
}

// A flag the transport cannot use is a command-line error, not a credential
// error. Dropped silently it resurfaces as a 401 whose hint points at the wrong
// thing entirely: "--ssh-key against https" fetched anonymously and was then
// told to pass --token.
func TestCheckAuthApplies_RejectsFlagsTheTransportCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		opts      authOpts
		wantErr   string
	}{
		{
			name: "ssh key against https",
			url:  "https://github.com/me/kb.git",
			opts: authOpts{sshKey: "/k", sshKeyFromFlag: true},
			// The hint has to name the flag that WOULD work.
			wantErr: "--token",
		},
		{
			name:    "token against ssh",
			url:     "git@github.com:me/kb.git",
			opts:    authOpts{token: "ghp_x", tokenFromFlag: true},
			wantErr: "--ssh-key",
		},
		{
			name:    "username against ssh",
			url:     "git@github.com:me/kb.git",
			opts:    authOpts{username: "x-token-auth"},
			wantErr: "--ssh-key",
		},
		{
			name:    "token against a local path",
			url:     "/srv/kb-mirror.git",
			opts:    authOpts{token: "ghp_x", tokenFromFlag: true},
			wantErr: "local source",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.checkAuthApplies(endpoint(t, tc.url))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)

			// And it must surface through authFor, which is the only thing the
			// commands call — a check nothing reaches is not a check.
			_, ferr := authFor(tc.url, tc.opts)
			require.Error(t, ferr)
		})
	}
}

// The matching flag must still work, or the check is just a break.
func TestCheckAuthApplies_AllowsTheMatchingTransport(t *testing.T) {
	require.NoError(t, authOpts{token: "t", tokenFromFlag: true}.
		checkAuthApplies(endpoint(t, "https://github.com/me/kb.git")))
	require.NoError(t, authOpts{username: "x-token-auth", token: "t", tokenFromFlag: true}.
		checkAuthApplies(endpoint(t, "https://bitbucket.org/me/kb.git")))
	require.NoError(t, authOpts{sshKey: "/k", sshKeyFromFlag: true}.
		checkAuthApplies(endpoint(t, "git@github.com:me/kb.git")))
	require.NoError(t, authOpts{}.checkAuthApplies(endpoint(t, "/srv/kb-mirror.git")))
}

// Ambient CI configuration must not break an unrelated fetch. $KNOMIT_OKF_SSH_KEY
// being set in a shell profile is not the user asking THIS run to use a key, so
// an https clone in that shell has to keep working.
func TestCheckAuthApplies_EnvironmentCredentialsAreExempt(t *testing.T) {
	t.Setenv("KNOMIT_OKF_SSH_KEY", "/home/ci/.ssh/id_ed25519")
	t.Setenv("KNOMIT_OKF_TOKEN", "ghp_ambient")

	var o authOpts
	require.NoError(t, o.resolve())
	require.NotEmpty(t, o.sshKey, "the environment was read")
	require.False(t, o.sshKeyFromFlag, "but it is not a flag")

	require.NoError(t, o.checkAuthApplies(endpoint(t, "https://github.com/me/kb.git")),
		"an ambient ssh key must not break an https clone")

	// The token still applies over https, which is the whole point of the env var.
	m, err := authFor("https://github.com/me/kb.git", o)
	require.NoError(t, err)
	require.NotNil(t, m)
}

// resolve is what records flag provenance, so a flag must survive it as a flag.
func TestAuthOpts_ResolveRecordsFlagProvenance(t *testing.T) {
	t.Setenv("KNOMIT_OKF_TOKEN", "")
	t.Setenv("KNOMIT_OKF_SSH_KEY", "")

	o := authOpts{token: "ghp_flag"}
	require.NoError(t, o.resolve())
	require.True(t, o.tokenFromFlag)
	require.False(t, o.sshKeyFromFlag)

	o = authOpts{sshKey: "/k"}
	require.NoError(t, o.resolve())
	require.True(t, o.sshKeyFromFlag)
	require.False(t, o.tokenFromFlag)
}

// A token file is a flag too — the credential arrives on the command line even
// though its value does not.
func TestAuthOpts_ResolveTreatsTokenFileAsAFlag(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(p, []byte("ghp_fromfile\n"), 0o600))

	o := authOpts{tokenFile: p}
	require.NoError(t, o.resolve())
	require.True(t, o.tokenFromFlag)
	require.Error(t, o.checkAuthApplies(endpoint(t, "git@github.com:me/kb.git")))
}
