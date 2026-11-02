package repos

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/corpus/repos/control"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/model"
	"knomit/internal/engine/store/remotesync"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
	"knomit/test/testenv/gitserver"
)

const linkedToken = "tok-linked-1"

// secretsFixture is a started Manager with an instance key and a smart-HTTP
// remote (127.0.0.1) that accepts only x-token:<token>.
type secretsFixture struct {
	m    *Manager
	deps Deps
	srv  *gitserver.Server
	url  string // the http URL of the seeded remote
	dir  string
}

func newSecretsFixture(t *testing.T) *secretsFixture {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "srv")
	srv := linkedRemote(t, root, "remote.git", seedBareRemote)
	home := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	key := filepath.Join(home, "agent.key")
	require.NoError(t, os.WriteFile(key, []byte("secret-auth-test-instance-key"), 0o600))
	deps := Deps{
		Cfg:     config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: dir},
		AgentID: testsigner.FixtureAgentID(trigAgent), AgentBranch: trigAgent,
		KeyPath: key,
		Machine: Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	}
	m := New(context.Background(), deps)
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	return &secretsFixture{m: m, deps: deps, srv: srv, url: srv.URL + "/remote.git", dir: dir}
}

// reboot closes the Manager and starts a new one on the same home, which
// reopens every repo through the Open stage.
func (f *secretsFixture) reboot(t *testing.T) {
	t.Helper()
	require.NoError(t, f.m.Close())
	f.m = New(context.Background(), f.deps)
	require.NoError(t, f.m.Start())
	m := f.m
	t.Cleanup(func() { _ = m.Close() })
}

// tokenSecret stores a token secret allowed for the fixture's host.
func (f *secretsFixture) tokenSecret(t *testing.T, name, value string) string {
	t.Helper()
	meta, err := f.m.Secrets().Create(control.SecretInput{Name: name, Type: control.SecretTypeToken,
		Hosts: []string{"127.0.0.1"}, Value: control.SecretValue{Token: value}}, time.Now())
	require.NoError(t, err)
	return meta.ID
}

// linkedRepo clones the fixture's remote with a stored token secret.
func (f *secretsFixture) linkedRepo(t *testing.T) (*RepoInstance, string) {
	t.Helper()
	id := f.tokenSecret(t, "gh-pat", linkedToken)
	ri, err := f.m.Create(context.Background(), CreateSpec{Name: testRepoName, Mode: "clone",
		Origin: &OriginSpec{URL: f.url, Branch: "main", AuthMethod: "token", SecretID: id}}, nil)
	require.NoError(t, err)
	return ri, id
}

// linkedRemote seeds root/name with seed and serves root over smart HTTP,
// requiring x-token:linkedToken. Pushes are enabled on the repo.
func linkedRemote(t *testing.T, root, name string, seed func(*testing.T, string) string) *gitserver.Server {
	t.Helper()
	bare := filepath.Join(root, name)
	seed(t, bare)
	runGit(t, bare, "config", "http.receivepack", "true")
	srv := gitserver.New(t, root)
	t.Cleanup(srv.Close)
	srv.Fault().RequireBasicAuth("x-token", linkedToken)
	return srv
}

// accepted counts the requests that carried the accepted credential.
func accepted(srv *gitserver.Server) int {
	n := 0
	for _, a := range srv.Fault().AuthAttempts() {
		if a.OK {
			n++
		}
	}
	return n
}

// refused lists the passwords of the refused requests ("" for none).
func refused(srv *gitserver.Server) []string {
	var out []string
	for _, a := range srv.Fault().AuthAttempts() {
		if !a.OK {
			out = append(out, a.Password)
		}
	}
	return out
}

func newTestSecrets(t *testing.T, crypt *model.Crypt) *control.Secrets {
	t.Helper()
	reg, err := control.OpenRegistry(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { reg.Close() })
	return control.OpenSecrets(reg.DB(), crypt)
}

