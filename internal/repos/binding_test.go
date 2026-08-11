package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewBindingOfRepo(t *testing.T) {
	ri := NewTestInstanceWithDeps(TestInstanceConfig{Name: "solo", AgentBranch: "agent/test"})

	b := NewBindingOfRepo(ri, "agent/test")
	require.Same(t, ri, b.Write())
	require.True(t, b.WriteOK(), "agent branch is writable")
	require.Equal(t, "solo", b.Name())
	require.Len(t, b.Reads(), 1)
	require.Same(t, ri, b.Reads()[0].RI)
	require.Equal(t, "agent/test", b.Reads()[0].Branch)

	ro := NewBindingOfRepo(ri, "main")
	require.False(t, ro.WriteOK(), "non-writable branch yields a read-only view")
	require.Equal(t, "main", ro.Reads()[0].Branch)
}

// TestBinding_PinID_FailsClosedOnEmptyUID pins pinOf's fail-closed contract: a
// uid-less RepoInstance (TestInstanceConfig.UID left at its zero value, as
// most test fixtures across the codebase do) must never produce a bare
// "repo:"/"lens:" prefix as its PinID — that would let two DIFFERENT
// uid-less bindings collide on a shared, non-empty pin. Two independently
// built, uid-less bindings over two DIFFERENT repos end up with neither
// having a real PinID to share: both come back empty. (What then keeps an
// empty PinID from letting a minted session resume under a different
// uid-less binding is the resume-side guard in internal/mcp/query.go and
// explain.go — sess.Binding == "" is rejected outright — which is exercised
// at the MCP layer, not here.)
func TestBinding_PinID_FailsClosedOnEmptyUID(t *testing.T) {
	riA := NewTestInstanceWithDeps(TestInstanceConfig{Name: "alpha", AgentBranch: "agent/test"})
	riB := NewTestInstanceWithDeps(TestInstanceConfig{Name: "beta", AgentBranch: "agent/test"})
	require.Empty(t, riA.UID(), "fixture must be uid-less for this test to mean anything")
	require.Empty(t, riB.UID())

	bA := NewBindingOfRepo(riA, "")
	bB := NewBindingOfRepo(riB, "")

	require.NotEqual(t, bA.Name(), bB.Name(), "sanity: these are genuinely different bindings")
	require.Empty(t, bA.PinID(), "an empty uid must never produce a bare \"repo:\" prefix")
	require.Empty(t, bB.PinID(), "same for a second, independently uid-less binding")

	// NewBindingForTest and NewBindingOfLens share the same pinOf helper, so
	// the fail-closed contract holds for every constructor, not just
	// NewBindingOfRepo.
	require.Empty(t, NewBindingForTest(riA).PinID())
}

func TestBinding_IsLens(t *testing.T) {
	ri := NewTestInstanceWithDeps(TestInstanceConfig{Name: "solo"})
	require.False(t, NewBindingOfRepo(ri, "").IsLens(), "lens-of-one is not a lens")

	w := NewTestInstanceWithDeps(TestInstanceConfig{Name: "w"})
	r := NewTestInstanceWithDeps(TestInstanceConfig{Name: "r"})
	lens := NewBindingForTest(w, ReadTarget{RI: w, Branch: w.AgentBranch()}, ReadTarget{RI: r, Branch: r.AgentBranch()})
	require.True(t, lens.IsLens(), "write + distinct read mount is a lens")
}

func TestBinding_WriteMountBranch(t *testing.T) {
	// Lens-of-one bound to an explicit branch: WriteMountBranch is that branch.
	ri := NewTestInstanceWithDeps(TestInstanceConfig{Name: "solo", AgentBranch: "agent/test"})
	b := NewBindingOfRepo(ri, "main")
	require.Equal(t, "main", b.WriteMountBranch())

	// Lens-of-one with default branch: agent branch.
	b2 := NewBindingOfRepo(ri, "")
	require.Equal(t, ri.AgentBranch(), b2.WriteMountBranch())
}

