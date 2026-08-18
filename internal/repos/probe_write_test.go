package repos

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/require"
)

// WRITE ACCESS HAS THREE STATES, and until now only two could be produced.
//
// ProbeResult.WriteAccess documents "" as "not established", and every failure
// of the receive-pack advertisement returned "denied" instead — so a timeout, a
// connection reset, a host-key change, or the anonymous 401 a public HTTPS
// remote answers before any credential has been offered all rendered as
// "github.com let knomit read this repository, but not push to it". That is an
// authorization verdict about someone's account, invented from a network error.
//
// Only the server actually refusing the credential is "denied". Everything else
// is the third state, exactly as `initialized` treats it: say nothing, and let
// the UI say nothing.
func TestClassifyWriteProbeError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"the server refused the credential", transport.ErrAuthorizationFailed, writeDenied},
		{"the server demanded one we did not have", transport.ErrAuthenticationRequired, writeDenied},
		{"the credential does not fit the transport", transport.ErrInvalidAuthMethod, writeDenied},
		// An empty remote accepted the credential and then had nothing to list.
		{"an empty remote is a yes", transport.ErrEmptyRemoteRepository, writeOK},
		// The three that used to be reported as a refusal.
		{"our own timeout", context.DeadlineExceeded, writeUnknown},
		{"our own cancellation", context.Canceled, writeUnknown},
		{"the connection broke", errors.New("read tcp 10.0.0.1:22: connection reset by peer"), writeUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := classifyWriteProbeError(c.err)
			require.Equal(t, c.want, got)
		})
	}
}

// The write probe is a SEPARATE question from "what shape is this remote", and
// only the wizard's access step asks it. The create paths read Empty and
// nothing else, so they must not pay for — or be exposed to — a receive-pack
// handshake: it is the one that dials without a context bound (go-git builds
// the SSH session eagerly), so it can block for the OS TCP timeout on a
// filtered port, uncancellable, in the middle of a create.
func TestProbeOrigin_WriteProbeIsOptional(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemoteNoOntology(t, filepath.Join(dir, "remote.git"))
	m := newRemoteModeManager(t, dir)

	withWrite, err := m.ProbeOrigin(context.Background(), OriginSpec{URL: url, Branch: "main"})
	require.NoError(t, err)
	require.NotEmpty(t, withWrite.WriteAccess, "the wizard's probe still establishes push access")

	refsOnly, err := m.ProbeOriginRefs(context.Background(), OriginSpec{URL: url, Branch: "main"})
	require.NoError(t, err)
	require.True(t, refsOnly.Reachable)
	require.Equal(t, withWrite.Branches, refsOnly.Branches)
	require.Empty(t, refsOnly.WriteAccess,
		"a refs-only probe must not CLAIM anything about push access, in either direction")
}
