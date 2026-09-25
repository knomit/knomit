package knomitapi

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
)

// Credentials live under the same data root the server uses, whatever the
// working directory: a "~/" or relative KNOMIT_HOME is resolved once, by
// config.ResolveHome, for every caller.
func TestCredentialsPath_UsesTheResolvedHome(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		root        func(osHome, cwd string) string
	}{
		{"tilde", "~/kh", func(osHome, _ string) string { return filepath.Join(osHome, "kh") }},
		{"relative", filepath.Join("rel", "kh"), func(_, cwd string) string { return filepath.Join(cwd, "rel", "kh") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, kv := range os.Environ() {
				if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "KNOMIT_") {
					t.Setenv(k, "")
				}
			}
			osHome := t.TempDir()
			t.Setenv("HOME", osHome)
			t.Setenv("USERPROFILE", osHome)
			cwd := t.TempDir()
			t.Chdir(cwd)
			// t.Chdir may leave a symlinked temp dir unresolved; compare
			// against what the process itself sees as its cwd.
			if wd, err := os.Getwd(); err == nil {
				cwd = wd
			}
			t.Setenv("KNOMIT_HOME", tc.value)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			u, _ := url.Parse("https://example.com")
			got, err := CredentialsPath(u)
			if err != nil {
				t.Fatalf("CredentialsPath: %v", err)
			}
			want := filepath.Join(tc.root(osHome, cwd), "credentials", "example.com_443")
			if got != want {
				t.Fatalf("CredentialsPath = %q, want %q", got, want)
			}
			if !strings.HasPrefix(got, cfg.Home+string(filepath.Separator)) {
				t.Fatalf("CredentialsPath = %q is not under Load().Home = %q", got, cfg.Home)
			}
		})
	}
}