func testCryptFor(t *testing.T, key string) *model.Crypt {
	t.Helper()
	c, err := model.NewCrypt([]byte(key))
	require.NoError(t, err)
	return c
}

func TestResolveLinkedAuth_TypesBuildTheRightAuth(t *testing.T) {
	s := newTestSecrets(t, testCryptFor(t, "k1"))
	now := time.Now()
	tok, err := s.Create(control.SecretInput{Name: "tok", Type: control.SecretTypeToken, Hosts: []string{"github.com"},
		Value: control.SecretValue{Token: "t-value"}}, now)
	require.NoError(t, err)
	basic, err := s.Create(control.SecretInput{Name: "basic", Type: control.SecretTypeBasic, Hosts: []string{"github.com"},
		Value: control.SecretValue{Username: "alice", Password: "pw-value"}}, now)
	require.NoError(t, err)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	blk, err := ssh.MarshalPrivateKey(priv, "test")
	require.NoError(t, err)
	sshKey, err := s.Create(control.SecretInput{Name: "key", Type: control.SecretTypeSSHKey, Hosts: []string{"github.com"},
		Value: control.SecretValue{PrivateKey: string(pem.EncodeToMemory(blk))}}, now)
	require.NoError(t, err)

	kh := filepath.Join(t.TempDir(), "known_hosts")
	fn := makeRemoteAuthFn(config.RemoteAuthConfig{KnownHosts: kh}, "", func() *control.Secrets { return s })

	a, err := fn(&remotesync.Remote{URL: "https://github.com/a/b.git", AuthMethod: "token", SecretID: tok.ID})
	require.NoError(t, err)
	require.Equal(t, &githttp.BasicAuth{Username: "x-token", Password: "t-value"}, a)

	a, err = fn(&remotesync.Remote{URL: "https://github.com/a/b.git", AuthMethod: "basic", SecretID: basic.ID})
	require.NoError(t, err)
	require.Equal(t, &githttp.BasicAuth{Username: "alice", Password: "pw-value"}, a)

	a, err = fn(&remotesync.Remote{URL: "git@github.com:a/b.git", AuthMethod: "ssh", SecretID: sshKey.ID})
	require.NoError(t, err)
	pk, ok := a.(*gitssh.PublicKeys)
	require.True(t, ok, "%T", a)
	want, err := ssh.NewPublicKey(priv.Public())
	require.NoError(t, err)
	require.Equal(t, want.Marshal(), pk.Signer.PublicKey().Marshal())
}

func TestResolveLinkedAuth_HostNotAllowed(t *testing.T) {
	s := newTestSecrets(t, testCryptFor(t, "k1"))
	meta, err := s.Create(control.SecretInput{Name: "gh-pat", Type: control.SecretTypeToken, Hosts: []string{"github.com"},
		Value: control.SecretValue{Token: "never-sent-value"}}, time.Now())
	require.NoError(t, err)
	_, err = resolveLinkedAuth(s, meta.ID, "https://gitlab.com/a/b.git", "")
	require.ErrorIs(t, err, control.ErrSecretHostNotAllowed)
	require.Contains(t, err.Error(), "gh-pat")
	require.NotContains(t, err.Error(), "never-sent-value")
}

func TestResolveLinkedAuth_TypeMismatch(t *testing.T) {
	s := newTestSecrets(t, testCryptFor(t, "k1"))
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	blk, err := ssh.MarshalPrivateKey(priv, "test")
	require.NoError(t, err)
	meta, err := s.Create(control.SecretInput{Name: "deploy-key", Type: control.SecretTypeSSHKey, Hosts: []string{"github.com"},
		Value: control.SecretValue{PrivateKey: string(pem.EncodeToMemory(blk))}}, time.Now())
	require.NoError(t, err)
	_, err = resolveLinkedAuth(s, meta.ID, "https://github.com/a/b.git", "")
	require.ErrorIs(t, err, ErrSecretTypeMismatch)
	require.Contains(t, err.Error(), "deploy-key")

	tok, err := s.Create(control.SecretInput{Name: "tok", Type: control.SecretTypeToken, Hosts: []string{"github.com"},
		Value: control.SecretValue{Token: "t"}}, time.Now())
	require.NoError(t, err)
	_, err = resolveLinkedAuth(s, tok.ID, "git@github.com:a/b.git", "")
	require.ErrorIs(t, err, ErrSecretTypeMismatch)
}

