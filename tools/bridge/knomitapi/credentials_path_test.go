package knomitapi

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
)

func isolateHomeEnv(t *testing.T, home string) string {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "KNOMIT_") {
			t.Setenv(k, "")
		}
	}
	osHome := t.TempDir()
	t.Setenv("HOME", osHome)
	t.Setenv("USERPROFILE", osHome)
	t.Setenv("KNOMIT_HOME", home)
	return osHome
}

// Credentials live under the same data root the server uses: a "~/"
// KNOMIT_HOME is expanded once, by config.ResolveHome, for every caller.
func TestCredentialsPath_UsesTheResolvedHome(t *testing.T) {
	osHome := isolateHomeEnv(t, "~/kh")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	u, _ := url.Parse("https://example.com")
	got, err := CredentialsPath(u)
	if err != nil {
		t.Fatalf("CredentialsPath: %v", err)
	}
	want := filepath.Join(osHome, "kh", "credentials", "example.com_443")
	if got != want {
		t.Fatalf("CredentialsPath = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, cfg.Home+string(filepath.Separator)) {
		t.Fatalf("CredentialsPath = %q is not under Load().Home = %q", got, cfg.Home)
	}
}

// A relative KNOMIT_HOME is refused here exactly as the server refuses it,
// rather than filing credentials under whatever directory `kb` ran from.
func TestCredentialsPath_RelativeHomeRefused(t *testing.T) {
	isolateHomeEnv(t, filepath.Join("rel", "kh"))
	_, loadErr := config.Load()
	u, _ := url.Parse("https://example.com")
	got, err := CredentialsPath(u)
	if err == nil || loadErr == nil {
		t.Fatalf("CredentialsPath = %q, err = %v, Load err = %v; want both to refuse a relative KNOMIT_HOME",
			got, err, loadErr)
	}
	if !strings.Contains(loadErr.Error(), err.Error()) {
		t.Fatalf("Load's error %q does not carry CredentialsPath's %q", loadErr, err)
	}
}
