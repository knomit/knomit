package repos

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos/control"
	"knomit/internal/engine/store/model"
)

// seedMember registers a repo and returns its uid — the key lens membership
// speaks. The uid is deliberately spelled UNLIKE the name so a test that hands
// a name to something expecting a uid fails loudly instead of passing by luck.
func seedMember(t *testing.T, r *control.Registry, name string) string {
	t.Helper()
	uid := "uid-" + name
	require.NoError(t, r.Insert(control.RepoRecord{
		UID: uid, Name: name, State: control.StateActive, Profile: control.ProfileCode, CreatedAt: 1,
	}))
	return uid
}

func openTestOrigins(t *testing.T, crypt *model.Crypt) (*control.Registry, *control.Origins) {
	t.Helper()
	r, err := control.OpenRegistry(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	o := control.OpenOrigins(r.DB(), crypt)
	return r, o
}

func testCrypt(t *testing.T) *model.Crypt {
	t.Helper()
	c, err := model.NewCrypt([]byte("test-key-material-not-a-real-ssh-key"))
	require.NoError(t, err)
	return c
}
