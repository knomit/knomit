//go:build windows

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// On Windows the local listener is a named pipe, so an explicit socket must
// be one. A tilde is NOT expanded in it there (a `~\` path could only ever be
// a file, which auth.ListenLocal refuses), and both sides refuse any value
// that is not a pipe name, with the same error.
func TestSocketPath_WindowsSocketMustBePipe(t *testing.T) {
	for _, v := range []string{`~\x.sock`, "~/x.sock", `C:\abs\x.sock`, "rel.sock"} {
		t.Run(v, func(t *testing.T) {
			setOSHome(t)
			isolateSocketEnv(t, t.TempDir())
			t.Setenv("KNOMIT_SOCKET", v)
			_, loadErr := config.Load()
			got, pathErr := config.SocketPath()
			if loadErr == nil || pathErr == nil {
				t.Fatalf("Load err = %v, SocketPath() = %q, err = %v; want both to refuse %q",
					loadErr, got, pathErr, v)
			}
			if loadErr.Error() != pathErr.Error() {
				t.Fatalf("Load and SocketPath disagree:\n  Load:       %v\n  SocketPath: %v", loadErr, pathErr)
			}
			if !strings.Contains(pathErr.Error(), auth.PipePrefix) {
				t.Fatalf("error %q does not name the pipe form %s<name>", pathErr, auth.PipePrefix)
			}
		})
	}
}

func TestSocketPath_AgreesWithLoad_WindowsPipeSocket(t *testing.T) {
	isolateSocketEnv(t, t.TempDir())
	want := auth.PipePrefix + "knomit-operator-named"
	t.Setenv("KNOMIT_SOCKET", want)
	requireAgreement(t, want)
}

// `~\` still means the user's home directory in every other path field.
func TestLoad_BackslashTildeInPathField(t *testing.T) {
	osHome := setOSHome(t)
	home := t.TempDir()
	isolateSocketEnv(t, home)
	body := "[remote]\nknown_hosts = '~\\kh'\n"
	if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if want := filepath.Join(osHome, "kh"); cfg.Remote.KnownHosts != want {
		t.Fatalf("Remote.KnownHosts = %q, want %q", cfg.Remote.KnownHosts, want)
	}
}

// A "~/" on Windows joins onto the home directory with the platform
// separator, so it names the same file as the `~\` spelling.
func TestLoad_ForwardSlashTildeJoinsOnWindows(t *testing.T) {
	osHome := setOSHome(t)
	home := t.TempDir()
	isolateSocketEnv(t, home)
	body := "[remote]\nknown_hosts = '~/kh'\n"
	if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if want := filepath.Join(osHome, "kh"); cfg.Remote.KnownHosts != want {
		t.Fatalf("Remote.KnownHosts = %q, want %q", cfg.Remote.KnownHosts, want)
	}
}
