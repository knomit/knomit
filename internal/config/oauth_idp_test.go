package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const secretBytes = "s3cr3t-provider-client-secret"

// idpHome writes knomit.toml with an [oauth] section plus the given
// [oauth.idp] body and points KNOMIT_HOME at it.
func idpHome(t *testing.T, idp string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	t.Setenv("KNOMIT_OAUTH_IDP_CLIENT_SECRET", "")
	toml := "[oauth]\nissuer = \"https://knomit.example.com\"\naddr = \"127.0.0.1:19280\"\n\n" + idp
	if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func writeSecret(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, "github.secret")
	if err := os.WriteFile(p, []byte(secretBytes+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask may have narrowed it
		t.Fatal(err)
	}
	return p
}

func idpSection(secretFile string, subjects string) string {
	s := "[oauth.idp]\nprovider = \"github\"\nclient_id = \"Iv1.abc\"\nallowed_subjects = [" + subjects + "]\n"
	if secretFile != "" {
		s += fmt.Sprintf("client_secret_file = %q\n", secretFile)
	}
	return s
}

func TestLoad_OAuthIDPOffByDefault(t *testing.T) {
	idpHome(t, "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OAuth.IDP != nil {
		t.Fatalf("no [oauth.idp] must mean no provider: %+v", cfg.OAuth.IDP)
	}
}

// The secret comes from a file that only its owner can read, or from the
// fixed env var, which wins (as every other secret's env var does). The
// trailing newline an editor adds is not part of it.
func TestLoad_OAuthIDPSecretFromFileOrEnv(t *testing.T) {
	home := idpHome(t, "")
	f := writeSecret(t, home, 0o600)
	idpHome2 := func() { // rewrite the toml in the same home
		toml := "[oauth]\nissuer = \"https://knomit.example.com\"\naddr = \"127.0.0.1:19280\"\n\n" + idpSection(f, `"github-583231", "github-9 read"`)
		if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	idpHome2()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	idp := cfg.OAuth.IDP
	if idp == nil || idp.Provider != "github" || idp.ClientID != "Iv1.abc" || idp.Secret.Reveal() != secretBytes {
		t.Fatalf("idp from file: %+v / secret %q", idp, idp.Secret.Reveal())
	}
	got := idp.Allowed()
	if len(got) != 2 || got[0].Subject != "github-583231" || got[0].Scopes != nil ||
		got[1].Subject != "github-9" || strings.Join(got[1].Scopes, " ") != "read" {
		t.Fatalf("allowed subjects parsed as %+v", got)
	}

	t.Setenv("KNOMIT_OAUTH_IDP_CLIENT_SECRET", "from-env")
	if cfg, err = Load(); err != nil || cfg.OAuth.IDP.Secret.Reveal() != "from-env" {
		t.Fatalf("env must win: %v %q", err, cfg.OAuth.IDP.Secret.Reveal())
	}
}

func TestLoad_OAuthIDPSecretFromEnvAlone(t *testing.T) {
	idpHome(t, idpSection("", `"github-1"`))
	t.Setenv("KNOMIT_OAUTH_IDP_CLIENT_SECRET", "from-env")
	cfg, err := Load()
	if err != nil || cfg.OAuth.IDP.Secret.Reveal() != "from-env" {
		t.Fatalf("env alone: %v", err)
	}
}

// Never plaintext in TOML (master ruling S1): a client_secret key is refused
// with a message naming both accepted forms, not warned about and ignored.
func TestLoad_OAuthIDPPlaintextSecretRefused(t *testing.T) {
	idpHome(t, idpSection("", `"github-1"`)+"client_secret = \""+secretBytes+"\"\n")
	t.Setenv("KNOMIT_OAUTH_IDP_CLIENT_SECRET", "from-env") // even with a valid source beside it
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "client_secret_file") || !strings.Contains(err.Error(), "KNOMIT_OAUTH_IDP_CLIENT_SECRET") {
		t.Fatalf("plaintext client_secret: %v", err)
	}
	if strings.Contains(err.Error(), secretBytes) {
		t.Fatalf("the refusal echoes the secret: %v", err)
	}
}

func TestLoad_OAuthIDPSecretFileMustBeOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits; Windows has no equivalent check (documented)")
	}
	home := t.TempDir()
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		f := writeSecret(t, home, mode)
		idpHome(t, idpSection(f, `"github-1"`))
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Errorf("mode %o: %v; want a refusal naming 0600", mode, err)
		}
	}
	f := writeSecret(t, home, 0o400)
	idpHome(t, idpSection(f, `"github-1"`))
	if _, err := Load(); err != nil {
		t.Fatalf("0400 is stricter than 0600 and must load: %v", err)
	}
}

