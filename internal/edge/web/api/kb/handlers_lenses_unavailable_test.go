package kb

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
)

// The role is decided once, from the RESOLVED instance: bare and a kb:// path
// naming the write repo are the write mount; a kb:// path naming another mount
// is a read mount. answersNotFound then hides an unreachable store behind the
// 404 on a read mount only, and a missing fact on either.
func TestLensFactMount_RoleDecidesTheUnavailableAnswer(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	bind := repos.NewBindingForTest(alpha,
		repos.ReadTarget{RI: alpha, Branch: alpha.ReadBranch()},
		repos.ReadTarget{RI: beta, Branch: beta.ReadBranch()})

	for _, tc := range []struct {
		name      string
		id        string
		qualified bool
		want      *repos.RepoInstance
		readMount bool
	}{
		{"bare", "", false, alpha, false},
		{"kb:// naming the write repo", federate.ID12(alpha.ID()), true, alpha, false},
		{"kb:// naming a read mount", federate.ID12(beta.ID()), true, beta, true},
	} {
		mount, ok := resolveLensFactMount(bind, tc.id, tc.qualified)
		require.True(t, ok, tc.name)
		require.Same(t, tc.want, mount.ri, tc.name)
		require.Equal(t, tc.want.ReadBranch(), mount.branch, tc.name)
		require.Equal(t, tc.readMount, mount.readMount, tc.name)
	}
	_, ok := resolveLensFactMount(bind, "0123456789ab", true)
	require.False(t, ok, "an unmounted id")

	read, write := lensFactMount{readMount: true}, lensFactMount{}
	for _, err := range []error{repos.ErrStoreUnavailable, repos.ErrRepoClosed} {
		require.True(t, read.answersNotFound(err), "read mount hides %v", err)
		require.False(t, write.answersNotFound(err), "write mount answers %v with the 503", err)
	}
	require.True(t, read.answersNotFound(errFactNotFound))
	require.True(t, write.answersNotFound(errFactNotFound))
	require.False(t, read.answersNotFound(errors.New("disk on fire")), "a real error is not a 404")
}
