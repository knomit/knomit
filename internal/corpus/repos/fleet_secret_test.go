package repos

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos/control"
)

// The HTTPS forge cases of fleet create, join, move and Update access with a
// stored secret are in cmd/fleet_secret_test.go (they swap go-git's https
// transport, which only a package with no parallel tests may do).

// A fleet create with secret_id and a pasted token is 400; a secret whose
// hosts list does not allow the forge's host is 422; both before anything
// is created or dialled.
func TestFleetCreate_SecretWithTokenRefused(t *testing.T) {
	dir := t.TempDir()
	m := newFleetHost(t, dir, nil)
	url := "https://127.0.0.1:1/fleet.git"
	meta, err := m.Secrets().Create(control.SecretInput{Name: "forge-pat", Type: control.SecretTypeToken,
		Hosts: []string{"127.0.0.1"}, Value: control.SecretValue{Token: "t"}}, time.Now())
	require.NoError(t, err)
	_, err = m.CreateFleet(context.Background(), FleetCreateSpec{Origin: &OriginSpec{URL: url, SecretID: meta.ID, AuthToken: "x"}})
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "auth_invalid", code)

	other, err := m.Secrets().Create(control.SecretInput{Name: "other-host", Type: control.SecretTypeToken,
		Hosts: []string{"github.com"}, Value: control.SecretValue{Token: "t"}}, time.Now())
	require.NoError(t, err)
	_, err = m.CreateFleet(context.Background(), FleetCreateSpec{Origin: &OriginSpec{URL: url, SecretID: other.ID}})
	status, code = fleetCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "auth_invalid", code)
	require.Contains(t, err.Error(), "other-host")
	require.Nil(t, m.fleetRepo(), "nothing was created")
}
