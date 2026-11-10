package app

import (
	"context"
	"testing"
	"time"

	"knomit/internal/edge/oauth"
	"knomit/internal/foundation/config"
	"knomit/test/testenv"
)

// TestApp_CredentialStoresAlwaysBuilt (unified auth P3): the credential store,
// the issuer core and the issuer set exist on every boot, with or without
// [oauth]; the OAuth listener and its approval routes still exist only when
// [oauth] is configured, and then share the one issuer. The OAuth token
// lifetimes stay [oauth]'s; the credential lifetimes are [auth]'s.
func TestApp_CredentialStoresAlwaysBuilt(t *testing.T) {
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	cfg.Host, cfg.Port = "127.0.0.1", "19278"
	cfg.Auth.LinkTTL = 45 * time.Second
	a, err := New(context.Background(), cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	s := a.Server()
	if s.AuthStore == nil || s.AuthIssuer == nil || s.AuthzStore == nil {
		a.Close()
		t.Fatalf("credential stores not built: %+v %+v %+v", s.AuthStore, s.AuthIssuer, s.AuthzStore)
	}
	if s.OAuthIssuer != nil || a.OAuthHandler() != nil {
		a.Close()
		t.Fatal("the OAuth listener's issuer exists with [oauth] unset")
	}
	if len(s.Issuers) != 1 || s.Issuers[0].String() != "http://localhost:19278" {
		a.Close()
		t.Fatalf("issuers = %v, want the loopback-derived default", s.Issuers)
	}
	lt := s.AuthStore.Lifetimes()
	want := oauth.Lifetimes{
		Access: cfg.OAuth.AccessTTL, RefreshIdle: cfg.OAuth.RefreshTTL, RefreshAbsolute: cfg.OAuth.RefreshTTL,
		Pending: cfg.Auth.PendingTTL, Link: 45 * time.Second,
		SessionIdle: cfg.Auth.SessionIdleTTL, SessionAbsolute: cfg.Auth.SessionAbsoluteTTL,
		PATDefault: cfg.Auth.PATDefaultTTL, PATMax: cfg.Auth.PATMaxTTL,
	}
	if lt != want {
		a.Close()
		t.Fatalf("lifetimes = %+v, want %+v", lt, want)
	}
	a.Close()

	cfg = config.Defaults()
	cfg.Home = t.TempDir()
	cfg.OAuth.Issuer, cfg.OAuth.Addr = "http://127.0.0.1:19280", "127.0.0.1:0"
	a, err = New(context.Background(), cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("boot with [oauth]: %v", err)
	}
	defer a.Close()
	if a.Server().OAuthIssuer == nil || a.Server().OAuthIssuer != a.Server().AuthIssuer {
		t.Fatal("with [oauth] the listener's issuer must be the issuer core")
	}
}
