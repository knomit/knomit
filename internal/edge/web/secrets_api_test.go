package web

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki/pkitest"
	"knomit/internal/testsupport/testsigner"
)

// secretsFixture is a Server over a STARTED Manager (so control.db and the
// Secrets tenant exist), with the OAuth fixture's grants and issuer for the
// gate cases.
type secretsFixture struct {
	*oauthWebFixture
	m *repos.Manager
}

func newSecretsFixture(t *testing.T, withKey bool) *secretsFixture {
	t.Helper()
	f := newKBFixture(t)
	home := t.TempDir()
	keyPath := filepath.Join(home, "missing-key")
	if withKey {
		keyPath = filepath.Join(home, "id_ed25519")
		buf := make([]byte, 64)
		_, _ = rand.Read(buf)
		require.NoError(t, os.WriteFile(keyPath, buf, 0o600))
	}
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: home, OntologyRoot: "kb"},
		AgentID: testsigner.FixtureAgentID("agent/test"), AgentBranch: "agent/test",
		KeyPath: keyPath,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	f.s.Manager = m
	return &secretsFixture{oauthWebFixture: f, m: m}
}

func (f *secretsFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	return serve(f.s.Handler(), localPeer(req))
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return out
}

func testSSHKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	blk, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "k", []byte("PHRASE-zz9"))
	require.NoError(t, err)
	return string(pem.EncodeToMemory(blk))
}

// linkSecret gives repo name (an E2E mount, so it has a registry row) an
// origin at url linked to secretID.
func (f *secretsFixture) linkSecret(t *testing.T, name, url, secretID string) *repos.RepoInstance {
	t.Helper()
	newE2EMount(t, f.m, name, false)
	require.NoError(t, f.m.Origins().Set("uid-"+name, control.Origin{URL: url, Branch: "main", AuthMethod: "token", SecretID: secretID}))
	return f.m.Get(name)
}

