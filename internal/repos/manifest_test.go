package repos

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A write to a torn-down instance must report the failure. WithRead does not
// invoke fn when no store is reachable, so an implementation that only captures
// errors set inside the closure returns nil — reporting success for a write
// that never happened.
func TestWriteKBManifest_ClosedInstance_ReportsError(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)

	ri.shutdown()

	committed, err := ri.WriteKBManifest(context.Background(), "# after close")
	require.ErrorIs(t, err, ErrRepoClosed)
	require.False(t, committed)
}

// The cap is enforced in the domain, so every writer of kb.md is bound by it —
// not only the HTTP handler.
func TestWriteKBManifest_EnforcesCap(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)

	_, err := ri.WriteKBManifest(context.Background(), strings.Repeat("x", MaxRepoDescriptionBytes+1))
	require.ErrorIs(t, err, ErrRepoDescriptionTooLong)

	// The rejected write must not have landed.
	got, err := ri.ReadKBManifest(context.Background())
	require.NoError(t, err)
	require.NotContains(t, got, strings.Repeat("x", 64))

	committed, err := ri.WriteKBManifest(context.Background(), strings.Repeat("x", MaxRepoDescriptionBytes))
	require.NoError(t, err)
	require.True(t, committed, "an at-cap description is accepted")
}

// Round-trip plus the unchanged-content skip: re-writing identical bytes
// reports committed=false and leaves the content in place.
func TestWriteKBManifest_RoundTripsAndSkipsUnchanged(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)
	ctx := context.Background()

	const md = "# Core\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"
	committed, err := ri.WriteKBManifest(ctx, md)
	require.NoError(t, err)
	require.True(t, committed)

	got, err := ri.ReadKBManifest(ctx)
	require.NoError(t, err)
	require.Equal(t, md, got, "stored byte-for-byte")

	committed, err = ri.WriteKBManifest(ctx, md)
	require.NoError(t, err)
	require.False(t, committed, "identical content must not produce a commit")

	got, err = ri.ReadKBManifest(ctx)
	require.NoError(t, err)
	require.Equal(t, md, got, "a skipped write leaves the manifest intact")
}
