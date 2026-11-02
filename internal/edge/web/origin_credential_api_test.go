package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/testsupport/gitfixture"
)

// credFixture: repo x attached (by an operator token, the real PUT /origin)
// to an empty local bare remote, with a token credential.
func credFixture(t *testing.T) (*escFixture, *repos.RepoInstance, string) {
	t.Helper()
	f := newEscFixture(t)
	riX := f.createX(t)
	bare := filepath.Join(f.root, "backup.git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	gitfixture.InitBare(t, bare, "main")
	tok := f.mintOperatorToken(t, "x")
	rec := escDo(f.s.OAuthHandler(), tok, http.MethodPut, "/api/v1/repos/x/origin",
		`{"url":"`+bare+`","branch":"main","auth_method":"token","token":"tok-old"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return f, riX, tok
}

func TestOriginCredentialAPI_OperatorGate(t *testing.T) {
	f, riX, _ := credFixture(t)
	tok := f.mintRepoToken(t, "x") // read+write, no operator
	rec := escDo(f.s.OAuthHandler(), tok, http.MethodPut, "/api/v1/repos/x/origin/credential",
		`{"auth_method":"token","token":"tok-new"}`)
	escProblem(t, rec, http.StatusForbidden, "Permission denied")
	o, err := f.m.Origins().Get(riX.UID())
	require.NoError(t, err)
	require.Equal(t, "tok-old", o.AuthToken, "nothing changed")
}

func TestOriginCredentialAPI_ReplacesToken(t *testing.T) {
	f, riX, tok := credFixture(t)
	h := f.s.OAuthHandler()
	before, err := f.m.Origins().Get(riX.UID())
	require.NoError(t, err)

	rec := escDo(h, tok, http.MethodPut, "/api/v1/repos/x/origin/credential", `{"auth_method":"token","token":"tok-new"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "ok", body["status"])
	require.NotContains(t, rec.Body.String(), "tok-new")

	o, err := f.m.Origins().Get(riX.UID())
	require.NoError(t, err)
	require.Equal(t, control.Origin{URL: before.URL, Branch: before.Branch, AuthMethod: "token", AuthToken: "tok-new", Mode: before.Mode}, *o)

	rec = escDo(h, tok, http.MethodGet, "/api/v1/repos/x/origin", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "tok-new", "GET /origin never shows the token")
	var view map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.Equal(t, "token", view["auth_method"])

	// basic from user and password.
	rec = escDo(h, tok, http.MethodPut, "/api/v1/repos/x/origin/credential", `{"auth_method":"basic","user":"bob","password":"pw"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	o, err = f.m.Origins().Get(riX.UID())
	require.NoError(t, err)
	require.Equal(t, "bob:pw", o.AuthToken)
}

func TestOriginCredentialAPI_Refusals(t *testing.T) {
	f, riX, tok := credFixture(t)
	h := f.s.OAuthHandler()
	put := func(body string) int {
		return escDo(h, tok, http.MethodPut, "/api/v1/repos/x/origin/credential", body).Code
	}
	require.Equal(t, http.StatusBadRequest, put(`{"auth_method":"token","secret_id":"abc"}`))
	require.Equal(t, http.StatusBadRequest, put(`{"auth_method":"token"}`))
	require.Equal(t, http.StatusBadRequest, put(`{"auth_method":"cert"}`))

	// A linked repo: 409 naming the unlink command.
	meta, err := f.m.Secrets().Create(control.SecretInput{Name: "pat", Type: control.SecretTypeToken,
		Hosts: []string{"github.com"}, Value: control.SecretValue{Token: "s"}}, time.Now().UTC())
	require.NoError(t, err)
	o, err := f.m.Origins().Get(riX.UID())
	require.NoError(t, err)
	o.SecretID, o.AuthToken = meta.ID, ""
	require.NoError(t, f.m.Origins().Set(riX.UID(), *o))
	rec := escDo(h, tok, http.MethodPut, "/api/v1/repos/x/origin/credential", `{"auth_method":"token","token":"t"}`)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "secret_id")

	// No origin.
	require.NoError(t, f.m.Origins().Delete(riX.UID()))
	rec = escDo(h, tok, http.MethodPut, "/api/v1/repos/x/origin/credential", `{"auth_method":"token","token":"t"}`)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "No origin")
}