func TestResolveLinkedAuth_Undecryptable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "control.db")
	reg, err := control.OpenRegistry(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { reg.Close() })
	written := control.OpenSecrets(reg.DB(), testCryptFor(t, "old-key"))
	meta, err := written.Create(control.SecretInput{Name: "gh-pat", Type: control.SecretTypeToken, Hosts: []string{"github.com"},
		Value: control.SecretValue{Token: "t"}}, time.Now())
	require.NoError(t, err)
	read := control.OpenSecrets(reg.DB(), testCryptFor(t, "new-key"))
	_, err = resolveLinkedAuth(read, meta.ID, "https://github.com/a/b.git", "")
	require.ErrorIs(t, err, control.ErrSecretUndecryptable)
	require.Contains(t, err.Error(), "gh-pat")
}

// A linked record never falls back: the [remote] fallback has a token and the
// secret does not decrypt, so the answer is an error and no auth.
func TestMakeRemoteAuthFn_LinkedNeverFallsBack(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "control.db")
	reg, err := control.OpenRegistry(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { reg.Close() })
	written := control.OpenSecrets(reg.DB(), testCryptFor(t, "old-key"))
	meta, err := written.Create(control.SecretInput{Name: "gh-pat", Type: control.SecretTypeToken, Hosts: []string{"github.com"},
		Value: control.SecretValue{Token: "t"}}, time.Now())
	require.NoError(t, err)
	read := control.OpenSecrets(reg.DB(), testCryptFor(t, "new-key"))

	fn := makeRemoteAuthFn(config.RemoteAuthConfig{Token: "fallback-token", AuthMethod: "token"}, "", func() *control.Secrets { return read })
	a, err := fn(&remotesync.Remote{URL: "https://github.com/a/b.git", AuthMethod: "token", AuthToken: "pasted", SecretID: meta.ID})
	require.Error(t, err)
	require.Nil(t, a)

	// No secrets store at all: still an error, never the fallback.
	a, err = makeRemoteAuthFn(config.RemoteAuthConfig{Token: "fallback-token"}, "", nil)(
		&remotesync.Remote{URL: "https://github.com/a/b.git", AuthMethod: "token", SecretID: meta.ID})
	require.Error(t, err)
	require.Nil(t, a)

	// A cert record with a link is refused.
	_, err = fn(&remotesync.Remote{URL: "https://github.com/a/b.git", AuthMethod: "cert", SecretID: meta.ID})
	require.ErrorIs(t, err, ErrSecretOnPeer)
}

func TestMakeRemoteAuthFn_PicksUpReplacedValue(t *testing.T) {
	s := newTestSecrets(t, testCryptFor(t, "k1"))
	meta, err := s.Create(control.SecretInput{Name: "tok", Type: control.SecretTypeToken, Hosts: []string{"github.com"},
		Value: control.SecretValue{Token: "v1"}}, time.Now())
	require.NoError(t, err)
	fn := makeRemoteAuthFn(config.RemoteAuthConfig{}, "", func() *control.Secrets { return s })
	rec := &remotesync.Remote{URL: "https://github.com/a/b.git", AuthMethod: "token", SecretID: meta.ID}
	a, err := fn(rec)
	require.NoError(t, err)
	require.Equal(t, "v1", a.(*githttp.BasicAuth).Password)
	require.NoError(t, s.ReplaceValue(meta.ID, control.SecretValue{Token: "v2"}, time.Now()))
	a, err = fn(rec)
	require.NoError(t, err)
	require.Equal(t, "v2", a.(*githttp.BasicAuth).Password)
}

