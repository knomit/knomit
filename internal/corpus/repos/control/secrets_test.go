package control

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"knomit/internal/engine/store/model"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func openTestSecrets(t *testing.T, crypt *model.Crypt) (*Registry, *Origins, *Secrets) {
	t.Helper()
	r, o := openTestOrigins(t, crypt)
	return r, o, OpenSecrets(r.DB(), crypt)
}

func otherCrypt(t *testing.T) *model.Crypt {
	t.Helper()
	c, err := model.NewCrypt([]byte("a-different-instance-key"))
	require.NoError(t, err)
	return c
}

func ed25519OpenSSH(t *testing.T, passphrase string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var blk *pem.Block
	if passphrase == "" {
		blk, err = ssh.MarshalPrivateKey(priv, "test")
	} else {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte(passphrase))
	}
	require.NoError(t, err)
	return string(pem.EncodeToMemory(blk))
}

func rsaPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

var testNow = time.Unix(1_800_000_000, 0).UTC()

func TestSecrets_CreateRevealRoundTrip(t *testing.T) {
	r, _, s := openTestSecrets(t, testCrypt(t))
	key := ed25519OpenSSH(t, "pp-hunter2")
	cases := []SecretInput{
		{Name: "tok", Type: SecretTypeToken, Hosts: []string{"GitHub.com"}, Value: SecretValue{Token: "ghp_ROUNDTRIP"}},
		{Name: "bas", Type: SecretTypeBasic, Hosts: []string{"gitlab.com"}, Value: SecretValue{Username: "bob", Password: "pw:with:colons"}},
		{Name: "key", Type: SecretTypeSSHKey, Hosts: []string{"github.com:2222"}, Value: SecretValue{PrivateKey: key, Passphrase: "pp-hunter2"}},
	}
	for _, in := range cases {
		meta, err := s.Create(in, testNow)
		require.NoError(t, err, in.Name)
		require.NotEmpty(t, meta.ID)
		require.Equal(t, in.Name, meta.Name)

		got, val, err := s.Reveal(meta.ID)
		require.NoError(t, err)
		require.Equal(t, meta.ID, got.ID)
		require.Equal(t, in.Value, val, "%s round-trips", in.Name)

		var enc, pp string
		require.NoError(t, r.DB().QueryRow(`SELECT value_enc, passphrase_enc FROM secrets WHERE id = ?`, meta.ID).Scan(&enc, &pp))
		for _, plain := range []string{in.Value.Token, in.Value.Password, in.Value.Passphrase} {
			if plain != "" {
				require.NotContains(t, enc+pp, plain, "stored at rest in the clear")
			}
		}
		require.NotContains(t, enc, "PRIVATE KEY")
	}
	m, err := s.List()
	require.NoError(t, err)
	require.Len(t, m, 3)
	require.Equal(t, []string{"bas", "key", "tok"}, []string{m[0].Name, m[1].Name, m[2].Name}, "ordered by name")
	require.Equal(t, []string{"github.com"}, m[2].Hosts, "hosts are lowercased")
	for _, x := range m {
		if x.Type == SecretTypeSSHKey {
			require.True(t, strings.HasPrefix(x.Fingerprint, "SHA256:"))
		} else {
			require.Empty(t, x.Fingerprint)
		}
	}
}

func TestSecrets_UndecryptableIsAnError(t *testing.T) {
	r, _, s := openTestSecrets(t, testCrypt(t))
	meta, err := s.Create(SecretInput{Name: "tok", Type: SecretTypeToken, Hosts: []string{"github.com"}, Value: SecretValue{Token: "t"}}, testNow)
	require.NoError(t, err)

	other := OpenSecrets(r.DB(), otherCrypt(t))
	_, _, err = other.Reveal(meta.ID)
	require.ErrorIs(t, err, ErrSecretUndecryptable)
	require.Contains(t, err.Error(), `"tok"`, "names the secret")

	list, err := other.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	_, err = other.Get(meta.ID)
	require.NoError(t, err)
}

