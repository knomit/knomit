package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The issuer is the audience root, the `iss` and the base of every URL in the
// metadata documents, so what it may be is decided once, here. Each row names
// the production change that would flip it: dropping the scheme check lets
// "ftp" and relative URLs through; dropping the loopback-only rule for http
// lets a plaintext issuer reach the network.
func TestNormalizeIssuer(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"https://knomit.example.com", "https://knomit.example.com", true},
		{"https://knomit.example.com/", "https://knomit.example.com", true}, // trailing slash normalised
		{"https://Knomit.Example.COM:443", "https://knomit.example.com:443", true},
		{"https://host.example/knomit/", "https://host.example/knomit", true},
		{"http://localhost:8080", "http://localhost:8080", true},
		{"http://127.0.0.1:19280", "http://127.0.0.1:19280", true},
		{"http://[::1]:19280/", "http://[::1]:19280", true},
		{"http://knomit.example.com", "", false}, // plaintext off loopback
		{"http://192.168.1.5:19280", "", false},  // plaintext on a LAN address
		{"http://localhost.example.com", "", false},
		{"/relative", "", false},
		{"knomit.example.com", "", false}, // no scheme
		{"ftp://knomit.example.com", "", false},
		{"https://knomit.example.com?x=1", "", false}, // query
		{"https://knomit.example.com#frag", "", false},
		{"https://user:pw@knomit.example.com", "", false}, // userinfo
		{"https://", "", false},                           // no host
		{"https://host.example/a/../b", "", false},        // non-canonical path
	} {
		got, err := NormalizeIssuer(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Errorf("NormalizeIssuer(%q) = %q, %v; want %q, nil", tc.in, got, err, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("NormalizeIssuer(%q) = %q, nil; want an error", tc.in, got)
		}
	}
}

// Issuer and addr are one switch: an issuer with no listener advertises
// endpoints nothing serves, and a listener with no issuer has no audience.
func TestValidate_OAuthIssuerAndAddrTogether(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults (oauth off) must validate: %v", err)
	}
	cfg.OAuth.Issuer = "https://knomit.example.com"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "oauth") {
		t.Fatalf("issuer without addr: want an [oauth] error, got %v", err)
	}
	cfg = Defaults()
	cfg.OAuth.Addr = "127.0.0.1:19280"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "oauth") {
		t.Fatalf("addr without issuer: want an [oauth] error, got %v", err)
	}
	cfg.OAuth.Issuer = "https://knomit.example.com"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("both set: %v", err)
	}
	cfg.OAuth.Issuer = "http://knomit.example.com"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate must apply the issuer rules too, not only Load")
	}
}

func TestValidate_OAuthTTLsPositive(t *testing.T) {
	for _, mut := range []func(*OAuthConfig){
		func(o *OAuthConfig) { o.AccessTTL = 0 },
		func(o *OAuthConfig) { o.RefreshTTL = -time.Hour },
	} {
		cfg := Defaults()
		cfg.OAuth.Issuer, cfg.OAuth.Addr = "https://knomit.example.com", "127.0.0.1:19280"
		mut(&cfg.OAuth)
		if err := cfg.Validate(); err == nil {
			t.Errorf("non-positive TTL accepted: %+v", cfg.OAuth)
		}
	}
}

