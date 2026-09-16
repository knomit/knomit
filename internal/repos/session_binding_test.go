package repos

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePin(t *testing.T) {
	for _, tc := range []struct {
		in, kind, uid string
		bad           bool
	}{
		{in: "repo:abc", kind: "repo", uid: "abc"},
		{in: "lens:abc", kind: "lens", uid: "abc"},
		{in: "repo:", bad: true},
		{in: "abc", bad: true},
		{in: "", bad: true},
		{in: "branch:abc", bad: true},
	} {
		k, u, err := ParsePin(tc.in)
		if tc.bad {
			require.Error(t, err, tc.in)
			continue
		}
		require.NoError(t, err, tc.in)
		require.Equal(t, tc.kind, k)
		require.Equal(t, tc.uid, u)
	}
}

func TestRequireBinding(t *testing.T) {
	// Nothing in context: unbound, and it does NOT panic.
	_, err := RequireBinding(context.Background())
	require.ErrorIs(t, err, ErrUnbound)

	ri := NewTestInstanceWithDeps(TestInstanceConfig{UID: "u1", AgentBranch: "agent/x"})

	// A RepoInstance alone synthesizes the lens-of-one, as BindingFromContext does.
	b, err := RequireBinding(WithRepoInstance(context.Background(), ri))
	require.NoError(t, err)
	require.Same(t, ri, b.Write())

	// An explicit Binding is returned untouched.
	explicit := NewBindingOfRepo(ri, "")
	b, err = RequireBinding(WithBinding(context.Background(), explicit))
	require.NoError(t, err)
	require.Same(t, explicit, b)

	// A stored-binding failure surfaces its own reason, not ErrUnbound.
	boom := errors.New("gone")
	_, err = RequireBinding(WithBindingError(context.Background(), boom))
	require.ErrorIs(t, err, boom)

	// An explicit binding still wins over a recorded error.
	b, err = RequireBinding(WithBinding(WithBindingError(context.Background(), boom), explicit))
	require.NoError(t, err)
	require.Same(t, explicit, b)
}

func TestResolveSessionBinding_RepoBindsAtAgentBranch(t *testing.T) {
	m := newTestManager(t)
	ri := bootNamedRepo(t, m, "core")

	ctx, err := ResolveSessionBinding(context.Background(), m, PinForRepo(ri))
	require.NoError(t, err)

	b, ok := BindingFromContextOpt(ctx)
	require.True(t, ok)
	require.True(t, b.WriteOK(), "a normal repo binds writable")
	require.Equal(t, ri.AgentBranch(), b.Reads()[0].Branch)

	got, ok := RepoFromContextOpt(ctx)
	require.True(t, ok)
	require.Same(t, ri, got, "the write repo is in context too, as on the lens mount")
}

// A subscription binds at the branch it FOLLOWS and refuses writes — with no
// gating logic of its own: NewBindingOfRepo(ri, "") takes the read branch and
// writeOK straight off the instance.
func TestResolveSessionBinding_SubscriptionIsReadOnlyAtUpstream(t *testing.T) {
	root := t.TempDir()
	m := newSubscribeTestManager(t, root)
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url},
	}, nil)
	require.NoError(t, err)
	require.True(t, ri.Subscribed())

	ctx, err := ResolveSessionBinding(context.Background(), m, PinForRepo(ri))
	require.NoError(t, err)

	b, ok := BindingFromContextOpt(ctx)
	require.True(t, ok)
	require.False(t, b.WriteOK(), "a subscription refuses writes")
	require.Equal(t, "main", b.Reads()[0].Branch, "bound at the followed upstream")
}

func TestResolveSessionBinding_Lens(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	write := createRepo(t, m, "writer")
	read := createRepo(t, m, "reader")

	l, err := m.LensRegistry().Create(Lens{
		Name: "eng", WriteUID: write.UID(),
		Reads:     []LensRead{{RepoUID: read.UID()}},
		CreatedAt: 1, UpdatedAt: 1,
	})
	require.NoError(t, err)

	ctx, err := ResolveSessionBinding(context.Background(), m, PinForLens(l))
	require.NoError(t, err)

	b, ok := BindingFromContextOpt(ctx)
	require.True(t, ok)
	require.Same(t, write, b.Write())
	require.Len(t, b.Reads(), 2)
}

func TestResolveSessionBinding_Unresolvable(t *testing.T) {
	m := newTestManager(t)
	bootNamedRepo(t, m, "core")

	var sbe *SessionBindingError

	_, err := ResolveSessionBinding(context.Background(), m, "repo:nope")
	require.ErrorAs(t, err, &sbe)
	require.Equal(t, BindingRepoUnavailable, sbe.Kind)
	require.Contains(t, err.Error(), "knomit_bind")

	_, err = ResolveSessionBinding(context.Background(), m, "garbage")
	require.ErrorAs(t, err, &sbe)
	require.Equal(t, BindingMalformed, sbe.Kind)
	require.Contains(t, err.Error(), "knomit_bind")

	_, err = ResolveSessionBinding(context.Background(), m, "lens:nope")
	require.ErrorAs(t, err, &sbe)
	require.Equal(t, BindingLensUnavailable, sbe.Kind)
	require.Contains(t, err.Error(), "knomit_bind")
}
