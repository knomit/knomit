package web

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos/control"
)

// listedCredential reads repo x's credential fields from GET /repos.
func listedCredential(t *testing.T, f *linkedFixture) (status, secret string, present bool) {
	t.Helper()
	rec := f.admin(http.MethodGet, "/api/v1/repos", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Embedded struct {
			Repos []map[string]any `json:"repos"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	for _, r := range body.Embedded.Repos {
		if r["name"] != "x" {
			continue
		}
		s, ok := r["credential_status"].(string)
		n, _ := r["credential_secret"].(string)
		return s, n, ok
	}
	t.Fatal("repo x not listed")
	return "", "", false
}

// The repo list carries a linked secret's expiry only when it needs the
// reader's attention: expiring or expired, with the secret's name. ok and
// no expiry omit both fields.
func TestRepoView_CredentialStatus(t *testing.T) {
	f := newLinkedFixture(t)
	setExpiry := func(at *time.Time) {
		require.NoError(t, f.m.Secrets().SetExpiry(f.secretID, at, time.Now()))
	}

	_, _, present := listedCredential(t, f)
	require.False(t, present, "no expiry: nothing to flag")

	far := time.Now().Add(60 * 24 * time.Hour)
	setExpiry(&far)
	_, _, present = listedCredential(t, f)
	require.False(t, present, "ok: nothing to flag")

	soon := time.Now().Add(3 * 24 * time.Hour)
	setExpiry(&soon)
	status, name, present := listedCredential(t, f)
	require.True(t, present)
	require.Equal(t, control.ExpiryExpiring, status)
	require.Equal(t, "gh-pat", name)

	past := time.Now().Add(-time.Hour)
	setExpiry(&past)
	status, name, _ = listedCredential(t, f)
	require.Equal(t, control.ExpiryExpired, status)
	require.Equal(t, "gh-pat", name)
	require.NotContains(t, f.admin(http.MethodGet, "/api/v1/repos", "").Body.String(), linkedSecretValue)
}
