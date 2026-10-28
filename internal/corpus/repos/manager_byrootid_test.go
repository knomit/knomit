package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// ByRootID addresses a repo by its ROOT-COMMIT id (what kb://<id12>/… names),
// never by uid or name, and refuses a 12-hex prefix two repos share rather
// than pick one.
func TestManager_ByRootID(t *testing.T) {
	m := New(context.Background(), Deps{})
	withID := func(name, uid, id string) *RepoInstance {
		ri := NewTestInstanceWithDeps(TestInstanceConfig{Name: name, UID: uid, AgentBranch: "agent/test"})
		ri.id = id
		m.Set(name, ri)
		return ri
	}
	a := withID("a", "uid-a", "aaaaaaaaaaaa1111111111111111111111111111")
	b1 := withID("b1", "uid-b1", "bbbbbbbbbbbb1111111111111111111111111111")
	b2 := withID("b2", "uid-b2", "bbbbbbbbbbbb2222222222222222222222222222")

	got, ok := m.ByRootID("aaaaaaaaaaaa")
	require.True(t, ok)
	require.Same(t, a, got, "the 12-hex wire prefix")
	got, ok = m.ByRootID("aaaaaaaaaaaa1111111111111111111111111111")
	require.True(t, ok)
	require.Same(t, a, got, "the full root-commit id")

	_, ok = m.ByRootID("bbbbbbbbbbbb")
	require.False(t, ok, "a prefix two repos share is ambiguous: not available")
	got, ok = m.ByRootID("bbbbbbbbbbbb2222222222222222222222222222")
	require.True(t, ok)
	require.Same(t, b2, got, "the full id still disambiguates")
	_ = b1

	for _, miss := range []string{"", "cccccccccccc", "uid-a", "a"} {
		_, ok = m.ByRootID(miss)
		require.False(t, ok, "%q is not a root-commit id of any repo", miss)
	}
}
