package repos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// newProbeTestManager returns an unstarted Manager whose LocalOriginRoot is
// root. ProbeOrigin routes filesystem origins through the same gate as every
// clone/fetch path, so a test exercising a real local-origin probe needs the
// gate open under a root it controls — newTestManager sets no
// LocalOriginRoot at all, which would disable filesystem origins entirely and
// make every probe below fail on the gate rather than exercise ProbeOrigin.
func newProbeTestManager(t *testing.T, root string) *Manager {
	t.Helper()
	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: t.TempDir(), LocalOriginRoot: root},
		AgentBranch:           "agent/test",
		DisableBackgroundSync: true,
	})
	t.Cleanup(func() { m.Close() })
	return m
}

// initBareRepo creates an empty bare git repo (no commits) under parent and
// returns a file:// URL to it.
func initBareRepo(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", dir).Run())
	return "file://" + dir
}

// An empty bare repo on disk is the case the wizard BLOCKS on: with no
// branches there is nothing to cut an agent branch from, and knomit never
// creates a branch on a remote other than its own.
func TestProbeOrigin_EmptyLocalRepo(t *testing.T) {
	root := t.TempDir()
	m := newProbeTestManager(t, root)
	url := initBareRepo(t, root)

	got, err := m.ProbeOrigin(context.Background(), OriginSpec{URL: url})
	require.NoError(t, err)
	require.True(t, got.Reachable, "Reachable = false, want true")
	require.True(t, got.Empty, "Empty = false, want true")
	require.NotNil(t, got.Branches, "Branches must be [] not JSON null, for the web client's string[] type")
}

func TestProbeOrigin_PopulatedLocalRepo(t *testing.T) {
	root := t.TempDir()
	m := newProbeTestManager(t, root)
	// seedBareRemote (lifecycle_test.go) builds a bare repo with one commit on
	// main and returns its file:// URL.
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	got, err := m.ProbeOrigin(context.Background(), OriginSpec{URL: url})
	require.NoError(t, err)
	require.False(t, got.Empty, "Empty = true, want false")
	require.Equal(t, "main", got.UpstreamBranch)
}

// The local-origin gate admits no exemption — a probe that skipped it would be
// a new hole in exactly the invariant every other clone/fetch path honours.
func TestProbeOrigin_RejectsUngatedLocalPath(t *testing.T) {
	m := newProbeTestManager(t, t.TempDir()) // root that does NOT contain /etc
	_, err := m.ProbeOrigin(context.Background(), OriginSpec{URL: "/etc"})
	require.Error(t, err, "expected the local-origin gate to reject an out-of-root path")
}

func TestProbeOrigin_UnreachableIsNotAnError(t *testing.T) {
	root := t.TempDir()
	m := newProbeTestManager(t, root)

	got, err := m.ProbeOrigin(context.Background(), OriginSpec{URL: filepath.Join(root, "does-not-exist")})
	require.NoError(t, err, "probe should report unreachability as a result, not an error")
	require.False(t, got.Reachable, "Reachable = true, want false")
	require.NotNil(t, got.Branches, "Branches must be [] not JSON null, for the web client's string[] type")
}

// A credential this machine cannot assemble at all — an SSH URL with no key
// configured (TestManager_ResolveAuth_SSHNoKeyFails) — is an AUTH result, not
// an unreachable one. Nothing in ProbeOrigin has touched the network at that
// point, so "could not reach that remote" is a claim about the remote with no
// evidence behind it.
//
// The consequence is what this pins: the web wizard's stepsFor collapses an
// unreachable probe to ['source'], which removes the access step — the only
// place a credential can be entered. Reporting this as unreachable therefore
// named the wrong cause AND locked the user out of the fix, with no way to
// create the repo at all. Reachable+AuthRequired keeps the access step in the
// list (see wizardState.test.ts's stepsFor cases) and lands in
// initializeProbeErr's authentication arm rather than its reachability one.
func TestProbeOrigin_UnresolvableCredentialIsAuthNotUnreachable(t *testing.T) {
	// KeyPath "" is the no-key-on-this-machine case; the URL is SSH-style so
	// ResolveAuth auto-detects ssh and then fails for want of a key.
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "agent/test", KeyPath: "", DisableBackgroundSync: true,
	})
	t.Cleanup(func() { m.Close() })

	got, err := m.ProbeOrigin(context.Background(), OriginSpec{URL: "git@github.com:user/repo.git", AuthMethod: "ssh"})
	require.NoError(t, err, "a credential failure is a RESULT, not an error")
	require.True(t, got.Reachable, "a credential we could not assemble says nothing about reachability")
	require.True(t, got.AuthRequired, "an unresolvable credential must be reported as an auth problem")
	require.Contains(t, got.Detail, "key path", "the underlying cause must survive into Detail")
	require.NotNil(t, got.Branches, "Branches must never serialize as JSON null")

	// initializeProbeErr must read it as authentication, not "no branches":
	// steering the user back to their host to create a branch they already have
	// is the wrong instruction, and mode "clone" would fail against the same
	// missing key anyway.
	serr := initializeProbeErr(got)
	require.Error(t, serr)
	require.False(t, errors.Is(serr, ErrRemoteNoBranches))
	require.Contains(t, serr.Error(), "requires authentication")
}

