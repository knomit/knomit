package web

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fleet routes take secret_id as an alternative to a pasted credential,
// never both: 400 auth_invalid before anything runs. (The forge cases with
// a stored secret are in cmd/fleet_secret_test.go.)
func TestFleetAPI_SecretWithPastedCredentialRefused(t *testing.T) {
	f := newSecretsFixture(t, true)
	for _, c := range []struct{ path, body string }{
		{"/api/v1/fleet/join", `{"bundle":"x","secret_id":"s","auth_token":"t"}`},
		{"/api/v1/fleet/join:check", `{"bundle":"x","secret_id":"s","auth_method":"token"}`},
		{"/api/v1/fleet/follow", `{"secret_id":"s","auth_method":"token","auth_token":"t"}`},
		{"/api/v1/fleet/create", `{"origin":{"url":"https://forge.example/fleet.git","secret_id":"s","auth_token":"t"}}`},
	} {
		rec := f.do(t, http.MethodPost, c.path, c.body)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", c.path, rec.Body.String())
		require.Contains(t, rec.Body.String(), "auth_invalid", c.path)
	}
}
