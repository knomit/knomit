package config

import (
	"strings"
	"testing"
	"time"
)

// TestDefaults_AuthLifetimes pins the lifetimes of the unified auth design
// (§7, plan Global Constraints): pending 10 min; login link 2 min; session
// 30 d idle / 90 d absolute; access token 2 h; refresh 30 d idle / 90 d
// absolute; PAT default 90 d, max 1 y.
func TestDefaults_AuthLifetimes(t *testing.T) {
	const day = 24 * time.Hour
	a := Defaults().Auth
	for _, tc := range []struct {
		name      string
		got, want time.Duration
	}{
		{"pending_ttl", a.PendingTTL, 10 * time.Minute},
		{"link_ttl", a.LinkTTL, 2 * time.Minute},
		{"session_idle_ttl", a.SessionIdleTTL, 30 * day},
		{"session_absolute_ttl", a.SessionAbsoluteTTL, 90 * day},
		{"access_ttl", a.AccessTTL, 2 * time.Hour},
		{"refresh_idle_ttl", a.RefreshIdleTTL, 30 * day},
		{"refresh_absolute_ttl", a.RefreshAbsoluteTTL, 90 * day},
		{"pat_default_ttl", a.PATDefaultTTL, 90 * day},
		{"pat_max_ttl", a.PATMaxTTL, 365 * day},
	} {
		if tc.got != tc.want {
			t.Errorf("[auth].%s default = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestLoad_AuthLifetimesFromTOML(t *testing.T) {
	writeConfig(t, "[auth]\nlink_ttl = \"30s\"\nsession_idle_ttl = \"1h\"\npat_max_ttl = \"4320h\"\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.LinkTTL != 30*time.Second || c.Auth.SessionIdleTTL != time.Hour || c.Auth.PATMaxTTL != 4320*time.Hour {
		t.Fatalf("lifetimes not read: %+v", c.Auth)
	}
	if c.Auth.PendingTTL != 10*time.Minute {
		t.Fatalf("an unset lifetime lost its default: %v", c.Auth.PendingTTL)
	}
}

func TestValidate_AuthLifetimes(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*AuthConfig)
		want string
	}{
		{"zero link", func(a *AuthConfig) { a.LinkTTL = 0 }, "link_ttl"},
		{"negative access", func(a *AuthConfig) { a.AccessTTL = -time.Second }, "access_ttl"},
		{"session idle beyond absolute", func(a *AuthConfig) { a.SessionIdleTTL = a.SessionAbsoluteTTL + time.Hour }, "session_idle_ttl"},
		{"refresh idle beyond absolute", func(a *AuthConfig) { a.RefreshIdleTTL = a.RefreshAbsoluteTTL + time.Hour }, "refresh_idle_ttl"},
		{"pat default beyond max", func(a *AuthConfig) { a.PATDefaultTTL = a.PATMaxTTL + time.Hour }, "pat_default_ttl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.Home, c.Socket = t.TempDir(), "/tmp/x.sock"
			tc.mut(&c.Auth)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want an error naming %s", err, tc.want)
			}
		})
	}
	c := Defaults()
	c.Home, c.Socket = t.TempDir(), "/tmp/x.sock"
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults refused: %v", err)
	}
}
