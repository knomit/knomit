package repos

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// These exercise ResolveLensBinding directly. The HTTP rendering of each
// failure kind is tested in internal/web (TestLensMiddleware_*), which is
// where the status/title mapping lives.

func TestResolveLensBinding_SetsBinding(t *testing.T) {
	m := newLifecycleManager(t)
	core := createRepo(t, m, testRepoName)
	_, err := m.LensRegistry().Create(Lens{Name: "eng", WriteUID: core.UID()})
	require.NoError(t, err)

	ctx, err := ResolveLensBinding(context.Background(), m, "eng")
	require.NoError(t, err)

	b, ok := BindingFromContextOpt(ctx)
	require.True(t, ok)
	require.Equal(t, "eng", b.Name())
	require.Same(t, m.Get(testRepoName), b.Write())

	// The write repo is also exposed as the context RepoInstance so
	// repo-based paths (AfterInitialize instructions) work on the lens
	// endpoint until Phase 5 makes them binding-aware.
	ri, ok := RepoFromContextOpt(ctx)
	require.True(t, ok)
	require.Same(t, m.Get(testRepoName), ri)
}

func TestResolveLensBinding_UnknownLens(t *testing.T) {
	m := newLifecycleManager(t)

	_, err := ResolveLensBinding(context.Background(), m, "nope")
	var re *LensResolveError
	require.True(t, errors.As(err, &re))
	require.Equal(t, LensNotFound, re.Kind)
}

func TestResolveLensBinding_UnavailableMember(t *testing.T) {
	m := newLifecycleManager(t)
	// Registered but never opened: a real row, no live instance.
	ghost := seedMember(t, m.Repos(), "ghost")
	_, err := m.LensRegistry().Create(Lens{Name: "broken", WriteUID: ghost})
	require.NoError(t, err)

	_, err = ResolveLensBinding(context.Background(), m, "broken")
	var re *LensResolveError
	require.True(t, errors.As(err, &re))
	require.Equal(t, LensUnavailable, re.Kind)
	// Named, not spelled as a uid — see TestNewBindingOfLens_UnavailableMemberFailsLoudly.
	require.Contains(t, err.Error(), `"ghost"`)
	require.NotContains(t, err.Error(), ghost)
}
