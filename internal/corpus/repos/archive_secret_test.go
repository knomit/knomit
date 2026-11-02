package repos

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos/control"
)

func TestArchive_UnlinksSecret(t *testing.T) {
	f := newSecretsFixture(t)
	ri, id := f.linkedRepo(t)
	uid := ri.UID()
	before, err := f.m.Origins().Get(uid)
	require.NoError(t, err)
	require.ErrorIs(t, f.m.Secrets().Delete(id), control.ErrSecretInUse)
	meta, err := f.m.Secrets().Get(id)
	require.NoError(t, err)
	require.Equal(t, 1, meta.LinkedRepos)

	_, err = f.m.Archive(testRepoName)
	require.NoError(t, err)
	o, err := f.m.Origins().Get(uid)
	require.NoError(t, err)
	require.NotNil(t, o)
	require.Empty(t, o.SecretID)
	require.Equal(t, before.URL, o.URL, "the URL is kept")
	require.Equal(t, before.Branch, o.Branch, "the branch is kept")
	require.Equal(t, before.Mode, o.Mode)
	require.Equal(t, "none", o.AuthMethod)
	meta, err = f.m.Secrets().Get(id)
	require.NoError(t, err)
	require.Zero(t, meta.LinkedRepos, "the secret's linked count drops")
	require.NoError(t, f.m.Secrets().Delete(id), "an archived repo never blocks deleting its secret")
}

// archivedLinked is a linked repo, archived. It returns its uid and the
// secret's id.
func (f *secretsFixture) archivedLinked(t *testing.T) (string, string) {
	t.Helper()
	ri, id := f.linkedRepo(t)
	uid := ri.UID()
	_, err := f.m.Archive(testRepoName)
	require.NoError(t, err)
	return uid, id
}

func TestRestore_RequiresCredentialChoice(t *testing.T) {
	f := newSecretsFixture(t)
	uid, id := f.archivedLinked(t)
	_, err := f.m.RestoreWith(uid, "", &RestoreCredential{})
	require.ErrorIs(t, err, ErrCredentialChoiceRequired)
	_, err = f.m.RestoreWith(uid, "", &RestoreCredential{SecretID: id, None: true})
	require.ErrorIs(t, err, ErrCredentialChoiceRequired)
	_, err = f.m.RestoreWith(uid, "", &RestoreCredential{AuthMethod: "token", AuthToken: "x", None: true})
	require.ErrorIs(t, err, ErrCredentialChoiceRequired)
	require.Nil(t, f.m.GetByUID(uid), "still archived")
}

func TestRestore_WithSecretID(t *testing.T) {
	f := newSecretsFixture(t)
	uid, id := f.archivedLinked(t)

	off, err := f.m.Secrets().Create(control.SecretInput{Name: "gl-pat", Type: control.SecretTypeToken,
		Hosts: []string{"gitlab.com"}, Value: control.SecretValue{Token: "t"}}, time.Now())
	require.NoError(t, err)
	_, err = f.m.RestoreWith(uid, "", &RestoreCredential{SecretID: off.ID})
	require.ErrorIs(t, err, control.ErrSecretHostNotAllowed)
	require.Nil(t, f.m.GetByUID(uid), "a refused restore leaves the repo archived")

	ri, err := f.m.RestoreWith(uid, "", &RestoreCredential{SecretID: id})
	require.NoError(t, err)
	o, err := f.m.Origins().Get(uid)
	require.NoError(t, err)
	require.Equal(t, id, o.SecretID)
	require.Equal(t, id, injected(t, ri).SecretID)
	require.NoError(t, ri.OriginRoundForTest(context.Background()))
	require.Equal(t, "ok", derefStr(injected(t, ri).LastStatus), "the restored repo syncs with the secret")
}

func TestRestore_WithPastedCredential(t *testing.T) {
	f := newSecretsFixture(t)
	uid, _ := f.archivedLinked(t)
	_, err := f.m.RestoreWith(uid, "", &RestoreCredential{AuthMethod: "token"})
	require.ErrorIs(t, err, ErrInvalidCredential)
	ri, err := f.m.RestoreWith(uid, "", &RestoreCredential{AuthMethod: "token", AuthToken: linkedToken})
	require.NoError(t, err)
	o, err := f.m.Origins().Get(uid)
	require.NoError(t, err)
	require.Empty(t, o.SecretID)
	require.Equal(t, linkedToken, o.AuthToken)
	require.NoError(t, ri.OriginRoundForTest(context.Background()))
	require.Equal(t, "ok", derefStr(injected(t, ri).LastStatus))
}

func TestRestore_ExplicitNone(t *testing.T) {
	f := newSecretsFixture(t)
	uid, _ := f.archivedLinked(t)
	ri, err := f.m.RestoreWith(uid, "", &RestoreCredential{None: true})
	require.NoError(t, err)
	o, err := f.m.Origins().Get(uid)
	require.NoError(t, err)
	require.Empty(t, o.SecretID)
	require.Equal(t, "none", o.AuthMethod)
	require.Empty(t, o.AuthToken)
	require.Equal(t, "none", injected(t, ri).AuthMethod)
}

func TestRestore_NoOriginNeedsNoChoice(t *testing.T) {
	f := newSecretsFixture(t)
	ri, err := f.m.Create(context.Background(), CreateSpec{Name: "local", Mode: "preset"}, nil)
	require.NoError(t, err)
	uid := ri.UID()
	_, err = f.m.Archive("local")
	require.NoError(t, err)
	id := f.tokenSecret(t, "gh-pat", linkedToken)
	_, err = f.m.RestoreWith(uid, "", &RestoreCredential{SecretID: id})
	require.ErrorIs(t, err, ErrNoOriginForCredential)
	_, err = f.m.RestoreWith(uid, "", &RestoreCredential{})
	require.NoError(t, err)
}