func TestSecrets_NilCryptRefuses(t *testing.T) {
	_, _, s := openTestSecrets(t, nil)
	_, err := s.Create(SecretInput{Name: "tok", Type: SecretTypeToken, Hosts: []string{"github.com"}, Value: SecretValue{Token: "t"}}, testNow)
	require.ErrorIs(t, err, ErrEncryptionUnavailable)
	_, _, err = s.Reveal("nope")
	require.ErrorIs(t, err, ErrEncryptionUnavailable)
	require.ErrorIs(t, s.ReplaceValue("nope", SecretValue{Token: "x"}, testNow), ErrEncryptionUnavailable)
}

func TestSecrets_Validation(t *testing.T) {
	_, _, s := openTestSecrets(t, testCrypt(t))
	_, err := s.Create(SecretInput{Name: "taken", Type: SecretTypeToken, Hosts: []string{"github.com"}, Value: SecretValue{Token: "t"}}, testNow)
	require.NoError(t, err)

	encKey := ed25519OpenSSH(t, "right")
	tok := func(name string, hosts ...string) SecretInput {
		return SecretInput{Name: name, Type: SecretTypeToken, Hosts: hosts, Value: SecretValue{Token: "t"}}
	}
	bad := []struct {
		name string
		in   SecretInput
		want error
	}{
		{"empty name", tok("  ", "github.com"), ErrSecretInvalid},
		{"long name", tok(strings.Repeat("n", 129), "github.com"), ErrSecretInvalid},
		{"duplicate name", tok("taken", "github.com"), ErrSecretNameTaken},
		{"no hosts", tok("a"), ErrSecretInvalid},
		{"url as host", tok("a", "https://github.com/x"), ErrSecretInvalid},
		{"userinfo host", tok("a", "git@github.com"), ErrSecretInvalid},
		{"bad type", SecretInput{Name: "a", Type: "oauth", Hosts: []string{"github.com"}, Value: SecretValue{Token: "t"}}, ErrSecretInvalid},
		{"empty token", SecretInput{Name: "a", Type: SecretTypeToken, Hosts: []string{"github.com"}}, ErrSecretInvalid},
		{"basic colon user", SecretInput{Name: "a", Type: SecretTypeBasic, Hosts: []string{"github.com"}, Value: SecretValue{Username: "a:b", Password: "p"}}, ErrSecretInvalid},
		{"basic no password", SecretInput{Name: "a", Type: SecretTypeBasic, Hosts: []string{"github.com"}, Value: SecretValue{Username: "a"}}, ErrSecretInvalid},
		{"garbage key", SecretInput{Name: "a", Type: SecretTypeSSHKey, Hosts: []string{"github.com"}, Value: SecretValue{PrivateKey: "not a key"}}, ErrSecretInvalid},
		{"wrong passphrase", SecretInput{Name: "a", Type: SecretTypeSSHKey, Hosts: []string{"github.com"}, Value: SecretValue{PrivateKey: encKey, Passphrase: "wrong"}}, ErrSecretInvalid},
		{"missing passphrase", SecretInput{Name: "a", Type: SecretTypeSSHKey, Hosts: []string{"github.com"}, Value: SecretValue{PrivateKey: encKey}}, ErrSecretInvalid},
	}
	for _, c := range bad {
		_, err := s.Create(c.in, testNow)
		require.ErrorIs(t, err, c.want, c.name)
	}

	for i, k := range []string{ed25519OpenSSH(t, ""), rsaPEM(t)} {
		meta, err := s.Create(SecretInput{Name: fmt.Sprintf("k%d", i), Type: SecretTypeSSHKey, Hosts: []string{"github.com"},
			Value: SecretValue{PrivateKey: k}}, testNow)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(meta.Fingerprint, "SHA256:"), meta.Fingerprint)
	}
}

