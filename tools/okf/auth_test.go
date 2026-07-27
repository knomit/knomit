package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
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
	"golang.org/x/crypto/ssh/knownhosts"
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

// safeURL is redactURL plus a guarantee for the input redactURL cannot handle:
// one url.Parse rejects, which redactURL returns VERBATIM. Every error and log
// line in this package must go through safeURL for that reason.
func TestSafeURL(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		// Parseable: identical to redactURL, including the ssh login-name rule.
		{"https userinfo stripped", "https://me:tok@github.com/me/kb.git", "https://github.com/me/kb.git"},
		{"ssh url keeps login name", "ssh://git@github.com/me/kb.git", "ssh://git@github.com/me/kb.git"},
		{"anonymous knomit untouched", "http://localhost:19278/git/knomit-kb", "http://localhost:19278/git/knomit-kb"},
		{"local path untouched", "/srv/kb-mirror.git", "/srv/kb-mirror.git"},

		// Unparseable: redactURL would hand these back with the token intact.
		{"unparseable https keeps nothing before the @",
			"https://me:supersecret@github.com:port/kb.git", "***@github.com:port/kb.git"},
		{"unparseable ssh keeps nothing before the @",
			"ssh://user:supersecret@example.com:port/kb.git", "***@example.com:port/kb.git"},

		// scp shorthand has no password field at all, so its userinfo is a
		// login name — dropping it would print an address that does not resolve.
		{"scp-like untouched", "git@github.com:me/kb.git", "git@github.com:me/kb.git"},
		// ...but a colon in it means it is not scp shorthand at all. url.Parse
		// ACCEPTS this one, as scheme "user" with an opaque remainder, so
		// redactURL sees no userinfo and returns it whole.
		{"opaque parse with a colon in the userinfo is redacted",
			"user:supersecret@github.com:me/kb.git", "***@github.com:me/kb.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, safeURL(tc.in))
		})
	}
}

// Every unparseable, credential-bearing shape must come out clean. redactURL
// fails ALL of these by design (it returns its input when url.Parse errors),
// which is the whole reason safeURL exists.
func TestSafeURL_NeverLeaksAnUnparseableCredential(t *testing.T) {
	for _, raw := range []string{
		"https://me:supersecret@github.com:port/kb.git",
		"ssh://me:supersecret@github.com:port/kb.git",
		"https://supersecret@github.com:port/kb.git",
		"user:supersecret@host:path/kb.git",
	} {
		require.Contains(t, redactURL(raw), "supersecret",
			"test premise: redactURL is the thing that cannot handle this")
		require.NotContains(t, safeURL(raw), "supersecret", "safeURL must redact %s", raw)
	}
}

