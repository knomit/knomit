package repos

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/corpus/repos/control"
)

// pastedRepo clones the fixture's remote with a pasted token (no secret).
func (f *secretsFixture) pastedRepo(t *testing.T) *RepoInstance {
	t.Helper()
	ri, err := f.m.Create(context.Background(), CreateSpec{Name: testRepoName, Mode: "clone",
		Origin: &OriginSpec{URL: f.url, Branch: "main", AuthMethod: "token", AuthToken: linkedToken}}, nil)
	require.NoError(t, err)
	return ri
}

func link(m *Manager, ri *RepoInstance, id string) error {
	return m.ChangeOriginCredential(context.Background(), ri, CredentialChange{SecretID: id}, false)
}

func TestLink_StoresTheLinkAndClearsTheToken(t *testing.T) {
	f := newSecretsFixture(t)
	ri := f.pastedRepo(t)
	id := f.tokenSecret(t, "gh-pat", linkedToken)
	require.NoError(t, link(f.m, ri, id))
	o, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Equal(t, id, o.SecretID)
	require.Equal(t, "token", o.AuthMethod)
	require.Empty(t, o.AuthToken, "linking clears the pasted token")
	mem := injected(t, ri)
	require.Equal(t, id, mem.SecretID)
	require.Empty(t, mem.AuthToken)
	require.NoError(t, ri.OriginRoundForTest(context.Background()))
	require.Equal(t, "ok", derefStr(injected(t, ri).LastStatus))
}

func TestLink_TypeSchemeMismatchRefused(t *testing.T) {
	f := newSecretsFixture(t)
	ri := f.pastedRepo(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	blk, err := ssh.MarshalPrivateKey(priv, "test")
	require.NoError(t, err)
	meta, err := f.m.Secrets().Create(control.SecretInput{Name: "deploy-key", Type: control.SecretTypeSSHKey,
		Hosts: []string{"127.0.0.1"}, Value: control.SecretValue{PrivateKey: string(pem.EncodeToMemory(blk))}}, time.Now())
	require.NoError(t, err)
	require.ErrorIs(t, link(f.m, ri, meta.ID), ErrSecretTypeMismatch)
	o, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Empty(t, o.SecretID)
	require.Equal(t, linkedToken, o.AuthToken, "nothing changed")
}

func TestLink_HostNotOnListRefused(t *testing.T) {
	f := newSecretsFixture(t)
	ri := f.pastedRepo(t)
	meta, err := f.m.Secrets().Create(control.SecretInput{Name: "gh-pat", Type: control.SecretTypeToken,
		Hosts: []string{"github.com"}, Value: control.SecretValue{Token: "t"}}, time.Now())
	require.NoError(t, err)
	err = link(f.m, ri, meta.ID)
	require.ErrorIs(t, err, control.ErrSecretHostNotAllowed)
	require.Contains(t, err.Error(), "gh-pat")
	o, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Empty(t, o.SecretID)
}

func TestLink_CertOriginRefused(t *testing.T) {
	f := newSecretsFixture(t)
	ri := f.pastedRepo(t)
	id := f.tokenSecret(t, "gh-pat", linkedToken)
	o, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	o.AuthMethod, o.AuthToken = "cert", ""
	require.NoError(t, f.m.Origins().Set(ri.UID(), *o))
	require.ErrorIs(t, link(f.m, ri, id), ErrPeerCredential)
}

func TestLink_ResetsBreakersAndWakes(t *testing.T) {
	f := newSecretsFixture(t)
	ri := f.pastedRepo(t)
	id := f.tokenSecret(t, "gh-pat", linkedToken)
	gen := ri.BreakerResetGenForTest()
	require.NoError(t, link(f.m, ri, id))
	require.Equal(t, gen+1, ri.BreakerResetGenForTest())
	require.NoError(t, f.m.ChangeOriginCredential(context.Background(), ri, CredentialChange{}, true))
	require.Equal(t, gen+2, ri.BreakerResetGenForTest(), "an unlink is a credential change too")
}

func TestUnlink_LeavesPastedCredentialAsGiven(t *testing.T) {
	f := newSecretsFixture(t)
	ri, _ := f.linkedRepo(t)
	require.ErrorIs(t, f.m.ChangeOriginCredential(context.Background(), ri,
		CredentialChange{AuthMethod: "token", AuthToken: "pasted"}, false), ErrLinkedRepo)
	require.NoError(t, f.m.ChangeOriginCredential(context.Background(), ri,
		CredentialChange{AuthMethod: "token", AuthToken: linkedToken}, true))
	o, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Empty(t, o.SecretID)
	require.Equal(t, "token", o.AuthMethod)
	require.Equal(t, linkedToken, o.AuthToken)
	mem := injected(t, ri)
	require.Empty(t, mem.SecretID)
	require.Equal(t, linkedToken, mem.AuthToken)
}

func TestUnlink_ExplicitNoneLeavesAnonymous(t *testing.T) {
	f := newSecretsFixture(t)
	ri, id := f.linkedRepo(t)
	require.NoError(t, f.m.ChangeOriginCredential(context.Background(), ri, CredentialChange{}, true))
	o, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Empty(t, o.SecretID)
	require.Equal(t, "none", o.AuthMethod)
	require.Empty(t, o.AuthToken)
	require.NoError(t, f.m.Secrets().Delete(id), "the secret is no longer linked")
}

// Replacing a linked secret's value reaches the next round with no restart:
// the server accepts only the new value, and the round succeeds.
func TestReplaceValue_NextRoundUsesNewValueNoRestart(t *testing.T) {
	f := newSecretsFixture(t)
	ri, id := f.linkedRepo(t)
	f.srv.Fault().RequireBasicAuth("x-token", "tok-rotated")
	require.NoError(t, ri.OriginRoundForTest(context.Background()))
	require.Equal(t, "error", derefStr(injected(t, ri).LastStatus), "the old value is refused")

	logs := captureLogs(t, zerolog.DebugLevel)
	gen := ri.BreakerResetGenForTest()
	require.NoError(t, f.m.ReplaceSecretValue(id, control.SecretValue{Token: "tok-rotated"}))
	require.Equal(t, gen+1, ri.BreakerResetGenForTest(), "the linked repo was signalled")
	require.NoError(t, ri.OriginRoundForTest(context.Background()))
	require.Equal(t, "ok", derefStr(injected(t, ri).LastStatus))
	require.Equal(t, 1, countLines(logs, "reconcile loop started"), "only this test's own round started a loop: nothing restarted")
	require.NotContains(t, logs.String(), "tok-rotated")
}

// A link that races an attach re-pointing the origin is refused by the
// URL compare-and-set, as a pasted change is.
func TestLink_CASRefusal(t *testing.T) {
	f := newSecretsFixture(t)
	ri := f.pastedRepo(t)
	id := f.tokenSecret(t, "gh-pat", linkedToken)
	before, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	moved := *before
	moved.URL = before.URL + "-moved"
	beforeCredentialWrite = func() { require.NoError(t, f.m.Origins().Set(ri.UID(), moved)) }
	t.Cleanup(func() { beforeCredentialWrite = nil })
	require.ErrorIs(t, link(f.m, ri, id), control.ErrOriginChanged)
	got, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Empty(t, got.SecretID)
}
