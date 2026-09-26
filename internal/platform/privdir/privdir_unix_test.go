//go:build !windows

package privdir

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// captureLog points the global logger at a buffer for the test's duration.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })
	return &buf
}

func TestEnsure_CreatesWith0700(t *testing.T) {
	logs := captureLog(t)
	home := filepath.Join(t.TempDir(), "a", "home")

	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("created %v, want a directory with mode 0700", fi.Mode())
	}
	if logs.Len() != 0 {
		t.Errorf("a fresh private directory logged: %s", logs)
	}
}

// An existing wider directory is reported and NOT chmodded: its mode is the
// user's choice.
func TestEnsure_ExistingWideDirIsWarnedNotChanged(t *testing.T) {
	logs := captureLog(t)
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// Chmod, not Mkdir's mode: the umask would otherwise decide it.
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("Ensure changed an existing directory's mode to %v, want 0755 left alone", fi.Mode().Perm())
	}
	if !strings.Contains(logs.String(), `"level":"warn"`) || !strings.Contains(logs.String(), home) {
		t.Errorf("no warning naming %s: %q", home, logs)
	}
}

func TestEnsure_ExistingPrivateDirIsQuiet(t *testing.T) {
	logs := captureLog(t)
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Errorf("a private directory logged: %s", logs)
	}
}