// linkRepo inserts an active repo with an origin at url, linked to secretID.
func linkRepo(t *testing.T, r *Registry, o *Origins, uid, name, url, secretID string) {
	t.Helper()
	require.NoError(t, r.Insert(RepoRecord{UID: uid, Name: name, State: StateActive, Profile: "code", CreatedAt: 1}))
	require.NoError(t, o.Set(uid, Origin{URL: url, Branch: "main", AuthMethod: "token"}))
	_, err := r.DB().Exec(`INSERT INTO repo_origin_secrets (repo_uid, secret_id) VALUES (?, ?)`, uid, secretID)
	require.NoError(t, err)
}

func TestSecrets_DeleteWhileLinkedIsRefused(t *testing.T) {
	r, o, s := openTestSecrets(t, testCrypt(t))
	meta, err := s.Create(SecretInput{Name: "tok", Type: SecretTypeToken, Hosts: []string{"github.com"}, Value: SecretValue{Token: "t"}}, testNow)
	require.NoError(t, err)
	linkRepo(t, r, o, "u1", "alpha", "https://github.com/a/b.git", meta.ID)

	got, err := s.Get(meta.ID)
	require.NoError(t, err)
	require.Equal(t, 1, got.LinkedRepos)

	err = s.Delete(meta.ID)
	require.ErrorIs(t, err, ErrSecretInUse)
	var inUse *InUseError
	require.True(t, errors.As(err, &inUse))
	require.Len(t, inUse.Repos, 1)
	require.Equal(t, "alpha", inUse.Repos[0].Name)
	require.Equal(t, "u1", inUse.Repos[0].UID)

	_, err = r.DB().Exec(`DELETE FROM repo_origin_secrets WHERE repo_uid = 'u1'`)
	require.NoError(t, err)
	require.NoError(t, s.Delete(meta.ID))
	_, err = s.Get(meta.ID)
	require.ErrorIs(t, err, ErrSecretNotFound)
	require.ErrorIs(t, s.Delete(meta.ID), ErrSecretNotFound)
}

func TestSecrets_SetHosts(t *testing.T) {
	r, o, s := openTestSecrets(t, testCrypt(t))
	meta, err := s.Create(SecretInput{Name: "tok", Type: SecretTypeToken, Hosts: []string{"github.com"}, Value: SecretValue{Token: "t"}}, testNow)
	require.NoError(t, err)
	linkRepo(t, r, o, "u1", "alpha", "https://GitHub.com/a/b.git", meta.ID)

	require.NoError(t, s.SetHosts(meta.ID, []string{"github.com", "GitLab.com", "gitlab.com"}, testNow))
	got, err := s.Get(meta.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"github.com", "gitlab.com"}, got.Hosts)

	require.NoError(t, s.SetHosts(meta.ID, []string{"github.com"}, testNow), "gitlab.com is unused")

	err = s.SetHosts(meta.ID, []string{"gitlab.com"}, testNow)
	require.ErrorIs(t, err, ErrSecretHostInUse)
	var hu *HostInUseError
	require.True(t, errors.As(err, &hu))
	require.Equal(t, []string{"github.com"}, hu.Hosts)
	require.Len(t, hu.Repos, 1)
	require.Equal(t, "alpha", hu.Repos[0].Name)
	got, err = s.Get(meta.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"github.com"}, got.Hosts, "unchanged after a refusal")

	require.ErrorIs(t, s.SetHosts(meta.ID, nil, testNow), ErrSecretInvalid)
	require.ErrorIs(t, s.SetHosts(meta.ID, []string{"github.com", "http://x/y"}, testNow), ErrSecretInvalid)
	require.ErrorIs(t, s.SetHosts("missing", []string{"github.com"}, testNow), ErrSecretNotFound)
}

