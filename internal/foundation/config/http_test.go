package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/foundation/config"
)

// `http` switches the plaintext TCP listener. It defaults to ON so `knomit
// serve`, Docker and the e2e suite are unchanged; the desktop writes false
// on fresh installs. Each case starts from the opposite of what it expects,
// so a Load that ignored the layer fails the row.
func TestLoad_HTTPKey(t *testing.T) {
	cases := []struct {
		name string
		toml string
		env  string
		want bool
	}{
		{"default is on", "", "", true},
		{"toml off", "http = false\n", "", false},
		{"toml on", "http = true\n", "", true},
		{"env off beats toml on", "http = true\n", "false", false},
		{"env on beats toml off", "http = false\n", "1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			isolateSocketEnv(t, home)
			if c.toml != "" {
				if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(c.toml), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if c.env != "" {
				t.Setenv("KNOMIT_HTTP", c.env)
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.HTTP != c.want {
				t.Fatalf("cfg.HTTP = %v, want %v", cfg.HTTP, c.want)
			}
		})
	}
}

func TestLoad_HTTPEnvMustBeABoolean(t *testing.T) {
	isolateSocketEnv(t, t.TempDir())
	t.Setenv("KNOMIT_HTTP", "maybe")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "KNOMIT_HTTP") {
		t.Fatalf("Load = %v, want an error naming KNOMIT_HTTP", err)
	}
}

// With http off the local socket/pipe is the only way in. A config that turns
// both off serves nothing, so it is refused at load rather than booting a
// healthy-looking process nobody can reach.
func TestValidate_NoHTTPNeedsALocalSocket(t *testing.T) {
	cfg := config.Defaults()
	cfg.HTTP = false
	cfg.Socket = ""
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "http") {
		t.Fatalf("Validate with http off and no socket = %v, want an error naming http", err)
	}
	cfg.Socket = filepath.Join(t.TempDir(), "k.sock")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with http off and a socket: %v", err)
	}
	cfg.HTTP, cfg.Socket = true, ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with http on and no socket (tests build such configs): %v", err)
	}
}

// The discovery file is keyed by home: the default home's lives in the
// per-user state directory (where every older kb and desktop looks), any
// other home's lives in that home, so a rehearsal or e2e server cannot
// overwrite the real one's file and kb under KNOMIT_HOME reads its own.
func TestLockfilePath_KeyedByHome(t *testing.T) {
	isolateSocketEnv(t, "")
	t.Setenv("KNOMIT_HOME", "")
	stateRoot := t.TempDir()
	t.Setenv("HOME", stateRoot)
	t.Setenv("XDG_STATE_HOME", filepath.Join(stateRoot, "state"))
	t.Setenv("LOCALAPPDATA", filepath.Join(stateRoot, "local"))

	def, err := config.DefaultHome()
	if err != nil {
		t.Fatal(err)
	}
	state, err := config.StateDir()
	if err != nil {
		t.Fatal(err)
	}

	// Unset or explicitly the default home: the state directory's file.
	for _, h := range []string{"", def} {
		t.Setenv("KNOMIT_HOME", h)
		got, err := config.LockfilePath()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(state, "server.json"); got != want {
			t.Errorf("KNOMIT_HOME=%q: LockfilePath = %q, want %q", h, got, want)
		}
	}

	other := t.TempDir()
	t.Setenv("KNOMIT_HOME", other)
	got, err := config.LockfilePath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(other, "server.json"); got != want {
		t.Errorf("non-default home: LockfilePath = %q, want %q", got, want)
	}
	if byHome := config.LockfilePathFor(other); byHome != got {
		t.Errorf("LockfilePathFor(%q) = %q, but LockfilePath = %q", other, byHome, got)
	}
	if byDefault := config.LockfilePathFor(def); byDefault != filepath.Join(state, "server.json") {
		t.Errorf("LockfilePathFor(default home) = %q, want the state directory's file", byDefault)
	}
}