func TestValidate_OAuthClients(t *testing.T) {
	for name, c := range map[string]OAuthClient{
		"empty id":         {ID: "", RedirectURIs: []string{"https://app.example/cb"}},
		"no redirect uris": {ID: "app"},
		"relative uri":     {ID: "app", RedirectURIs: []string{"/cb"}},
		"fragment in uri":  {ID: "app", RedirectURIs: []string{"https://app.example/cb#x"}},
	} {
		cfg := Defaults()
		cfg.OAuth.Issuer, cfg.OAuth.Addr = "https://knomit.example.com", "127.0.0.1:19280"
		cfg.OAuth.Clients = []OAuthClient{c}
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cfg := Defaults()
	cfg.OAuth.Issuer, cfg.OAuth.Addr = "https://knomit.example.com", "127.0.0.1:19280"
	cfg.OAuth.Clients = []OAuthClient{
		{ID: "app", RedirectURIs: []string{"https://app.example/cb"}},
		{ID: "app", RedirectURIs: []string{"https://app.example/cb2"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("duplicate client id accepted")
	}
}

func TestDefaults_OAuthOffWithPolicyTTLs(t *testing.T) {
	o := Defaults().OAuth
	if o.Issuer != "" || o.Addr != "" {
		t.Fatalf("[oauth] must default OFF, got %+v", o)
	}
	if o.AccessTTL != 2*time.Hour || o.RefreshTTL != 14*24*time.Hour {
		t.Fatalf("TTL defaults = %v / %v, want 2h / 336h", o.AccessTTL, o.RefreshTTL)
	}
	if !o.DefaultClient {
		t.Fatal("default_client must default true")
	}
}

// The built-in `kb` client: present by default, removable with
// default_client = false, and REPLACED (not duplicated) by a configured
// client of the same id.
func TestEffectiveClients(t *testing.T) {
	o := Defaults().OAuth
	got := o.EffectiveClients()
	if len(got) != 1 || got[0].ID != KBClientID {
		t.Fatalf("default: want only the built-in kb, got %+v", got)
	}
	for _, u := range got[0].RedirectURIs {
		if !strings.HasPrefix(u, "http://127.0.0.1/") && !strings.HasPrefix(u, "http://[::1]/") {
			t.Fatalf("built-in kb redirect %q is not a loopback IP literal", u)
		}
	}

	o.DefaultClient = false
	if got := o.EffectiveClients(); len(got) != 0 {
		t.Fatalf("default_client=false: want none, got %+v", got)
	}

	o = Defaults().OAuth
	mine := OAuthClient{ID: KBClientID, Name: "mine", RedirectURIs: []string{"http://127.0.0.1/other"}}
	o.Clients = []OAuthClient{mine, {ID: "app", RedirectURIs: []string{"https://app.example/cb"}}}
	got = o.EffectiveClients()
	if len(got) != 2 {
		t.Fatalf("configured kb must replace the built-in, got %+v", got)
	}
	for _, c := range got {
		if c.ID == KBClientID && c.Name != "mine" {
			t.Fatalf("built-in kb won over the configured one: %+v", c)
		}
	}
}

func TestLoad_OAuthFromTOMLAndEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	toml := `
[oauth]
issuer = "https://knomit.example.com/"
addr = "127.0.0.1:19280"
access_ttl = "30m"
refresh_ttl = "72h"

[[oauth.client]]
id = "app"
name = "App"
redirect_uris = ["https://app.example/cb"]
`
	if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OAuth.Issuer != "https://knomit.example.com" {
		t.Fatalf("issuer not normalised at Load: %q", cfg.OAuth.Issuer)
	}
	if cfg.OAuth.AccessTTL != 30*time.Minute || cfg.OAuth.RefreshTTL != 72*time.Hour {
		t.Fatalf("TTLs from TOML: %v / %v", cfg.OAuth.AccessTTL, cfg.OAuth.RefreshTTL)
	}
	if len(cfg.OAuth.Clients) != 1 || cfg.OAuth.Clients[0].ID != "app" {
		t.Fatalf("clients from TOML: %+v", cfg.OAuth.Clients)
	}

	t.Setenv("KNOMIT_OAUTH_ISSUER", "http://localhost:19280")
	t.Setenv("KNOMIT_OAUTH_ADDR", "127.0.0.1:19281")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.OAuth.Issuer != "http://localhost:19280" || cfg.OAuth.Addr != "127.0.0.1:19281" {
		t.Fatalf("env overrides: %+v", cfg.OAuth)
	}

	t.Setenv("KNOMIT_OAUTH_ISSUER", "http://knomit.example.com")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a plaintext non-loopback issuer from env")
	}
}