func TestLoad_OAuthIDPNoSecretRefused(t *testing.T) {
	idpHome(t, idpSection("", `"github-1"`))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "client_secret_file") {
		t.Fatalf("no secret source: %v", err)
	}
	idpHome(t, idpSection("/nonexistent/secret", `"github-1"`))
	if _, err := Load(); err == nil {
		t.Fatal("a missing secret file must fail the load")
	}
}

// Every rule of the allow list, each row naming what it guards. Ids only
// (R4): a login is re-claimable by someone else after a rename, so the
// @login form is refused with a pointer to the resolve helper.
func TestValidate_OAuthIDP(t *testing.T) {
	base := func() Config {
		c := Defaults()
		c.OAuth.Issuer, c.OAuth.Addr = "https://knomit.example.com", "127.0.0.1:19280"
		c.OAuth.IDP = &OAuthIDPConfig{Provider: "github", ClientID: "Iv1.abc", AllowedSubjects: []string{"github-1"}, Secret: NewIDPSecret("x")}
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("base must validate: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"oauth off", func(c *Config) { c.OAuth.Issuer, c.OAuth.Addr = "", "" }, "[oauth]"},
		{"unknown provider", func(c *Config) { c.OAuth.IDP.Provider = "gitlab" }, "provider"},
		{"no client id", func(c *Config) { c.OAuth.IDP.ClientID = "" }, "client_id"},
		{"client id with a control char", func(c *Config) { c.OAuth.IDP.ClientID = "a\x1b[8m" }, "client_id"},
		{"no secret", func(c *Config) { c.OAuth.IDP.Secret = IDPSecret{} }, "secret"},
		{"empty allow list", func(c *Config) { c.OAuth.IDP.AllowedSubjects = nil }, "nobody"},
		{"login form", func(c *Config) { c.OAuth.IDP.AllowedSubjects = []string{"github-@octocat"} }, "knomit oauth idp resolve"},
		{"colon form", func(c *Config) { c.OAuth.IDP.AllowedSubjects = []string{"github:1"} }, "github-<numeric id>"},
		{"leading zero", func(c *Config) { c.OAuth.IDP.AllowedSubjects = []string{"github-01"} }, "github-<numeric id>"},
		{"other provider's prefix", func(c *Config) { c.OAuth.IDP.AllowedSubjects = []string{"gitlab-1"} }, "github-<numeric id>"},
		{"duplicate", func(c *Config) { c.OAuth.IDP.AllowedSubjects = []string{"github-1", "github-1 read"} }, "twice"},
		{"admin scope", func(c *Config) { c.OAuth.IDP.AllowedSubjects = []string{"github-1 admin"} }, "admin"},
		{"unknown scope", func(c *Config) { c.OAuth.IDP.AllowedSubjects = []string{"github-1 root"} }, "root"},
	} {
		c := base()
		tc.mut(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v; want an error mentioning %q", tc.name, err, tc.want)
		}
	}
}

// A second provider block is a TOML error (a redefined table), so "exactly
// one provider" holds by construction; this pins it.
func TestLoad_OAuthIDPSecondBlockRefused(t *testing.T) {
	idpHome(t, idpSection("", `"github-1"`)+"\n"+idpSection("", `"github-2"`))
	t.Setenv("KNOMIT_OAUTH_IDP_CLIENT_SECRET", "x")
	if _, err := Load(); err == nil {
		t.Fatal("two [oauth.idp] tables loaded")
	}
}

// The secret never leaves through a print or an encoding of the config.
func TestIDPSecret_NeverPrinted(t *testing.T) {
	c := OAuthIDPConfig{Provider: "github", ClientID: "Iv1.abc", Secret: NewIDPSecret(secretBytes)}
	// %d is the verb a String method does not cover: without Format it
	// prints the struct's fields. A variable keeps vet's printf check quiet.
	dVerb := "%d"
	cfg := Defaults()
	cfg.OAuth.IDP = &c
	j, _ := json.Marshal(cfg)
	for name, s := range map[string]string{
		"%v": fmt.Sprintf("%v", c), "%+v": fmt.Sprintf("%+v", c), "%#v": fmt.Sprintf("%#v", c),
		"%s": fmt.Sprintf("%s", c.Secret), "%q": fmt.Sprintf("%q", c.Secret), "%x": fmt.Sprintf("%x", c.Secret), "%d": fmt.Sprintf(dVerb, c.Secret), "%d cfg": fmt.Sprintf(dVerb, c),
		"config %+v": fmt.Sprintf("%+v", *cfg.OAuth.IDP), "json": string(j),
	} {
		if strings.Contains(s, secretBytes) || strings.Contains(s, fmt.Sprintf("%x", secretBytes)) {
			t.Errorf("%s leaks the secret: %s", name, s)
		}
	}
	if c.Secret.Reveal() != secretBytes {
		t.Fatal("Reveal must return the secret")
	}
}
