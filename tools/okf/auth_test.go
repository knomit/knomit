package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/stretchr/testify/require"
	gossh "golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
)

// redactURL must strip secrets without destroying a URL's usability. An http(s)
// token can appear as EITHER the username or the password, so the whole
// userinfo goes. An ssh username is a LOGIN NAME, not a secret — stripping it
// would publish a source URL that no longer works.
func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"https user and password", "https://me:tok@github.com/me/kb.git", "https://github.com/me/kb.git"},
		{"https token as username", "https://TOKEN@github.com/me/kb.git", "https://github.com/me/kb.git"},
		{"bitbucket token user", "https://x-token-auth:tok@bitbucket.org/me/kb.git", "https://bitbucket.org/me/kb.git"},
		{"scp-like ssh untouched", "git@github.com:me/kb.git", "git@github.com:me/kb.git"},
		{"ssh url keeps login name", "ssh://git@github.com/me/kb.git", "ssh://git@github.com/me/kb.git"},
		{"ssh password dropped", "ssh://git:secret@github.com/me/kb.git", "ssh://git@github.com/me/kb.git"},
		{"anonymous knomit untouched", "http://localhost:19278/git/knomit-kb", "http://localhost:19278/git/knomit-kb"},
		{"local path untouched", "/srv/kb-mirror.git", "/srv/kb-mirror.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, redactURL(tc.in))
		})
	}
}

// A credential-bearing source URL must never reach the COMMITTED config —
// .knomit-okf.yaml is pushed, so a token there is a published token.
func TestRenderFiles_PublishSourceStripsCredentials(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	_ = kbDir
	head, err := kbRepo.Head()
	require.NoError(t, err)

	var out bytes.Buffer
	files, _, err := renderFiles(exportRequest{
		repo: kbRepo, dir: t.TempDir(), branch: head.Name().Short(), head: head.Hash(),
		source: "https://me:supersecret@github.com/me/kb.git", publishSource: true,
		ui: newUI(&out),
	})
	require.NoError(t, err)

	cfg := string(files[configFile])
	require.NotContains(t, cfg, "supersecret", "the committed config must not carry a token")
	require.Contains(t, cfg, "source: https://github.com/me/kb.git")
}

// A knomit instance needs no credentials, and a local path cannot take them.
// Returning nil for both is what keeps the common case untouched by this work.
func TestAuthFor_NilWhenNoCredentialsApply(t *testing.T) {
	m, err := authFor("http://localhost:19278/git/knomit-kb", authOpts{})
	require.NoError(t, err)
	require.Nil(t, m, "an anonymous knomit source must stay anonymous")

	m, err = authFor("/srv/kb-mirror.git", authOpts{token: "tok"})
	require.NoError(t, err)
	require.Nil(t, m, "a local path takes no credentials even if a token is set")
}

// GitHub, GitLab and Bitbucket all reject `Authorization: Bearer` for
// git-over-HTTPS; they want the token as the basic-auth PASSWORD. Using
// go-git's TokenAuth here would fail against every host we target.
func TestAuthFor_TokenIsBasicAuthNotBearer(t *testing.T) {
	m, err := authFor("https://github.com/me/kb.git", authOpts{token: "ghp_xyz"})
	require.NoError(t, err)

	basic, ok := m.(*githttp.BasicAuth)
	require.True(t, ok, "token auth must be BasicAuth, never TokenAuth (Bearer)")
	require.Equal(t, "ghp_xyz", basic.Password)
	require.Equal(t, defaultTokenUser, basic.Username)
}

// Bitbucket access tokens require the literal user "x-token-auth"; GitHub and
// GitLab ignore the field. Hence an override rather than a constant.
func TestAuthFor_UsernameOverride(t *testing.T) {
	m, err := authFor("https://bitbucket.org/me/kb.git",
		authOpts{token: "tok", username: "x-token-auth"})
	require.NoError(t, err)
	require.Equal(t, "x-token-auth", m.(*githttp.BasicAuth).Username)
}