// wrapURLError must scrub the WRAPPED error's own text, not just our "%s" of
// the URL. Two layers below us print credentials on their own: net/http's
// *url.Error strips only the password (so a token as the username survives),
// and net/url.Parse quotes its entire input. It must do that WITHOUT breaking
// errors.Is, which explainFetchError depends on to recognise a 401.
func TestWrapURLError_ScrubsTheWrappedTextAndKeepsErrorsIs(t *testing.T) {
	t.Run("token as the username, quoted by the layer below", func(t *testing.T) {
		const raw = "https://ghp_secret@127.0.0.1:1/kb.git"
		inner := fmt.Errorf(`Get "https://ghp_secret@127.0.0.1:1/kb.git/info/refs": dial tcp: refused`)
		err := wrapURLError("fetch", raw, inner)
		require.NotContains(t, err.Error(), "ghp_secret")
		require.Contains(t, err.Error(), "***@127.0.0.1:1")
	})

	t.Run("url.Parse quotes the whole unparseable input", func(t *testing.T) {
		const raw = "https://user:ghp_secret@127.0.0.1:nope/kb.git"
		inner := fmt.Errorf(`parse %q: invalid port ":nope" after host`, raw)
		err := wrapURLError("fetch", raw, inner)
		require.NotContains(t, err.Error(), "ghp_secret")
	})

	t.Run("errors.Is still reaches the cause", func(t *testing.T) {
		err := wrapURLError("fetch", "https://me:tok@github.com/me/kb.git",
			transport.ErrAuthenticationRequired)
		require.ErrorIs(t, err, transport.ErrAuthenticationRequired,
			"explainFetchError recognises a 401 through this wrapper")
		require.NotContains(t, err.Error(), "tok")
	})

	t.Run("an ssh login name is kept, its password is not", func(t *testing.T) {
		err := wrapURLError("fetch", "ssh://git:supersecret@github.com/me/kb.git",
			errors.New("ssh://git:supersecret@github.com/me/kb.git: handshake failed"))
		require.NotContains(t, err.Error(), "supersecret")
		require.Contains(t, err.Error(), "git@github.com", "the login name is not a secret")
	})
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

// An ALREADY-PUBLISHED source URL is redacted on the way through too. This is
// the remediation path for a config an older build wrote (or a user hand-edited)
// with a credential in it: preserving cfg.Source verbatim would republish the
// token on every sync forever, and rewriting the file is the only chance to
// remove it.
func TestRenderFiles_PrevSourceIsRedacted(t *testing.T) {
	_, kbRepo := newKB(t)
	head, err := kbRepo.Head()
	require.NoError(t, err)

	var out bytes.Buffer
	files, _, err := renderFiles(exportRequest{
		repo: kbRepo, dir: t.TempDir(), branch: head.Name().Short(), head: head.Hash(),
		// No publishSource: this is a bare sync, which merely PRESERVES what
		// was published before — and must launder it on the way.
		prevSource: "https://me:supersecret@github.com/me/kb.git",
		ui:         newUI(&out),
	})
	require.NoError(t, err)

	cfg := string(files[configFile])
	require.NotContains(t, cfg, "supersecret",
		"a token already in the committed config must not survive the next sync")
	require.Contains(t, cfg, "source: https://github.com/me/kb.git",
		"the address itself is preserved — only the credential goes")
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

	// An explicitly named credential source that yields nothing must FAIL. The
	// alternative — falling through to $KNOMIT_OKF_TOKEN — sends a different
	// token than the one the user chose, and does it silently.
	t.Run("an empty token file is an error, not a fallback", func(t *testing.T) {
		t.Setenv("KNOMIT_OKF_TOKEN", "ghp_fromenv")
		for _, body := range []string{"", "   \n\t\n"} {
			p := filepath.Join(t.TempDir(), "tok")
			require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
			o := authOpts{tokenFile: p}
			err := o.resolve()
			require.ErrorContains(t, err, "empty", "an empty --token-file must error")
			require.ErrorContains(t, err, p, "and must name the file")
			require.NotEqual(t, "ghp_fromenv", o.token,
				"the environment must never override an explicitly named token file")
		}
	})

	// validate is what `branches --no-fetch` calls: it must reject a
	// contradictory command line without reading a file or the environment.
	t.Run("validate catches the flag conflict on its own", func(t *testing.T) {
		o := authOpts{token: "a", tokenFile: filepath.Join(t.TempDir(), "does-not-exist")}
		require.ErrorContains(t, o.validate(), "mutually exclusive")
		require.False(t, authOpts{}.specified())
		require.True(t, authOpts{sshKey: "k"}.specified())
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

// "pass --token" is wrong advice when the user already passed one — in the
// URL. The 401 must say the supplied credentials were REJECTED, or they will
// go looking for a flag that would change nothing.
func TestExplainFetchError_CredentialsInTheURLGetAccurateAdvice(t *testing.T) {
	err := explainFetchError(transport.ErrAuthenticationRequired,
		"https://me:supersecret@github.com/me/kb.git", authOpts{})
	require.ErrorContains(t, err, "rejected the credentials embedded in the source URL")
	require.NotContains(t, err.Error(), "requires credentials: pass --token",
		"the user did supply credentials; telling them to supply some is misleading")
	require.NotContains(t, err.Error(), "supersecret")

	// With no userinfo the original advice is still the right advice.
	err = explainFetchError(transport.ErrAuthenticationRequired,
		"https://github.com/me/kb.git", authOpts{})
	require.ErrorContains(t, err, "requires credentials: pass --token")
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

// A host-key MISMATCH and an UNKNOWN host read alike and are fixed by opposite
// actions. Telling a user to append the key that just failed to match — which
// the ssh-keyscan hint does — silences the one check standing between them and
// a machine-in-the-middle. The two must never share a message.
func TestExplainFetchError_HostKeyMismatchNeverSuggestsKeyscan(t *testing.T) {
	// The typed error x/crypto returns for a mismatch: Want is populated.
	typed := &knownhosts.KeyError{Want: []knownhosts.KnownKey{{Filename: "known_hosts", Line: 3}}}
	wrapped := fmt.Errorf("ssh: handshake failed: %w", typed)

	err := explainFetchError(wrapped, "git@github.com:me/kb.git", authOpts{})
	require.ErrorContains(t, err, "does NOT match")
	require.ErrorContains(t, err, "ssh-keygen -R github.com", "the fix is to remove the stale entry")
	// The message may NAME ssh-keyscan to warn against it; what it must never
	// carry is the command that appends the mismatching key to known_hosts.
	require.NotContains(t, err.Error(), ">> ~/.ssh/known_hosts",
		"appending the mismatching key would silence a possible machine-in-the-middle")
	require.ErrorContains(t, err, "do NOT run ssh-keyscan",
		"and the instinct it preempts should be named outright")

	// The untyped fallback classifies the same way.
	err = explainFetchError(errors.New("ssh: handshake failed: knownhosts: key mismatch"),
		"git@github.com:me/kb.git", authOpts{})
	require.NotContains(t, err.Error(), ">> ~/.ssh/known_hosts")
}

// An unknown host still gets the keyscan hint — the typed error says so with an
// empty Want, and that is the case keyscan is actually the answer to.
func TestExplainFetchError_TypedUnknownHostStillGetsKeyscan(t *testing.T) {
	wrapped := fmt.Errorf("ssh: handshake failed: %w", &knownhosts.KeyError{})
	err := explainFetchError(wrapped, "git@github.com:me/kb.git", authOpts{})
	require.ErrorContains(t, err, "ssh-keyscan github.com")
	require.NotContains(t, err.Error(), "does NOT match")
}

// --username names the basic-auth field a TOKEN rides in. On its own it
// authenticates nothing, so the fetch would go out anonymously and fail with a
// 401 — sending the user after a credential problem that is really a
// command-line one, exactly like --ssh-key against an https source.
func TestCheckAuthApplies_UsernameWithoutATokenIsRejected(t *testing.T) {
	_, err := authFor("https://github.com/me/kb.git", authOpts{username: "x-token-auth"})
	require.Error(t, err)
	require.ErrorContains(t, err, "--token")

	// With a token it is exactly the override it exists to be.
	m, err := authFor("https://bitbucket.org/me/kb.git",
		authOpts{username: "x-token-auth", token: "tok"})
	require.NoError(t, err)
	require.Equal(t, "x-token-auth", m.(*githttp.BasicAuth).Username)
}

// An SSH handshake that fails for an unrelated reason must pass through
// untouched. A looser host-key match would dress "no common algorithm for host
// key" up as a possible interception, which is both wrong and alarming.
func TestExplainFetchError_UnrelatedHandshakeFailureIsNotAHostKeyWarning(t *testing.T) {
	raw := errors.New("ssh: handshake failed: ssh: no common algorithm for host key; client offered: [...]")
	err := explainFetchError(raw, "git@github.com:me/kb.git", authOpts{})
	require.Equal(t, raw, err, "an unrelated error must pass through unchanged")
}
