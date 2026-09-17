package lockfile_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"knomit/tools/desktop/internal/lockfile"
)

func TestWriteRead_Roundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "server.json")
	in := lockfile.Info{PID: 1234, Port: 52341, Version: "0.1.0"}

	if err := lockfile.Write(path, in); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// Windows has no Unix mode bits. Go synthesises Perm() from the read-only
	// attribute alone, so a writable file always reports 0666 and 0600 is not
	// a mode any file here can have. Restricting this file to its owner on
	// Windows means an ACL, which nothing in the tree sets; the rest of the
	// round trip is still worth asserting.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("perms = %o, want 0600", info.Mode().Perm())
	}

	got, err := lockfile.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != in {
		t.Errorf("Read = %+v, want %+v", got, in)
	}
}

func TestRead_MissingReturnsErrNotExist(t *testing.T) {
	_, err := lockfile.Read(filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read missing = %v, want os.ErrNotExist", err)
	}
}

func TestRemove_NoFileIsNoError(t *testing.T) {
	if err := lockfile.Remove(filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Errorf("Remove non-existent: %v", err)
	}
}
