package web

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/foundation/auth"
)

// archivedX archives the fixture's linked repo x and returns its restore path.
func (f *linkedFixture) archivedX(t *testing.T) string {
	t.Helper()
	_, err := f.m.Archive("x")
	require.NoError(t, err)
	return "/api/v1/archived/" + f.ri.UID() + "/restore"
}

func TestRestore_RequiresCredentialChoiceAPI(t *testing.T) {
	f := newLinkedFixture(t)
	path := f.archivedX(t)
	rec := f.admin(http.MethodPost, path, `{}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "no_credential")
	rec = f.admin(http.MethodPost, path, `{"secret_id":"`+f.secretID+`","no_credential":true}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Nil(t, f.m.Get("x"), "still archived")
}

func TestRestore_WithSecretID_NonAdminRefused(t *testing.T) {
	f := newLinkedFixture(t)
	path := f.archivedX(t)
	op := f.mintTokenAt(t, "/api/v1", "api-operator", auth.Read, auth.Write, auth.Operator)
	rec := escDo(f.s.OAuthHandler(), op, http.MethodPost, path, `{"secret_id":"`+f.secretID+`"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Nil(t, f.m.Get("x"), "still archived")

	// The same with no_credential needs no admin.
	rec = escDo(f.s.OAuthHandler(), op, http.MethodPost, path, `{"no_credential":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.Equal(t, "none", o.AuthMethod)
}

func TestRestore_WithSecretIDAPI(t *testing.T) {
	f := newLinkedFixture(t)
	path := f.archivedX(t)
	rec := f.admin(http.MethodPost, path, `{"secret_id":"`+f.secretID+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	f.ri = f.m.Get("x")
	require.NotNil(t, f.ri)
	f.requireLinked(t, f.url)
}

func TestRestore_WithPastedCredentialAPI(t *testing.T) {
	f := newLinkedFixture(t)
	path := f.archivedX(t)
	// A pasted credential needs operator: a read+write token is refused.
	writer := f.mintTokenAt(t, "/api/v1", "api-writer", auth.Read, auth.Write)
	rec := escDo(f.s.OAuthHandler(), writer, http.MethodPost, path, `{"auth_method":"token","token":"`+linkedSecretValue+`"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	op := f.mintTokenAt(t, "/api/v1", "api-operator", auth.Read, auth.Write, auth.Operator)
	rec = escDo(f.s.OAuthHandler(), op, http.MethodPost, path, `{"auth_method":"token","token":"`+linkedSecretValue+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.Equal(t, linkedSecretValue, o.AuthToken)
	require.Empty(t, o.SecretID)
}