func TestSecretsAPI_CRUD(t *testing.T) {
	f := newSecretsFixture(t, true)

	rec := f.do(t, "POST", "/api/v1/secrets", `{"name":"gh","type":"token","hosts":["GitHub.com"],"value":"ghp_crud","expires_at":"2030-01-02T03:04:05Z"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	v := decodeMap(t, rec)
	id := v["id"].(string)
	require.Equal(t, "gh", v["name"])
	require.Equal(t, "token", v["type"])
	require.Equal(t, []any{"github.com"}, v["hosts"])
	require.Equal(t, "2030-01-02T03:04:05Z", v["expires_at"])
	require.Equal(t, "ok", v["status"])
	require.EqualValues(t, 0, v["linked_repos"])
	require.Equal(t, "/api/v1/secrets/"+id, v["_links"].(map[string]any)["self"].(map[string]any)["href"])
	require.NotContains(t, v, "fingerprint")
	require.Equal(t, "/api/v1/secrets/"+id, rec.Header().Get("Location"))

	rec = f.do(t, "GET", "/api/v1/secrets", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	list := decodeMap(t, rec)["secrets"].([]any)
	require.Len(t, list, 1)

	rec = f.do(t, "GET", "/api/v1/secrets/"+id, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []any{}, decodeMap(t, rec)["repos"])

	rec = f.do(t, "PATCH", "/api/v1/secrets/"+id, `{"name":"github-pat","expires_at":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	v = decodeMap(t, rec)
	require.Equal(t, "github-pat", v["name"])
	require.Nil(t, v["expires_at"])
	require.Equal(t, "none", v["status"])

	soon := time.Now().UTC().Add(3 * 24 * time.Hour).Format(time.RFC3339)
	rec = f.do(t, "PATCH", "/api/v1/secrets/"+id, `{"expires_at":"`+soon+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "expiring", decodeMap(t, rec)["status"])
	require.Equal(t, "github-pat", decodeMap(t, rec)["name"], "absent name is kept")

	rec = f.do(t, "PATCH", "/api/v1/secrets/"+id, `{"value":"ghp_new"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, val, err := f.m.Secrets().Reveal(id)
	require.NoError(t, err)
	require.Equal(t, "ghp_new", val.Token)

	require.Equal(t, http.StatusNotFound, f.do(t, "GET", "/api/v1/secrets/nope", "").Code)
	require.Equal(t, http.StatusNotFound, f.do(t, "PATCH", "/api/v1/secrets/nope", `{"name":"x"}`).Code)
	require.Equal(t, http.StatusNotFound, f.do(t, "DELETE", "/api/v1/secrets/nope", "").Code)

	require.Equal(t, http.StatusNoContent, f.do(t, "DELETE", "/api/v1/secrets/"+id, "").Code)
	require.Equal(t, http.StatusNotFound, f.do(t, "GET", "/api/v1/secrets/"+id, "").Code)
}

func TestSecretsAPI_Errors(t *testing.T) {
	f := newSecretsFixture(t, true)
	require.Equal(t, http.StatusCreated, f.do(t, "POST", "/api/v1/secrets", `{"name":"a","type":"token","hosts":["github.com"],"value":"x"}`).Code)
	for name, c := range map[string]struct {
		body string
		code int
		want string
	}{
		"malformed":     {`{"name":`, http.StatusBadRequest, ""},
		"no hosts":      {`{"name":"b","type":"token","hosts":[],"value":"x"}`, http.StatusBadRequest, "hosts"},
		"bad type":      {`{"name":"b","type":"oauth","hosts":["github.com"],"value":"x"}`, http.StatusBadRequest, "type"},
		"duplicate":     {`{"name":"a","type":"token","hosts":["github.com"],"value":"x"}`, http.StatusConflict, ""},
		"bad expiry":    {`{"name":"b","type":"token","hosts":["github.com"],"value":"x","expires_at":"tomorrow"}`, http.StatusBadRequest, "expires_at"},
		"field to type": {`{"name":"b","type":"token","hosts":["github.com"],"password":"x"}`, http.StatusBadRequest, ""},
	} {
		rec := f.do(t, "POST", "/api/v1/secrets", c.body)
		require.Equal(t, c.code, rec.Code, "%s: %s", name, rec.Body.String())
		require.Contains(t, rec.Body.String(), c.want, name)
	}
}

// No response body and no log line carries a value, password, key or
// passphrase, whatever the type and whichever endpoint answered.
func TestSecretsAPI_NeverReturnsValue(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&logs).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = prev })

	f := newSecretsFixture(t, true)
	key := testSSHKey(t)
	keyJSON, _ := json.Marshal(key)
	bodies := []string{
		`{"name":"t","type":"token","hosts":["github.com"],"value":"TOKEN-zz9"}`,
		`{"name":"b","type":"basic","hosts":["github.com"],"username":"bob","password":"PASS-zz9"}`,
		`{"name":"k","type":"ssh_key","hosts":["github.com"],"private_key":` + string(keyJSON) + `,"passphrase":"PHRASE-zz9"}`,
	}
	var all strings.Builder
	var ids []string
	for _, b := range bodies {
		rec := f.do(t, "POST", "/api/v1/secrets", b)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		all.WriteString(rec.Body.String())
		ids = append(ids, decodeMap(t, rec)["id"].(string))
	}
	all.WriteString(f.do(t, "GET", "/api/v1/secrets", "").Body.String())
	for _, id := range ids {
		all.WriteString(f.do(t, "GET", "/api/v1/secrets/"+id, "").Body.String())
	}
	all.WriteString(f.do(t, "PATCH", "/api/v1/secrets/"+ids[0], `{"value":"TOKEN2-zz9"}`).Body.String())
	all.WriteString(f.do(t, "PATCH", "/api/v1/secrets/"+ids[1], `{"username":"bob","password":"PASS2-zz9"}`).Body.String())
	all.WriteString(f.do(t, "PATCH", "/api/v1/secrets/"+ids[2], `{"private_key":`+string(keyJSON)+`,"passphrase":"WRONG-zz9"}`).Body.String())

	out := all.String() + logs.String()
	require.Contains(t, out, "SHA256:", "the ssh view shows its fingerprint")
	for _, s := range []string{"TOKEN-zz9", "TOKEN2-zz9", "PASS-zz9", "PASS2-zz9", "PHRASE-zz9", "WRONG-zz9", "PRIVATE KEY", key[40:80]} {
		require.NotContains(t, out, s)
	}
}

func TestSecretsAPI_Gate(t *testing.T) {
	f := newSecretsFixture(t, true)
	create := `{"name":"g","type":"token","hosts":["github.com"],"value":"x"}`

	// The web UI on this listener: anonymous loopback with admin and the proof.
	rec := serve(f.s.Handler(), browserReq(http.MethodPost, "/api/v1/secrets", create, "127.0.0.1"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = serve(f.s.Handler(), browserReq(http.MethodGet, "/api/v1/secrets", "", "127.0.0.1"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The same without the browser header.
	req := browserReq(http.MethodGet, "/api/v1/secrets", "", "127.0.0.1")
	req.Header.Del(KnomitClientHeader)
	require.Equal(t, http.StatusForbidden, serve(f.s.Handler(), req).Code)

	// A bearer token holding admin, on the OAuth listener: the secrets API is
	// not served there at all.
	tok := f.mint(t, "admin-token", testIssuer, []string{"read", "write"}, auth.Read, auth.Write, auth.Admin)
	breq := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	breq.Header.Set("Authorization", "Bearer "+tok)
	code := serve(f.s.OAuthHandler(), breq).Code
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnauthorized}, code)
	// ...and on the plain listener the header is ignored: anonymous loopback
	// without the proof.
	breq = fromLoopback(httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil))
	breq.Header.Set("Authorization", "Bearer "+tok)
	require.Equal(t, http.StatusForbidden, serve(f.s.Handler(), breq).Code)

	// An operator certificate holding admin.
	pk := pkitest.New(t)
	op := pk.Enroll(t, "op@example.com")
	opP := auth.OperatorPrincipal(op.Fingerprint())
	for _, perm := range []auth.Permission{auth.Read, auth.Write, auth.Admin} {
		require.NoError(t, f.grants.Grant(context.Background(), opP, perm, "test"))
	}
	rec = serve(asTLSPeer(f.s.Handler(), op.Cert), httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A read-only server refuses writes.
	f.s.ReadOnly = true
	rec = f.do(t, "POST", "/api/v1/secrets", `{"name":"ro","type":"token","hosts":["github.com"],"value":"x"}`)
	require.NotEqual(t, http.StatusCreated, rec.Code)
	list, err := f.m.Secrets().List()
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestSecretsAPI_PatchHosts(t *testing.T) {
	f := newSecretsFixture(t, true)
	rec := f.do(t, "POST", "/api/v1/secrets", `{"name":"h","type":"token","hosts":["github.com"],"value":"x"}`)
	require.Equal(t, http.StatusCreated, rec.Code)
	id := decodeMap(t, rec)["id"].(string)
	f.linkSecret(t, "alpha", "https://github.com/a/b.git", id)

	rec = f.do(t, "PATCH", "/api/v1/secrets/"+id, `{"hosts":["github.com","gitlab.com"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []any{"github.com", "gitlab.com"}, decodeMap(t, rec)["hosts"])
	rec = f.do(t, "PATCH", "/api/v1/secrets/"+id, `{"hosts":["github.com"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = f.do(t, "PATCH", "/api/v1/secrets/"+id, `{"hosts":["gitlab.com"]}`)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	p := decodeMap(t, rec)
	require.Contains(t, p["detail"], "alpha")
	require.Equal(t, []any{"github.com"}, p["hosts"])
	require.Equal(t, "alpha", p["repos"].([]any)[0].(map[string]any)["name"])
	got, err := f.m.Secrets().Get(id)
	require.NoError(t, err)
	require.Equal(t, []string{"github.com"}, got.Hosts)

	require.Equal(t, http.StatusBadRequest, f.do(t, "PATCH", "/api/v1/secrets/"+id, `{"hosts":[]}`).Code)

	req := browserReq(http.MethodPatch, "/api/v1/secrets/"+id, `{"hosts":["github.com","x.com"]}`, "127.0.0.1")
	req.Header.Del(KnomitClientHeader)
	require.Equal(t, http.StatusForbidden, serve(f.s.Handler(), req).Code)
}

func TestSecretsAPI_DeleteWhileLinked409(t *testing.T) {
	f := newSecretsFixture(t, true)
	rec := f.do(t, "POST", "/api/v1/secrets", `{"name":"d","type":"token","hosts":["github.com"],"value":"x"}`)
	id := decodeMap(t, rec)["id"].(string)
	f.linkSecret(t, "alpha", "https://github.com/a/b.git", id)

	rec = f.do(t, "GET", "/api/v1/secrets/"+id, "")
	v := decodeMap(t, rec)
	require.EqualValues(t, 1, v["linked_repos"])
	require.Equal(t, "alpha", v["repos"].([]any)[0].(map[string]any)["name"])

	rec = f.do(t, "DELETE", "/api/v1/secrets/"+id, "")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	p := decodeMap(t, rec)
	require.Equal(t, "Unlink these repositories first: alpha", p["detail"])
	require.Len(t, p["repos"], 1)
	_, err := f.m.Secrets().Get(id)
	require.NoError(t, err, "still there")
}

func TestSecretsAPI_NoCrypt503(t *testing.T) {
	f := newSecretsFixture(t, false)
	rec := f.do(t, "POST", "/api/v1/secrets", `{"name":"n","type":"token","hosts":["github.com"],"value":"x"}`)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "Secret storage is unavailable: the instance key cannot be read.")
	require.Equal(t, http.StatusOK, f.do(t, "GET", "/api/v1/secrets", "").Code, "metadata reads still work")
}

// Replacing a value asks every ACTIVE linked repo's loop to reset its
// breakers and wakes it (RepoInstance.credentialChanged).
func TestSecretsAPI_ReplaceValueWakesLinked(t *testing.T) {
	f := newSecretsFixture(t, true)
	rec := f.do(t, "POST", "/api/v1/secrets", `{"name":"w","type":"token","hosts":["github.com"],"value":"x"}`)
	id := decodeMap(t, rec)["id"].(string)
	ri := f.linkSecret(t, "alpha", "https://github.com/a/b.git", id)
	before := ri.BreakerResetGenForTest()

	rec = f.do(t, "PATCH", "/api/v1/secrets/"+id, `{"name":"renamed"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, before, ri.BreakerResetGenForTest(), "a rename is not a credential change")

	rec = f.do(t, "PATCH", "/api/v1/secrets/"+id, `{"value":"y"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, before+1, ri.BreakerResetGenForTest())
}
