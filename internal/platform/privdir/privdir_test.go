package privdir

import (
	"os"
	"path/filepath"
	"testing"
)

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
