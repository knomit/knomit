package repos

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store"
)

// Every path that rebuilds a repo's origin keeps its secret link, durably
// (control.db) and in memory (the injected origin the loop resolves). A
// dropped link would sync anonymously or with a stale token.

func requireLinked(t *testing.T, f *secretsFixture, ri *RepoInstance, id string) {
	t.Helper()
	o, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.NotNil(t, o)
	require.Equal(t, id, o.SecretID, "control.db keeps the link")
	require.Empty(t, o.AuthToken)
	require.Equal(t, id, injected(t, ri).SecretID, "memory keeps the link")
}

func TestCarry_CreateCloneWithSecret(t *testing.T) {
	f := newSecretsFixture(t)
	ri, id := f.linkedRepo(t)
	requireLinked(t, f, ri, id)
}

func TestCarry_OpenStageReinjects(t *testing.T) {
	f := newSecretsFixture(t)
	ri, id := f.linkedRepo(t)
	uid := ri.UID()
	f.reboot(t)
	ri = f.m.GetByUID(uid)
	require.NotNil(t, ri)
	requireLinked(t, f, ri, id)
	require.NoError(t, ri.OriginRoundForTest(context.Background()))
	require.Equal(t, "ok", derefStr(injected(t, ri).LastStatus), "the reopened repo syncs with the secret")
}

func TestCarry_AttachOrigin(t *testing.T) {
	f := newSecretsFixture(t)
	ri, id := f.linkedRepo(t)
	_, err := f.m.Send(context.Background(), ri, AttachOrigin(OriginSpec{URL: f.url, Branch: "main", AuthMethod: "token", SecretID: id}))
	require.NoError(t, err)
	requireLinked(t, f, ri, id)
}

func TestCarry_SwapStore(t *testing.T) {
	f := newSecretsFixture(t)
	ri, id := f.linkedRepo(t)
	require.NoError(t, ri.WithRead(func(s *store.Service) { require.NoError(t, s.Checkpoint()) }))
	tmp := filepath.Join(t.TempDir(), "copy.db")
	copyDB(t, f.m.RepoPath(ri.UID()), tmp)
	_, err := f.m.Send(context.Background(), ri, SwapStore(SwapSpec{TempDB: tmp,
		Origin: OriginSpec{URL: f.url, Branch: "main", AuthMethod: "token", SecretID: id}}))
	require.NoError(t, err)
	requireLinked(t, f, ri, id)
}

// A failed fleet move restores the previous origin with restoreFleetOrigin;
// a linked previous origin comes back linked.
func TestCarry_RestoreFleetOrigin(t *testing.T) {
	f := newSecretsFixture(t)
	ri, id := f.linkedRepo(t)
	prev, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	_, err = f.m.Send(context.Background(), ri, DetachOrigin())
	require.NoError(t, err)
	require.NoError(t, f.m.restoreFleetOrigin(context.Background(), ri, prev))
	requireLinked(t, f, ri, id)
}

// An attach that re-points a linked repo at a host the secret does not
// allow is refused by the link's own host check: nothing changes.
func TestCarry_AttachToAnotherHostIsRefused(t *testing.T) {
	f := newSecretsFixture(t)
	ri, id := f.linkedRepo(t)
	other := "http://localhost" + f.url[len("http://127.0.0.1"):]
	_, err := f.m.Send(context.Background(), ri, AttachOrigin(OriginSpec{URL: other, Branch: "main", AuthMethod: "token", SecretID: id}))
	require.Error(t, err)
	requireLinked(t, f, ri, id)
	require.Equal(t, f.url, injected(t, ri).URL)
}