func TestAuthOpts_Resolve(t *testing.T) {
	t.Run("token file is read and trimmed", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "tok")
		require.NoError(t, os.WriteFile(p, []byte("ghp_fromfile\n"), 0o600))
		o := authOpts{tokenFile: p}
		require.NoError(t, o.resolve())
		require.Equal(t, "ghp_fromfile", o.token)
	})

	t.Run("both token sources is an error", func(t *testing.T) {
		o := authOpts{token: "a", tokenFile: "b"}
		require.ErrorContains(t, o.resolve(), "mutually exclusive")
	})

	t.Run("environment fills an unset token", func(t *testing.T) {
		t.Setenv("KNOMIT_OKF_TOKEN", "ghp_fromenv")
		o := authOpts{}
		require.NoError(t, o.resolve())
		require.Equal(t, "ghp_fromenv", o.token)
	})

	t.Run("flag beats environment", func(t *testing.T) {
		t.Setenv("KNOMIT_OKF_TOKEN", "ghp_fromenv")
		o := authOpts{token: "ghp_fromflag"}
		require.NoError(t, o.resolve())
		require.Equal(t, "ghp_fromflag", o.token)
	})
}

// writeTestKey generates an unencrypted ed25519 private key in OpenSSH format.
func writeTestKey(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := gossh.MarshalPrivateKey(priv, "")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(block), 0o600))
}

// An explicit --ssh-key must win over everything, including a populated agent.
func TestSSHAuth_ExplicitKeyWins(t *testing.T) {
	key := filepath.Join(t.TempDir(), "id_test")
	writeTestKey(t, key)

	m, err := authFor("git@github.com:me/kb.git", authOpts{sshKey: key})
	require.NoError(t, err)

	pk, ok := m.(*gitssh.PublicKeys)
	require.True(t, ok, "--ssh-key must produce PublicKeys")
	require.Equal(t, "git", pk.User, "the ssh user comes from the URL")
}

// NewSSHAgentAuth succeeds whenever the agent SOCKET exists, even with zero
// identities loaded — that is the common case this whole fallback chain
// exists to handle. Without probing the agent for signers, a naive
// `if err == nil { return agentAuth }` would win here and the
// default-identity fallback below it would never run in practice. This test
// serves a real, empty agent.Keyring over a real unix socket so the
// `m.Callback()` probe in sshAuth is genuinely exercised, not just the
// no-socket-at-all case covered by TestSSHAuth_FallsBackToDefaultIdentity.
func TestSSHAuth_AgentWithZeroIdentitiesFallsBackToDefaultIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SSH_AUTH_SOCK is POSIX-only")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows
	writeTestKey(t, filepath.Join(home, ".ssh", "id_ed25519"))

	// A short, independent temp dir: unix socket paths are capped at ~104
	// bytes on macOS/BSD, and t.TempDir() here would nest under this test's
	// (long) name and blow that limit.
	sockDir, err := os.MkdirTemp("", "okfssh")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "agent.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err, "must be able to open a unix socket for the fake agent")

	keyring := sshagent.NewKeyring() // empty: a real agent, zero identities
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed below
			}
			go sshagent.ServeAgent(keyring, conn)
		}
	}()
	// t.Cleanup runs LIFO: close the listener (which unblocks Accept and lets
	// the goroutine exit) BEFORE waiting for it, or this would deadlock.
	t.Cleanup(func() { <-done })
	t.Cleanup(func() { _ = ln.Close() })

	t.Setenv("SSH_AUTH_SOCK", sockPath)

	m, err := authFor("git@github.com:me/kb.git", authOpts{})
	require.NoError(t, err)
	require.IsType(t, &gitssh.PublicKeys{}, m,
		"an agent socket with zero identities must be treated as no credential, "+
			"not returned as the winning auth method")
}

