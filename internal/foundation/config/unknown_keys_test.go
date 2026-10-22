package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// captureLogs swaps the global logger for a buffer for the duration of a test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = orig })
	return &buf
}

// writeConfig plants a knomit.toml in a temp home and points Load at it.
func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "knomit.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KNOMIT_HOME", dir)
}

// TestLoad_WarnsOnUnknownKey pins the one signal a dropped key gets. The decoder
// ignores anything it cannot map, so a typo looks exactly like a setting that
// does not work — as does a key knomit used to read and no longer does.
func TestLoad_WarnsOnUnknownKey(t *testing.T) {
	buf := captureLogs(t)
	writeConfig(t, "[log]\nlevle = \"debug\"\n")

	if _, err := Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "levle") {
		t.Errorf("an unknown key must be named in the warning; got %q", out)
	}
}

// TestLoad_WarnsOnRetiredGitOrigin is the same path, exercised through the key
// this actually removed: git.origin is gone, and a leftover one must not decode
// into silence while the repo keeps syncing against its own remotes row.
func TestLoad_WarnsOnRetiredGitOrigin(t *testing.T) {
	buf := captureLogs(t)
	writeConfig(t, "[git]\norigin = \"https://example.com/kb.git\"\n")

	if _, err := Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "git.origin") {
		t.Errorf("a retired key must be named in the warning; got %q", out)
	}
}

// TestLoad_QuietOnCleanConfig keeps the warning worth reading: a config that
// decodes fully must produce none.
func TestLoad_QuietOnCleanConfig(t *testing.T) {
	buf := captureLogs(t)
	writeConfig(t, "[log]\nlevel = \"debug\"\n")

	if _, err := Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if out := buf.String(); out != "" {
		t.Errorf("a fully-decoded config must log nothing; got %q", out)
	}
}
