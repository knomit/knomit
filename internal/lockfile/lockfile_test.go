package lockfile_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"knomit/internal/lockfile"
)

func TestV2FieldsRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	in := lockfile.Info{PID: 7, Port: 0, Version: "1.2.3", Schema: lockfile.SchemaV2, Home: "/h",
		HTTP: "", Address: "unix:///h/knomit.sock", Socket: "/h/knomit.sock", TLS: "127.0.0.1:9", Generation: 1}
	if err := lockfile.Write(path, in); err != nil {
		t.Fatal(err)
	}
	got, err := lockfile.Read(path)
	if err != nil || got != in {
		t.Fatalf("Read = %+v, %v; want %+v", got, err, in)
	}
	raw, _ := os.ReadFile(path)
	// http is always present, so an off listener reads as "", not as a v1 file.
	if !strings.Contains(string(raw), `"http": ""`) || !strings.Contains(string(raw), `"port": 0`) {
		t.Fatalf("file lacks explicit http/port for an off listener:\n%s", raw)
	}
}

func TestRemoveIfOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	if err := lockfile.Write(path, lockfile.Info{PID: 10}); err != nil {
		t.Fatal(err)
	}
	if err := lockfile.RemoveIfOwner(path, 11); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a non-owner removed the file: %v", err)
	}
	if err := lockfile.RemoveIfOwner(path, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the owner left the file: %v", err)
	}
	if err := lockfile.RemoveIfOwner(path, 10); err != nil {
		t.Fatalf("missing file: %v", err)
	}
}

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
