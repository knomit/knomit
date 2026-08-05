package repos

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// The file must land at README.md with its case intact — a git provider looks
// for that exact name, and case-preservation is the whole point of the rename.
func TestWriteReadme_LandsAtExactPath(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(config.DefaultRepoName)
	require.NotNil(t, ri)

	committed, err := ri.WriteReadme(context.Background(), "# my kb")
	require.NoError(t, err)
	require.True(t, committed)

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		paths, lerr := svc.Facts().ListAll(context.Background(), ri.AgentBranch())
		require.NoError(t, lerr)
		require.Contains(t, paths, "README.md")
		require.NotContains(t, paths, "readme.md")
	}))
}

// Clean break: kb.md is not read. A repo that still has one reports no
// description, which is the ordinary "no manifest" state.
func TestReadReadme_IgnoresLegacyKBMd(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(config.DefaultRepoName)
	require.NotNil(t, ri)

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, werr := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(),
			"kb.md", "# legacy manifest", "seed legacy", "update")
		require.NoError(t, werr)
	}))

	got, err := ri.ReadReadme(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)
}

// A write to a torn-down instance must report the failure. WithRead does not
// invoke fn when no store is reachable, so an implementation that only captures
// errors set inside the closure returns nil — reporting success for a write
// that never happened.
func TestWriteReadme_ClosedInstance_ReportsError(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(config.DefaultRepoName)
	require.NotNil(t, ri)

	ri.shutdown()

	committed, err := ri.WriteReadme(context.Background(), "# after close")
	require.ErrorIs(t, err, ErrRepoClosed)
	require.False(t, committed)
}

// The cap is enforced in the domain, so every writer of README.md is bound by
// it — not only the HTTP handler.
func TestWriteReadme_EnforcesCap(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(config.DefaultRepoName)
	require.NotNil(t, ri)

	_, err := ri.WriteReadme(context.Background(), strings.Repeat("x", MaxRepoDescriptionBytes+1))
	require.ErrorIs(t, err, ErrRepoDescriptionTooLong)

	// The rejected write must not have landed.
	got, err := ri.ReadReadme(context.Background())
	require.NoError(t, err)
	require.NotContains(t, got, strings.Repeat("x", 64))

	committed, err := ri.WriteReadme(context.Background(), strings.Repeat("x", MaxRepoDescriptionBytes))
	require.NoError(t, err)
	require.True(t, committed, "an at-cap description is accepted")
}

// Round-trip plus the unchanged-content skip: re-writing identical bytes
// reports committed=false and leaves the content in place.
func TestWriteReadme_RoundTripsAndSkipsUnchanged(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(config.DefaultRepoName)
	require.NotNil(t, ri)
	ctx := context.Background()

	const md = "# Core\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"
	committed, err := ri.WriteReadme(ctx, md)
	require.NoError(t, err)
	require.True(t, committed)

	got, err := ri.ReadReadme(ctx)
	require.NoError(t, err)
	require.Equal(t, md, got, "stored byte-for-byte")

	committed, err = ri.WriteReadme(ctx, md)
	require.NoError(t, err)
	require.False(t, committed, "identical content must not produce a commit")

	got, err = ri.ReadReadme(ctx)
	require.NoError(t, err)
	require.Equal(t, md, got, "a skipped write leaves the manifest intact")
}