// TestClassifyProbeError unit-tests classifyProbeError directly against
// synthetic errors — no network or live server needed. This is the coverage
// that was missing: ProbeOrigin's own tests only ever hit real go-git errors
// reachable from a local file:// remote (ErrEmptyRemoteRepository), so an
// SSH auth failure's classification was never exercised end to end.
func TestClassifyProbeError(t *testing.T) {
	// A representative SSH auth failure, shaped exactly like what go-git's
	// ssh transport actually propagates: it wraps whatever
	// golang.org/x/crypto/ssh's NewClientConn returned in
	// fmt.Errorf("ssh: handshake failed: %w", err) (x/crypto/ssh client.go),
	// and when every offered auth method is rejected that inner error is the
	// bare fmt.Errorf("ssh: unable to authenticate, attempted methods %v, no
	// supported methods remain", tried) from x/crypto/ssh's client_auth.go —
	// no sentinel, no type, just this message text.
	sshAuthFailure := fmt.Errorf("ssh: handshake failed: %w",
		errors.New("ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"))

	cases := []struct {
		name             string
		err              error
		wantEmpty        bool
		wantAuthRequired bool
	}{
		{
			name:      "empty remote",
			err:       transport.ErrEmptyRemoteRepository,
			wantEmpty: true,
		},
		{
			name:             "http authentication required (401)",
			err:              fmt.Errorf("%w: token invalid", transport.ErrAuthenticationRequired),
			wantAuthRequired: true,
		},
		{
			name:             "http authorization failed (403)",
			err:              fmt.Errorf("%w: token invalid", transport.ErrAuthorizationFailed),
			wantAuthRequired: true,
		},
		{
			name:             "ssh authentication failure",
			err:              sshAuthFailure,
			wantAuthRequired: true,
		},
		// The create wizard's reported break: an SSH URL with a token typed
		// under auto-detect. resolveAuth turns the token into githttp.BasicAuth,
		// go-git dispatches by scheme, and the ssh transport rejects it before
		// any network call. Classified as unreachable it collapsed stepsFor to
		// ['source'] and threw the user back to the first screen — with the
		// access step, the only place the credential can be corrected, gone.
		{
			name:             "credential does not fit the URL's transport",
			err:              transport.ErrInvalidAuthMethod,
			wantAuthRequired: true,
		},
		{
			name:             "wrapped invalid auth method",
			err:              fmt.Errorf("probe origin: %w", transport.ErrInvalidAuthMethod),
			wantAuthRequired: true,
		},
		{
			name: "plain network error (unreachable, not auth)",
			err:  errors.New("dial tcp 127.0.0.1:22: connect: connection refused"),
		},
		{
			name: "ssh error that is not an auth failure (e.g. host-key mismatch)",
			err:  errors.New("ssh: handshake failed: knownhosts: key mismatch"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			empty, authRequired := classifyProbeError(c.err)
			require.Equal(t, c.wantEmpty, empty, "empty")
			require.Equal(t, c.wantAuthRequired, authRequired, "authRequired")
		})
	}
}

func TestClassifyProbeError_NilIsNeitherEmptyNorAuthRequired(t *testing.T) {
	empty, authRequired := classifyProbeError(nil)
	require.False(t, empty)
	require.False(t, authRequired)
}

// A remote that accepts the connection and then never answers must not hang
// the wizard's first step: ProbeOrigin bounds the ref listing by
// Cfg.Git.NetworkTimeout, exactly as every other remote git call in the
// package does, and reports the expiry as an ordinary unreachable result.
//
// The detail deliberately does NOT read "context deadline exceeded": that is
// go-git's plumbing leaking into a sentence the user is asked to act on.
func TestProbeOrigin_HonoursNetworkTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	// Registered in this order so LIFO cleanup releases the handler BEFORE
	// srv.Close, which otherwise blocks forever waiting on it.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: t.TempDir(), Git: config.GitConfig{NetworkTimeout: 150 * time.Millisecond}},
		AgentBranch: "agent/test", DisableBackgroundSync: true,
	})
	t.Cleanup(func() { m.Close() })

	start := time.Now()
	res, err := m.ProbeOrigin(context.Background(), OriginSpec{URL: srv.URL + "/r.git"})
	elapsed := time.Since(start)

	require.NoError(t, err, "a timed-out probe is a RESULT, not an error")
	require.False(t, res.Reachable)
	require.Contains(t, res.Detail, "timed out after")
	require.NotContains(t, res.Detail, "context deadline exceeded")
	require.Less(t, elapsed, 5*time.Second, "probe did not honour the configured network timeout")
	require.NotNil(t, res.Branches, "Branches must never serialize as JSON null")
}

// A caller that cancels keeps go-git's own error rather than being told the
// remote timed out — the caller already knows why it stopped, and claiming a
// timeout would misattribute its own cancel to the remote.
func TestProbeFailureDetail_CallerCancelKeepsTheUnderlyingError(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	derived, dcancel := probeCtx(parent, time.Millisecond)
	defer dcancel()
	<-derived.Done()

	require.Equal(t, "boom", probeFailureDetail(parent, derived, errors.New("boom"), time.Millisecond))
}

// timeout <= 0 keeps the legacy unbounded behaviour, matching internal/store's
// netCtxWith — a config that switches the bound off must not make every probe
// fail instantly instead.
func TestProbeCtx_ZeroTimeoutIsUnbounded(t *testing.T) {
	ctx, cancel := probeCtx(context.Background(), 0)
	defer cancel()
	_, ok := ctx.Deadline()
	require.False(t, ok)
}