func TestSecrets_RenameExpiryReplace(t *testing.T) {
	_, _, s := openTestSecrets(t, testCrypt(t))
	meta, err := s.Create(SecretInput{Name: "tok", Type: SecretTypeToken, Hosts: []string{"github.com"}, Value: SecretValue{Token: "old"}}, testNow)
	require.NoError(t, err)
	_, err = s.Create(SecretInput{Name: "other", Type: SecretTypeToken, Hosts: []string{"github.com"}, Value: SecretValue{Token: "x"}}, testNow)
	require.NoError(t, err)

	later := testNow.Add(time.Hour)
	require.NoError(t, s.Rename(meta.ID, "renamed", later))
	require.ErrorIs(t, s.Rename(meta.ID, "other", later), ErrSecretNameTaken)
	exp := testNow.Add(30 * 24 * time.Hour)
	require.NoError(t, s.SetExpiry(meta.ID, &exp, later))
	got, err := s.Get(meta.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name)
	require.True(t, got.ExpiresAt.Equal(exp))
	require.True(t, got.UpdatedAt.Equal(later))
	require.True(t, got.CreatedAt.Equal(testNow))
	require.NoError(t, s.SetExpiry(meta.ID, nil, later))
	got, err = s.Get(meta.ID)
	require.NoError(t, err)
	require.Nil(t, got.ExpiresAt)

	require.ErrorIs(t, s.ReplaceValue(meta.ID, SecretValue{Username: "u", Password: "p"}, later), ErrSecretInvalid,
		"validated against the stored type")
	require.NoError(t, s.ReplaceValue(meta.ID, SecretValue{Token: "new"}, later))
	_, v, err := s.Reveal(meta.ID)
	require.NoError(t, err)
	require.Equal(t, "new", v.Token)
	require.ErrorIs(t, s.ReplaceValue("missing", SecretValue{Token: "x"}, later), ErrSecretNotFound)
}

func TestRemoteHost(t *testing.T) {
	ok := map[string]string{
		"https://GitHub.com/a/b.git":    "github.com",
		"http://example.test:8080/x":    "example.test:8080",
		"ssh://git@host:2222/x":         "host:2222",
		"ssh://git@Host/x":              "host",
		"git@host:x":                    "host",
		"git@GitHub.com:owner/repo.git": "github.com",
	}
	for in, want := range ok {
		got, err := RemoteHost(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, in := range []string{"file:///x", "/srv/repo.git", "", "relative/path"} {
		_, err := RemoteHost(in)
		require.Error(t, err, in)
	}
}

func TestHostAllowed(t *testing.T) {
	require.True(t, HostAllowed([]string{"github.com"}, "github.com"))
	require.True(t, HostAllowed([]string{"github.com"}, "github.com:2222"), "a bare host allows every port")
	require.True(t, HostAllowed([]string{"github.com:2222"}, "github.com:2222"))
	require.False(t, HostAllowed([]string{"github.com:2222"}, "github.com"))
	require.False(t, HostAllowed([]string{"github.com"}, "gitlab.com"))
}

func TestExpiryStatus_Boundaries(t *testing.T) {
	at := func(d time.Duration) *time.Time { x := testNow.Add(d); return &x }
	require.Equal(t, "none", ExpiryStatus(nil, testNow))
	require.Equal(t, "ok", ExpiryStatus(at(ExpiringWithin+time.Second), testNow))
	require.Equal(t, "expiring", ExpiryStatus(at(ExpiringWithin), testNow))
	require.Equal(t, "expiring", ExpiryStatus(at(time.Second), testNow))
	require.Equal(t, "expired", ExpiryStatus(at(0), testNow))
	require.Equal(t, "expired", ExpiryStatus(at(-time.Second), testNow))
}

func TestSecretValue_NeverPrints(t *testing.T) {
	v := SecretValue{Token: "TOK-xyz", Username: "USER-xyz", Password: "PASS-xyz", PrivateKey: "KEY-xyz", Passphrase: "PHRASE-xyz"}
	in := SecretInput{Name: "n", Type: SecretTypeToken, Value: v}
	out := fmt.Sprintf("%v %+v %#v %s %v %+v %#v", v, v, v, v, in, in, &in)
	js, err := json.Marshal(v)
	require.NoError(t, err)
	jsIn, err := json.Marshal(in)
	require.NoError(t, err)
	out += string(js) + string(jsIn)
	for _, s := range []string{"TOK-xyz", "USER-xyz", "PASS-xyz", "KEY-xyz", "PHRASE-xyz"} {
		require.NotContains(t, out, s)
	}
	require.Contains(t, out, "[redacted]")
}