// A linked repo really syncs with its secret, end to end over HTTP.
func TestLoop_LinkedRepoSyncsWithTheSecret(t *testing.T) {
	f := newSecretsFixture(t)
	ri, id := f.linkedRepo(t)
	stored, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Equal(t, id, stored.SecretID)
	require.Empty(t, stored.AuthToken)
	require.Equal(t, id, injected(t, ri).SecretID)
	require.Positive(t, accepted(f.srv), "the clone authenticated with the secret")
	require.NotContains(t, refused(f.srv), "", "never an anonymous request after the challenge")

	require.NoError(t, ri.OriginRoundForTest(context.Background()))
	require.Equal(t, "ok", derefStr(injected(t, ri).LastStatus))
}

// A linked repo whose secret no longer decrypts fails the round as a sync
// error naming the secret, and no fetch is attempted.
func TestLoop_UndecryptableSecretIsASyncError(t *testing.T) {
	f := newSecretsFixture(t)
	ri, id := f.linkedRepo(t)
	_, err := f.m.Repos().DB().Exec(`UPDATE secrets SET value_enc = 'garbage' WHERE id = ?`, id)
	require.NoError(t, err)

	var attempts []string
	setSyncHooks(t, syncHooks{attempt: func(repo, step string) { attempts = append(attempts, step) }})
	before := accepted(f.srv)
	require.NoError(t, ri.OriginRoundForTest(context.Background()))
	r := injected(t, ri)
	require.Equal(t, "error", derefStr(r.LastStatus))
	require.Contains(t, derefStr(r.LastError), "gh-pat")
	require.Empty(t, attempts, "no fetch or push was attempted")
	require.Equal(t, before, accepted(f.srv), "nothing reached the remote")
}

// A linked repo whose URL is changed (any path) to a host off the list: the
// round fails and the secret is never sent.
func TestLoop_HostOffTheListNeverSendsTheSecret(t *testing.T) {
	f := newSecretsFixture(t)
	ri, _ := f.linkedRepo(t)
	// Point memory at "localhost" (same server, another host name).
	moved := strings.Replace(f.url, "127.0.0.1", "localhost", 1)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		o := svc.Remote().InjectedOrigin()
		o.URL = moved
		svc.Remote().SetOrigin(o)
	}))
	before := accepted(f.srv)
	require.NoError(t, ri.OriginRoundForTest(context.Background()))
	r := injected(t, ri)
	require.Equal(t, "error", derefStr(r.LastStatus))
	require.Contains(t, derefStr(r.LastError), "not allowed for host localhost")
	require.Equal(t, before, accepted(f.srv))
	for _, p := range refused(f.srv) {
		require.NotEqual(t, linkedToken, p)
	}
}

func TestResolveOrigin_PeerWithSecretRefused(t *testing.T) {
	// ResolveOrigin's peer branch needs fleet trust; the refusal condition
	// itself is the spec check, exercised through the cert record path and
	// resolveSpecAuth here.
	m := New(context.Background(), Deps{Cfg: config.Config{Home: t.TempDir()}})
	_, _, err := m.resolveSpecAuth(&OriginSpec{URL: "https://peer.example/kb", AuthMethod: "cert", SecretID: "x"})
	require.ErrorIs(t, err, ErrSecretOnPeer)
}

// authConfigFromSpec is called only inside resolveSpecAuth: every
// spec-carried operation resolves a linked secret.
func TestAuthConfigFromSpecHasOneCaller(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	require.NoError(t, err)
	var callers []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, d := range file.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok {
						if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "authConfigFromSpec" {
							callers = append(callers, fd.Name.Name)
						}
					}
					return true
				})
			}
		}
	}
	require.Equal(t, []string{"resolveSpecAuth"}, callers)
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
