package repos

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

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

// An empty bare repo on disk is the case the wizard's "seed" path depends on.
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