// The default-identity fallback is what makes a repo that clones fine with
// plain `git` work here too. It is reachable ONLY if an empty agent is treated
// as "no credentials", so this test pins that behaviour.
func TestSSHAuth_FallsBackToDefaultIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows
	t.Setenv("SSH_AUTH_SOCK", "") // no agent
	writeTestKey(t, filepath.Join(home, ".ssh", "id_ed25519"))

	m, err := authFor("git@github.com:me/kb.git", authOpts{})
	require.NoError(t, err)
	require.IsType(t, &gitssh.PublicKeys{}, m, "~/.ssh/id_ed25519 must be found")
}

// The error has to say what was tried, or the user cannot act on it.
func TestSSHAuth_ErrorListsWhatWasTried(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	_, err := authFor("git@github.com:me/kb.git", authOpts{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--ssh-key")
	require.Contains(t, err.Error(), "id_ed25519")
	require.Contains(t, err.Error(), "ssh-add", "the error must carry a fix")
}

// A bad key path fails loudly rather than silently falling through to another
// credential: the user asked for THIS key.
func TestSSHAuth_ExplicitKeyErrorIsNotSwallowed(t *testing.T) {
	_, err := authFor("git@github.com:me/kb.git",
		authOpts{sshKey: filepath.Join(t.TempDir(), "missing")})
	require.ErrorContains(t, err, "missing")
}

// go-git offers no interactive "continue connecting (yes/no)?", so a first
// contact with an unknown host fails with an opaque error. The fix must be in
// the message.
func TestExplainFetchError_UnknownHostKey(t *testing.T) {
	raw := errors.New("ssh: handshake failed: knownhosts: key is unknown")
	err := explainFetchError(raw, "git@github.com:me/kb.git", authOpts{})
	require.ErrorContains(t, err, "known_hosts")
	require.ErrorContains(t, err, "ssh-keyscan github.com")
}

// A 401 must say which credential was used, or a stale token is
// indistinguishable from no token at all.
func TestExplainFetchError_AuthRequiredNamesTheSource(t *testing.T) {
	err := explainFetchError(transport.ErrAuthenticationRequired,
		"https://github.com/me/kb.git", authOpts{})
	require.ErrorContains(t, err, "--token")

	err = explainFetchError(transport.ErrAuthorizationFailed,
		"https://bitbucket.org/me/kb.git", authOpts{token: "tok"})
	require.ErrorContains(t, err, "--username",
		"a Bitbucket token with the wrong user fails exactly this way")
}

// An unrelated error must pass through untouched rather than acquire a
// misleading auth hint.
func TestExplainFetchError_PassesOtherErrorsThrough(t *testing.T) {
	raw := errors.New("connection refused")
	require.Equal(t, raw, explainFetchError(raw, "http://localhost:1/x", authOpts{}))
}

// A credential-bearing URL must not reach a terminal or a CI log.
func TestExplainFetchError_RedactsTheURL(t *testing.T) {
	err := explainFetchError(transport.ErrAuthenticationRequired,
		"https://me:supersecret@github.com/me/kb.git", authOpts{})
	require.NotContains(t, err.Error(), "supersecret")
}

// The known_hosts fallback (transport.NewEndpoint cannot parse rawURL) must
// never print rawURL verbatim. redactURL cannot be trusted to have cleaned it
// first: NewEndpoint's only failure path for a URL like this is net/url.Parse
// itself — the exact parse redactURL also runs — so whenever NewEndpoint
// fails here, redactURL has independently failed on the identical string and
// silently returned it unredacted. This URL both fails NewEndpoint (an
// invalid, non-numeric port) and carries real userinfo, proving the leak is
// reachable, not hypothetical.
func TestExplainFetchError_UnknownHostKeyNeverLeaksAnUnparseableURL(t *testing.T) {
	const bad = "ssh://user:supersecret@example.com:port/kb.git"
	_, epErr := transport.NewEndpoint(bad)
	require.Error(t, epErr, "test premise: this URL must be one NewEndpoint cannot parse")

	raw := errors.New("ssh: handshake failed: knownhosts: key is unknown")
	err := explainFetchError(raw, bad, authOpts{})
	require.NotContains(t, err.Error(), "supersecret", "a credential must never survive an unparseable source URL")
	require.NotContains(t, err.Error(), "user:supersecret@", "no userinfo segment may survive either")
}
