package config

import (
	"errors"
	"strings"
	"testing"
)

// TestLoad_RemovedKeysAreBootErrors pins that a key (or env var) knomit used
// to read and no longer does stops startup with a message naming the
// replacement, instead of being ignored with a warning.
func TestLoad_RemovedKeysAreBootErrors(t *testing.T) {
	for _, tc := range []struct{ name, toml, env, wantKey string }{
		{name: "idp table", toml: "[oauth.idp]\nprovider = \"github\"\n", wantKey: "[oauth.idp]"},
		{name: "idp nested keys", toml: "[oauth.idp]\nclient_id = \"x\"\nallowed_subjects = [\"github-1\"]\n", wantKey: "[oauth.idp]"},
		// A dotted key under [oauth] reports only "oauth.idp.client_id", never
		// "oauth.idp", so these reach the prefix match, not the exact one.
		{name: "idp dotted key under oauth", toml: "[oauth]\nidp.client_id = \"x\"\n", wantKey: "[oauth.idp]"},
		{name: "idp dotted key at top level", toml: "oauth.idp.client_id = \"x\"\n", wantKey: "[oauth.idp]"},
		{name: "idp secret env", toml: "", env: "KNOMIT_OAUTH_IDP_CLIENT_SECRET", wantKey: "KNOMIT_OAUTH_IDP_CLIENT_SECRET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfig(t, tc.toml)
			if tc.env != "" {
				t.Setenv(tc.env, "s3cret")
			}
			_, err := Load()
			var rk *RemovedKeyError
			if !errors.As(err, &rk) {
				t.Fatalf("Load error = %v, want *RemovedKeyError", err)
			}
			if rk.Key != tc.wantKey {
				t.Errorf("Key = %q, want %q", rk.Key, tc.wantKey)
			}
			if rk.Replacement == "" || !strings.Contains(err.Error(), "removed") || !strings.Contains(err.Error(), rk.Replacement) {
				t.Errorf("error %q must say removed and name the replacement", err)
			}
		})
	}
}

// A config without removed keys is not refused for that reason.
func TestLoad_NoRemovedKeysIsNotRefused(t *testing.T) {
	writeConfig(t, "[oauth]\naccess_ttl = \"1h\"\n")
	_, err := Load()
	var rk *RemovedKeyError
	if errors.As(err, &rk) {
		t.Fatalf("unexpected removed-key error: %v", err)
	}
}
