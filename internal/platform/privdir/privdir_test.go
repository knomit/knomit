package privdir

import (
	"bytes"
	"os"
	"path/filepath"
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

// The one case that IS an error on every OS: the directory cannot be
// created, here because its parent is a regular file.
func TestEnsure_UncreatableIsAnError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(filepath.Join(file, "home")); err == nil {
		t.Fatal("Ensure under a regular file returned nil")
	}
}