func TestNewBindingOfLens_ResolvesMembersAndDefaultsBranches(t *testing.T) {
	m := newLifecycleManager(t)
	core := createRepo(t, m, testRepoName)
	work, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)

	lens, err := m.LensRegistry().Create(Lens{
		Name:     "eng",
		WriteUID: work.UID(),
		Reads:    []LensRead{{RepoUID: core.UID(), Branch: "", Source: "core-src"}},
	})
	require.NoError(t, err)

	b, err := NewBindingOfLens(m, lens)
	require.NoError(t, err)
	require.Equal(t, "eng", b.Name())
	require.Same(t, m.Get("work"), b.Write())
	require.True(t, b.WriteOK(), "lens writes go to the write repo's agent branch")
	require.Len(t, b.Reads(), 2) // core + work, sorted by repo uid from normalize

	for _, rt := range b.Reads() {
		require.NotNil(t, rt.RI)
		require.Equal(t, rt.RI.AgentBranch(), rt.Branch, "empty branch defaults to the member's agent branch")
	}

	// Source survives resolution.
	var coreSource string
	for _, rt := range b.Reads() {
		if rt.RI.Name() == testRepoName {
			coreSource = rt.Source
		}
	}
	require.Equal(t, "core-src", coreSource)

	// ByID routes to the right mount.
	id := m.Get("work").ID()
	rt, ok := b.ByID(id)
	require.True(t, ok)
	require.Same(t, m.Get("work"), rt.RI)
	_, ok = b.ByID("0000000000000000000000000000000000000000")
	require.False(t, ok)
}

// A lens member that is REGISTERED but has no live instance (its .db is
// missing or failed to open) must fail resolution loudly rather than silently
// shrinking the read set. The uid keying makes this the only shape a dangling
// member can take: the foreign key refuses a member with no registry row at all.
func TestNewBindingOfLens_UnavailableMemberFailsLoudly(t *testing.T) {
	m := newLifecycleManager(t)
	ghost := seedMember(t, m.Repos(), "ghost") // registry row only; never opened
	lens, err := m.LensRegistry().Create(Lens{Name: "broken", WriteUID: ghost})
	require.NoError(t, err)
	require.Nil(t, m.GetByUID(ghost), "the member must have no live instance")

	_, err = NewBindingOfLens(m, lens)
	require.Error(t, err)
	require.Contains(t, err.Error(), `"broken"`)
	// The member is named, not spelled as its registry uid. Membership is
	// uid-keyed, but a bare ksuid names nothing the reader has ever been shown;
	// the registry row survives the repo being unopenable, so the name is there
	// to be looked up.
	require.Contains(t, err.Error(), `"ghost"`)
	require.NotContains(t, err.Error(), ghost, "the raw uid must not be what the operator is handed")
}

func TestBinding_ByID(t *testing.T) {
	m := newLifecycleManager(t)
	core := createRepo(t, m, testRepoName)
	id := core.ID()
	require.Len(t, id, 40)

	b := NewBindingOfRepo(core, "")

	// Full-hash match.
	rt, ok := b.ByID(id)
	require.True(t, ok)
	require.Same(t, core, rt.RI)

	// 12-hex wire prefix match.
	rt, ok = b.ByID(id[:12])
	require.True(t, ok)
	require.Same(t, core, rt.RI)

	// 11- and 13-char inputs do NOT match.
	_, ok = b.ByID(id[:11])
	require.False(t, ok, "11-char prefix must not match")
	_, ok = b.ByID(id[:13])
	require.False(t, ok, "13-char prefix must not match")

	// Empty does not match.
	_, ok = b.ByID("")
	require.False(t, ok)
}

func TestBindingFromContext_SynthesizesLensOfOne(t *testing.T) {
	ri := NewTestInstanceWithDeps(TestInstanceConfig{Name: "solo", AgentBranch: "agent/test"})

	// Explicit binding wins.
	explicit := NewBindingOfRepo(ri, "agent/test")
	got := BindingFromContext(WithBinding(context.Background(), explicit))
	require.Same(t, explicit, got)

	// RepoInstance + bound branch → synthesized lens-of-one on that branch.
	ctx := WithBranch(WithRepoInstance(context.Background(), ri), "main")
	syn := BindingFromContext(ctx)
	require.Same(t, ri, syn.Write())
	require.False(t, syn.WriteOK())
	require.Equal(t, "main", syn.Reads()[0].Branch)

	// RepoInstance alone → agent branch (keeps every pre-binding test working).
	syn = BindingFromContext(WithRepoInstance(context.Background(), ri))
	require.Equal(t, "agent/test", syn.Reads()[0].Branch)
	require.True(t, syn.WriteOK())

	// Neither binding nor repo: programming error.
	require.Panics(t, func() { BindingFromContext(context.Background()) })
}
