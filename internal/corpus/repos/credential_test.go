package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos/control"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/remotesync"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
)

// newCredentialRepo is a started Manager WITH an instance key (so tokens can
// be stored) holding one repo cloned from a local bare remote.
func newCredentialRepo(t *testing.T) (*Manager, *RepoInstance) {
	t.Helper()
	dir := t.TempDir()
	url := seedBareRemote(t, filepath.Join(dir, "remote.git"))
	home := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	key := filepath.Join(home, "agent.key")
	require.NoError(t, os.WriteFile(key, []byte("credential-test-instance-key"), 0o600))
	m := New(context.Background(), Deps{
		Cfg:     config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: dir},
		AgentID: testsigner.FixtureAgentID(trigAgent), AgentBranch: trigAgent,
		KeyPath: key,
		Machine: Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	ri, err := m.Create(context.Background(), CreateSpec{Name: testRepoName, Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main"}}, nil)
	require.NoError(t, err)
	return m, ri
}

func injected(t *testing.T, ri *RepoInstance) *remotesync.Remote {
	t.Helper()
	var r *remotesync.Remote
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		var err error
		r, err = svc.Remote().GetRemote("origin")
		require.NoError(t, err)
	}))
	require.NotNil(t, r)
	return r
}

// The light swap changes the credential durably and in memory, signals a
// credential change, and runs NO lifecycle: the Sync stage is not
// re-entered (its loop would log "reconcile loop started" again).
func TestChangeOriginCredential_NoLifecycle(t *testing.T) {
	m, ri := newCredentialRepo(t)
	before, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	// A subscribe-mode row: the swap must carry Mode untouched.
	sub := *before
	sub.Mode = control.OriginModeSubscribe
	require.NoError(t, m.Origins().Set(ri.UID(), sub))

	logs := captureLogs(t, zerolog.DebugLevel)
	gen := ri.BreakerResetGenForTest()
	require.NoError(t, m.ChangeOriginCredential(context.Background(), ri,
		CredentialChange{AuthMethod: "token", AuthToken: "tok-new"}, false))

	after, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Equal(t, control.Origin{URL: before.URL, Branch: before.Branch, AuthMethod: "token", AuthToken: "tok-new",
		Mode: control.OriginModeSubscribe}, *after)
	mem := injected(t, ri)
	require.Equal(t, "tok-new", mem.AuthToken)
	require.Equal(t, "token", mem.AuthMethod)
	require.Equal(t, before.URL, mem.URL)
	require.Equal(t, before.Branch, mem.Branch)
	require.Equal(t, gen+1, ri.BreakerResetGenForTest(), "a credential change was signalled")
	require.Zero(t, countLines(logs, "reconcile loop started"), "the Sync stage was not restarted")
	require.NotContains(t, logs.String(), "tok-new")
}

// A URL that changes in control.db between the read and the write (an attach
// landing) refuses the swap: nothing is written and memory is not touched.
func TestChangeOriginCredential_CASRefusal(t *testing.T) {
	m, ri := newCredentialRepo(t)
	before, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	beforeMem := injected(t, ri)

	moved := *before
	moved.URL = before.URL + "-moved"
	beforeCredentialWrite = func() { require.NoError(t, m.Origins().Set(ri.UID(), moved)) }
	t.Cleanup(func() { beforeCredentialWrite = nil })

	gen := ri.BreakerResetGenForTest()
	err = m.ChangeOriginCredential(context.Background(), ri, CredentialChange{AuthMethod: "token", AuthToken: "tok-new"}, false)
	require.ErrorIs(t, err, control.ErrOriginChanged)
	got, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Equal(t, moved, *got, "the attach's row stands")
	require.Equal(t, *beforeMem, *injected(t, ri), "memory untouched")
	require.Equal(t, gen, ri.BreakerResetGenForTest())
}

func TestChangeOriginCredential_CertRefused(t *testing.T) {
	m, ri := newCredentialRepo(t)
	o, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	o.AuthMethod = "cert"
	require.NoError(t, m.Origins().Set(ri.UID(), *o))
	err = m.ChangeOriginCredential(context.Background(), ri, CredentialChange{AuthMethod: "token", AuthToken: "x"}, false)
	require.ErrorIs(t, err, ErrPeerCredential)
}

func TestChangeOriginCredential_Refusals(t *testing.T) {
	m, ri := newCredentialRepo(t)
	ctx := context.Background()

	for name, c := range map[string]CredentialChange{
		"cert as input":      {AuthMethod: "cert"},
		"unknown method":     {AuthMethod: "oauth", AuthToken: "x"},
		"token without one":  {AuthMethod: "token"},
		"token with ssh":     {AuthMethod: "ssh", AuthToken: "x"},
		"basic without user": {AuthMethod: "basic", AuthToken: "nocolon"},
	} {
		err := m.ChangeOriginCredential(ctx, ri, c, false)
		require.Error(t, err, name)
		require.ErrorIs(t, err, ErrInvalidCredential, name)
	}

	// An ssh method on an http URL is a method mismatch.
	o, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	httpOrigin := *o
	httpOrigin.URL = "https://example.test/kb.git"
	require.NoError(t, m.Origins().Set(ri.UID(), httpOrigin))
	err = m.ChangeOriginCredential(ctx, ri, CredentialChange{AuthMethod: "ssh"}, false)
	require.ErrorIs(t, err, ErrInvalidCredential)
	require.Contains(t, err.Error(), "SSH auth cannot be used with HTTP")

	// A linked repo is refused unless the caller unlinks.
	meta, err := m.Secrets().Create(control.SecretInput{Name: "pat", Type: control.SecretTypeToken,
		Hosts: []string{"example.test"}, Value: control.SecretValue{Token: "s"}}, time.Now().UTC())
	require.NoError(t, err)
	linked := httpOrigin
	linked.SecretID = meta.ID
	require.NoError(t, m.Origins().Set(ri.UID(), linked))
	err = m.ChangeOriginCredential(ctx, ri, CredentialChange{AuthMethod: "token", AuthToken: "x"}, false)
	require.ErrorIs(t, err, ErrLinkedRepo)

	// No origin at all.
	require.NoError(t, m.Origins().Delete(ri.UID()))
	err = m.ChangeOriginCredential(ctx, ri, CredentialChange{AuthMethod: "token", AuthToken: "x"}, false)
	require.ErrorIs(t, err, ErrNoOrigin)
}

// An attach that lands between the swap's re-read and its in-memory write
// wins in memory too: the swap must never put the old origin's credential
// back over the attached URL (go-git would send it to the new host).
func TestChangeOriginCredential_AttachAfterRereadWins(t *testing.T) {
	m, ri := newCredentialRepo(t)
	other := seedBareRemote(t, filepath.Join(t.TempDir(), "other.git"))
	afterCredentialReread = func() {
		require.NoError(t, applyAttach(ri, OriginSpec{URL: other, Branch: "main", AuthMethod: "none"}))
	}
	t.Cleanup(func() { afterCredentialReread = nil })
	gen := ri.BreakerResetGenForTest()

	err := m.ChangeOriginCredential(context.Background(), ri, CredentialChange{AuthMethod: "token", AuthToken: "tok-new"}, false)
	require.ErrorIs(t, err, control.ErrOriginChanged)
	mem := injected(t, ri)
	require.Equal(t, other, mem.URL)
	require.Equal(t, "none", mem.AuthMethod)
	require.Empty(t, mem.AuthToken, "the old origin's new token never reached the attached URL")
	row, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Equal(t, other, row.URL)
	require.Empty(t, row.AuthToken)
	require.Equal(t, gen, ri.BreakerResetGenForTest(), "no credential change was signalled")
}

// A detach in the same window: the swap must not resurrect the removed origin
// in memory.
func TestChangeOriginCredential_DetachAfterRereadWins(t *testing.T) {
	m, ri := newCredentialRepo(t)
	afterCredentialReread = func() { require.NoError(t, applyDetach(ri)) }
	t.Cleanup(func() { afterCredentialReread = nil })

	err := m.ChangeOriginCredential(context.Background(), ri, CredentialChange{AuthMethod: "token", AuthToken: "tok-new"}, false)
	require.ErrorIs(t, err, control.ErrOriginChanged)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		r, gerr := svc.Remote().GetRemote("origin")
		require.NoError(t, gerr)
		require.Nil(t, r, "the detached origin stays gone in memory")
	}))
	row, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Nil(t, row)
}

func TestChangeOriginCredential_BasicNeedsPassword(t *testing.T) {
	m, ri := newCredentialRepo(t)
	err := m.ChangeOriginCredential(context.Background(), ri, CredentialChange{AuthMethod: "basic", AuthToken: "bob:"}, false)
	require.ErrorIs(t, err, ErrInvalidCredential)
	o, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.NotEqual(t, "bob:", o.AuthToken)
}

// A re-attach to the SAME URL with another credential, in the same window:
// the URL alone matches, so only a whole-origin compare keeps memory on the
// attach's credential (which control.db holds) instead of the swap's.
func TestChangeOriginCredential_SameURLReattachAfterRereadWins(t *testing.T) {
	m, ri := newCredentialRepo(t)
	before, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	afterCredentialReread = func() {
		require.NoError(t, applyAttach(ri, OriginSpec{URL: before.URL, Branch: before.Branch, AuthMethod: "basic", AuthToken: "att:pw"}))
	}
	t.Cleanup(func() { afterCredentialReread = nil })

	err = m.ChangeOriginCredential(context.Background(), ri, CredentialChange{AuthMethod: "token", AuthToken: "tok-new"}, false)
	require.ErrorIs(t, err, control.ErrOriginChanged)
	mem := injected(t, ri)
	require.Equal(t, "basic", mem.AuthMethod)
	require.Equal(t, "att:pw", mem.AuthToken, "memory matches the attach")
	row, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Equal(t, "att:pw", row.AuthToken, "control.db matches the attach")
}
